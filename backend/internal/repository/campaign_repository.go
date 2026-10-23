package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"smart-campaign-composer/backend/internal/domain"
)

// ErrCampaignNotDraft is returned when content of a campaign that already
// left the draft state is changed. Approved content is immutable.
var ErrCampaignNotDraft = errors.New("campaign is not in draft status")

// CampaignRepository persists campaigns and their per-channel messages.
type CampaignRepository struct {
	db *sql.DB
}

// NewCampaignRepository returns a repository backed by db.
func NewCampaignRepository(db *sql.DB) *CampaignRepository {
	return &CampaignRepository{db: db}
}

// Create inserts a draft campaign for brief.
func (r *CampaignRepository) Create(ctx context.Context, brief string, now time.Time) (domain.Campaign, error) {
	c := domain.Campaign{Brief: strings.TrimSpace(brief), Status: domain.CampaignStatusDraft, CreatedAt: now.UTC().Truncate(time.Second)}
	res, err := r.db.ExecContext(ctx, "INSERT INTO campaigns (brief, status, created_at) VALUES (?, ?, ?)",
		c.Brief, string(c.Status), formatTime(c.CreatedAt))
	if err != nil {
		return domain.Campaign{}, fmt.Errorf("insert campaign: %w", err)
	}
	if c.ID, err = res.LastInsertId(); err != nil {
		return domain.Campaign{}, err
	}
	return c, nil
}

// GetByID returns the campaign with the given id or ErrNotFound.
func (r *CampaignRepository) GetByID(ctx context.Context, id int64) (domain.Campaign, error) {
	var (
		c         domain.Campaign
		status    string
		createdAt string
		rule      sql.NullString
	)
	err := r.db.QueryRowContext(ctx, "SELECT id, brief, status, created_at, segment_rule FROM campaigns WHERE id = ?", id).
		Scan(&c.ID, &c.Brief, &status, &createdAt, &rule)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Campaign{}, ErrNotFound
	}
	if err != nil {
		return domain.Campaign{}, err
	}
	c.Status = domain.CampaignStatus(status)
	if rule.Valid {
		c.SegmentRule = json.RawMessage(rule.String)
	}
	if c.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.Campaign{}, err
	}
	return c, nil
}

// SaveDraftMessages replaces all messages of a draft campaign in one
// transaction (regenerate / edit). Callers pass content that already passed
// composer validation; the schema's CHECK constraints are the last line of
// defence (e.g. an SMS row can never carry a subject).
func (r *CampaignRepository) SaveDraftMessages(ctx context.Context, campaignID int64, msgs []domain.CampaignMessage) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var status string
	err = tx.QueryRowContext(ctx, "SELECT status FROM campaigns WHERE id = ?", campaignID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if domain.CampaignStatus(status) != domain.CampaignStatusDraft {
		return ErrCampaignNotDraft
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM campaign_messages WHERE campaign_id = ?", campaignID); err != nil {
		return fmt.Errorf("delete old messages: %w", err)
	}
	if err := insertMessages(ctx, tx, campaignID, msgs, domain.MessageStatusDraft); err != nil {
		return err
	}
	return tx.Commit()
}

// insertMessages writes one row per channel. status is used for messages
// that do not carry their own.
func insertMessages(ctx context.Context, tx *sql.Tx, campaignID int64, msgs []domain.CampaignMessage, status domain.MessageStatus) error {
	seen := map[domain.Channel]bool{}
	for i := range msgs {
		m := &msgs[i]
		if !m.Channel.Valid() || seen[m.Channel] {
			return fmt.Errorf("message %d: invalid or duplicate channel %q", i, m.Channel)
		}
		seen[m.Channel] = true
		st := m.Status
		if st == "" {
			st = status
		}
		// SMS has no subject column value at all (NULL), e-mail always has one.
		var subject sql.NullString
		if m.Channel == domain.ChannelEmail {
			subject = sql.NullString{String: m.Subject, Valid: true}
		} else if m.Subject != "" {
			return fmt.Errorf("message %d: %s message cannot have a subject", i, m.Channel)
		}
		res, err := tx.ExecContext(ctx,
			"INSERT INTO campaign_messages (campaign_id, channel, content, subject, status) VALUES (?, ?, ?, ?, ?)",
			campaignID, string(m.Channel), m.Content, subject, string(st))
		if err != nil {
			return fmt.Errorf("insert %s message: %w", m.Channel, err)
		}
		if m.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		m.CampaignID, m.Status = campaignID, st
	}
	return nil
}

// ErrMissingSegmentRule is returned when a campaign is approved without an
// audience rule.
var ErrMissingSegmentRule = errors.New("campaign has no segment rule")

// CreateApproved stores a campaign the marketer has reviewed and approved in
// one step: the campaign row (status approved, with its validated segment
// rule) and its per-channel messages (status approved) are written in a
// single transaction, so a half-created campaign can never be dispatched.
//
// Callers pass a rule that went through the segment parser (canonical JSON)
// and content that passed composer validation; the schema CHECKs
// (json_valid, SMS without subject, ...) are the last line of defence.
func (r *CampaignRepository) CreateApproved(ctx context.Context, brief string, rule json.RawMessage,
	msgs []domain.CampaignMessage, now time.Time) (domain.Campaign, error) {
	if len(rule) == 0 {
		return domain.Campaign{}, ErrMissingSegmentRule
	}
	if len(msgs) == 0 {
		return domain.Campaign{}, ErrNoMessages
	}
	c := domain.Campaign{
		Brief: strings.TrimSpace(brief), Status: domain.CampaignStatusApproved,
		CreatedAt: now.UTC().Truncate(time.Second), SegmentRule: append(json.RawMessage(nil), rule...),
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Campaign{}, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		"INSERT INTO campaigns (brief, status, created_at, segment_rule) VALUES (?, ?, ?, ?)",
		c.Brief, string(c.Status), formatTime(c.CreatedAt), string(c.SegmentRule))
	if err != nil {
		return domain.Campaign{}, fmt.Errorf("insert campaign: %w", err)
	}
	if c.ID, err = res.LastInsertId(); err != nil {
		return domain.Campaign{}, err
	}
	for i := range msgs {
		msgs[i].Status = domain.MessageStatusApproved
	}
	if err := insertMessages(ctx, tx, c.ID, msgs, domain.MessageStatusApproved); err != nil {
		return domain.Campaign{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Campaign{}, err
	}
	return c, nil
}

// Messages returns the messages of a campaign, SMS first.
func (r *CampaignRepository) Messages(ctx context.Context, campaignID int64) ([]domain.CampaignMessage, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, campaign_id, channel, content, subject, status
		FROM campaign_messages WHERE campaign_id = ?
		ORDER BY CASE channel WHEN 'sms' THEN 0 ELSE 1 END, id`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CampaignMessage
	for rows.Next() {
		var (
			m               domain.CampaignMessage
			channel, status string
			subject         sql.NullString
		)
		if err := rows.Scan(&m.ID, &m.CampaignID, &channel, &m.Content, &subject, &status); err != nil {
			return nil, err
		}
		m.Channel, m.Status, m.Subject = domain.Channel(channel), domain.MessageStatus(status), subject.String
		out = append(out, m)
	}
	return out, rows.Err()
}

// ErrStatusConflict is returned by TransitionStatus when the campaign is not
// in the expected status (another request changed it first).
var ErrStatusConflict = domain.ErrStatusConflict

// ErrNoMessages is returned when a campaign without content is approved.
var ErrNoMessages = errors.New("campaign has no messages")

// TransitionStatus moves a campaign from one status to another with a
// compare-and-set UPDATE. Only transitions allowed by domain are accepted,
// and exactly one of several concurrent callers can win (e.g. two "send"
// clicks: only one moves approved -> sending).
func (r *CampaignRepository) TransitionStatus(ctx context.Context, id int64, from, to domain.CampaignStatus) error {
	if !from.CanTransitionTo(to) {
		return fmt.Errorf("campaign %d: transition %s -> %s is not allowed", id, from, to)
	}
	res, err := r.db.ExecContext(ctx, "UPDATE campaigns SET status = ? WHERE id = ? AND status = ?",
		string(to), id, string(from))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	if _, err := r.GetByID(ctx, id); err != nil {
		return err // ErrNotFound
	}
	return ErrStatusConflict
}

// Approve is the human approval step: it marks every message of a draft
// campaign as approved and moves the campaign to approved, atomically.
// Approved content is immutable (SaveDraftMessages refuses it).
func (r *CampaignRepository) Approve(ctx context.Context, id int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var status string
	err = tx.QueryRowContext(ctx, "SELECT status FROM campaigns WHERE id = ?", id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if domain.CampaignStatus(status) != domain.CampaignStatusDraft {
		return ErrCampaignNotDraft
	}
	res, err := tx.ExecContext(ctx, "UPDATE campaign_messages SET status = 'approved' WHERE campaign_id = ?", id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNoMessages
	}
	if _, err := tx.ExecContext(ctx, "UPDATE campaigns SET status = 'approved' WHERE id = ? AND status = 'draft'", id); err != nil {
		return err
	}
	return tx.Commit()
}
