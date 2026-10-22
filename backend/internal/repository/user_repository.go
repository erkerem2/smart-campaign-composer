package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
