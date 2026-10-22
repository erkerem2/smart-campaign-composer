package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"smart-campaign-composer/backend/internal/domain"
)

// ErrDispatchNotPending is returned when a delivery is finalised that is not
// in the pending state (already finalised, or never claimed). A final state is
// never overwritten.
var ErrDispatchNotPending = errors.New("dispatch log is not pending")

// optInColumns maps a channel to its opt-in column. Column names only ever
// come from this map, never from input.
var optInColumns = map[domain.Channel]string{
	domain.ChannelSMS:   "opt_in_sms",
	domain.ChannelEmail: "opt_in_email",
}

// claimBatchSize bounds the size of one claim transaction so a large
// audience does not hold SQLite's single writer lock for long.
const claimBatchSize = 500

// DispatchRepository persists per-recipient delivery state (dispatch_logs).
//
// It implements the claim protocol described in ADR 007:
//
//	ClaimBatch   INSERT 'pending' iff the user is opted in AND no row exists
//	MarkSent     pending -> sent    (only from pending)
//	MarkFailed   pending -> failed  (only from pending)
type DispatchRepository struct {
	db *sql.DB
}

// NewDispatchRepository returns a repository backed by db.
func NewDispatchRepository(db *sql.DB) *DispatchRepository {
	return &DispatchRepository{db: db}
}

// ClaimBatch reserves the (campaignID, user, ch) slot for every user in
// userIDs that is opted in to ch at this moment and has no dispatch row for
// this campaign and channel yet. It returns the ids it reserved; only those
// may be sent to the provider.
//
// Both guarantees are evaluated by SQLite inside the INSERT itself:
//   - permission: the row is selected from users WHERE opt_in_<ch> = 1, so a
//     user who opted out after the audience was loaded is never claimed;
//   - uniqueness: ON CONFLICT DO NOTHING on UNIQUE(campaign_id, user_id,
//     channel), so a second claim (other worker, other run, other process)
//     affects zero rows.
func (r *DispatchRepository) ClaimBatch(ctx context.Context, campaignID int64, ch domain.Channel, userIDs []int64, now time.Time) ([]int64, error) {
	col, ok := optInColumns[ch]
	if !ok {
		return nil, fmt.Errorf("claim: unsupported channel %q", ch)
	}
	query := `INSERT INTO dispatch_logs (campaign_id, user_id, channel, status, created_at, updated_at)
		SELECT ?, id, ?, 'pending', ?, ? FROM users WHERE id = ? AND ` + col + ` = 1
		ON CONFLICT (campaign_id, user_id, channel) DO NOTHING`
	ts := formatTime(now)

	acquired := make([]int64, 0, len(userIDs))
	for start := 0; start < len(userIDs); start += claimBatchSize {
		end := min(start+claimBatchSize, len(userIDs))
		got, err := r.claimChunk(ctx, query, campaignID, ch, userIDs[start:end], ts)
		if err != nil {
			return nil, err
		}
		acquired = append(acquired, got...)
	}
	return acquired, nil
}

func (r *DispatchRepository) claimChunk(ctx context.Context, query string, campaignID int64, ch domain.Channel, ids []int64, ts string) ([]int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	var acquired []int64
	for _, id := range ids {
		res, err := stmt.ExecContext(ctx, campaignID, string(ch), ts, ts, id)
		if err != nil {
			return nil, fmt.Errorf("claim %s for user %d: %w", ch, id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 1 {
			acquired = append(acquired, id)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return acquired, nil
}

// PendingUserIDs returns users whose delivery on ch was claimed but never
// finalised (e.g. the process stopped mid-run). Resume re-sends them with the
// same idempotency key, which the provider deduplicates.
func (r *DispatchRepository) PendingUserIDs(ctx context.Context, campaignID int64, ch domain.Channel) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT user_id FROM dispatch_logs WHERE campaign_id = ? AND channel = ? AND status = 'pending' ORDER BY user_id",
		campaignID, string(ch))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MarkSent finalises a pending delivery as sent.
func (r *DispatchRepository) MarkSent(ctx context.Context, campaignID, userID int64, ch domain.Channel, providerMessageID string, retries int, at time.Time) error {
	ts := formatTime(at)
	return r.finalise(ctx, `UPDATE dispatch_logs
		SET status = 'sent', provider_message_id = ?, retry_count = ?, last_error = NULL, sent_at = ?, updated_at = ?
		WHERE campaign_id = ? AND user_id = ? AND channel = ? AND status = 'pending'`,
		providerMessageID, retries, ts, ts, campaignID, userID, string(ch))
}

// MarkFailed finalises a pending delivery as failed. reason is stored for
// operators; it never contains message content.
func (r *DispatchRepository) MarkFailed(ctx context.Context, campaignID, userID int64, ch domain.Channel, retries int, reason string, at time.Time) error {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	return r.finalise(ctx, `UPDATE dispatch_logs
		SET status = 'failed', retry_count = ?, last_error = ?, updated_at = ?
		WHERE campaign_id = ? AND user_id = ? AND channel = ? AND status = 'pending'`,
		retries, reason, formatTime(at), campaignID, userID, string(ch))
}

func (r *DispatchRepository) finalise(ctx context.Context, query string, args ...any) error {
	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDispatchNotPending
	}
	return nil
}

// Get returns one dispatch log or ErrNotFound.
func (r *DispatchRepository) Get(ctx context.Context, campaignID, userID int64, ch domain.Channel) (domain.DispatchLog, error) {
	var (
		d                       domain.DispatchLog
		channel, status         string
		providerID, lastErr, at sql.NullString
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT id, campaign_id, user_id, channel, status, provider_message_id, retry_count, last_error, sent_at
		FROM dispatch_logs WHERE campaign_id = ? AND user_id = ? AND channel = ?`,
		campaignID, userID, string(ch)).
		Scan(&d.ID, &d.CampaignID, &d.UserID, &channel, &status, &providerID, &d.RetryCount, &lastErr, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DispatchLog{}, ErrNotFound
	}
	if err != nil {
		return domain.DispatchLog{}, err
	}
	d.Channel, d.Status = domain.Channel(channel), domain.DispatchStatus(status)
	d.ProviderMessageID, d.LastError = providerID.String, lastErr.String
	if d.SentAt, err = parseNullableTime(at); err != nil {
		return domain.DispatchLog{}, err
	}
	return d, nil
}

// Counts aggregates the dispatch logs of a campaign per channel in a single
// indexed query. It is the durable source for the dashboard when no run is
// active in this process (e.g. after a restart).
func (r *DispatchRepository) Counts(ctx context.Context, campaignID int64) (map[domain.Channel]domain.DispatchCounts, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT channel,
		       COUNT(*),
		       COALESCE(SUM(CASE WHEN status = 'pending' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'sent'    THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'failed'  THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(retry_count), 0)
		FROM dispatch_logs WHERE campaign_id = ? GROUP BY channel`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[domain.Channel]domain.DispatchCounts{}
	for rows.Next() {
		var (
			ch string
			c  domain.DispatchCounts
		)
		if err := rows.Scan(&ch, &c.Total, &c.Pending, &c.Sent, &c.Failed, &c.Retries); err != nil {
			return nil, err
		}
		out[domain.Channel(ch)] = c
	}
	return out, rows.Err()
}
