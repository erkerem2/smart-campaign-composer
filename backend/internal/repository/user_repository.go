package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"smart-campaign-composer/backend/internal/domain"
)

// UserRepository persists domain.User rows.
type UserRepository struct {
	db *sql.DB
}

// NewUserRepository returns a repository backed by db.
func NewUserRepository(db *sql.DB) *UserRepository {
	return &UserRepository{db: db}
}

const userColumns = `id, first_name, last_name, email, phone, city, age, total_spend,
	cart_last_active_at, last_purchase_at, opt_in_sms, opt_in_email, created_at`

// InsertBatch inserts users in a single transaction and writes the assigned
// ids back into the slice. Either every user is inserted or none is.
func (r *UserRepository) InsertBatch(ctx context.Context, users []domain.User) error {
	if len(users) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO users (first_name, last_name, email, phone, city, age, total_spend,
			cart_last_active_at, last_purchase_at, opt_in_sms, opt_in_email, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for i := range users {
		u := &users[i]
		res, err := stmt.ExecContext(ctx,
			u.FirstName, u.LastName, u.Email, u.Phone, u.City, u.Age, u.TotalSpend,
			formatNullableTime(u.CartLastActiveAt), formatNullableTime(u.LastPurchaseAt),
			boolToInt(u.OptInSMS), boolToInt(u.OptInEmail), formatTime(u.CreatedAt),
		)
		if err != nil {
			return fmt.Errorf("insert user %q: %w", u.Email, err)
		}
		if u.ID, err = res.LastInsertId(); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Count returns the total number of users.
func (r *UserRepository) Count(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&n)
	return n, err
}

// GetByID returns the user with the given id or ErrNotFound.
func (r *UserRepository) GetByID(ctx context.Context, id int64) (domain.User, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE id = ?", id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, ErrNotFound
	}
	return u, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(s rowScanner) (domain.User, error) {
	var (
		u                    domain.User
		cartAt, purchaseAt   sql.NullString
		createdAt            string
		optInSMS, optInEmail int
	)
	if err := s.Scan(&u.ID, &u.FirstName, &u.LastName, &u.Email, &u.Phone, &u.City,
		&u.Age, &u.TotalSpend, &cartAt, &purchaseAt, &optInSMS, &optInEmail, &createdAt); err != nil {
		return domain.User{}, err
	}
	var err error
	if u.CartLastActiveAt, err = parseNullableTime(cartAt); err != nil {
		return domain.User{}, err
	}
	if u.LastPurchaseAt, err = parseNullableTime(purchaseAt); err != nil {
		return domain.User{}, err
	}
	if u.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.User{}, err
	}
	u.OptInSMS = optInSMS == 1
	u.OptInEmail = optInEmail == 1
	return u, nil
}

// forEachChunk bounds the IN (...) list; SQLite's default variable limit is
// far higher, but small chunks keep each read short.
const forEachChunk = 500

// ForEachByIDs loads the given users in id chunks and calls fn for each one
// that exists, in id order within a chunk. Missing ids are skipped. Rows are
// fully read and closed before fn runs, so fn may write to the database.
func (r *UserRepository) ForEachByIDs(ctx context.Context, ids []int64, fn func(domain.User) error) error {
	for start := 0; start < len(ids); start += forEachChunk {
		chunk := ids[start:min(start+forEachChunk, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		rows, err := r.db.QueryContext(ctx,
			"SELECT "+userColumns+" FROM users WHERE id IN ("+placeholders+") ORDER BY id", args...)
		if err != nil {
			return err
		}
		var users []domain.User
		for rows.Next() {
			u, err := scanUser(rows)
			if err != nil {
				rows.Close()
				return err
			}
			users = append(users, u)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, u := range users {
			if err := fn(u); err != nil {
				return err
			}
		}
	}
	return nil
}

// SetOptIn records a user's consent change for one channel (subscribe /
// unsubscribe). The column name comes from a fixed map, never from input.
func (r *UserRepository) SetOptIn(ctx context.Context, userID int64, ch domain.Channel, optIn bool) error {
	col, ok := optInColumns[ch]
	if !ok {
		return fmt.Errorf("set opt-in: unsupported channel %q", ch)
	}
	res, err := r.db.ExecContext(ctx, "UPDATE users SET "+col+" = ? WHERE id = ?", boolToInt(optIn), userID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}
