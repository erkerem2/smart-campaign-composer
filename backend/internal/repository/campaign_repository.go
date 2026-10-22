package repository

import (
	"context"
	"database/sql"
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
	)
	err := r.db.QueryRowContext(ctx, "SELECT id, brief, status, created_at FROM campaigns WHERE id = ?", id).
		Scan(&c.ID, &c.Brief, &status, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Campaign{}, ErrNotFound
	}
	if err != nil {
		return domain.Campaign{}, err
	}
	c.Status = domain.CampaignStatus(status)
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
	seen := map[domain.Channel]bool{}
	for i := range msgs {
		m := &msgs[i]
		if !m.Channel.Valid() || seen[m.Channel] {
			return fmt.Errorf("message %d: invalid or duplicate channel %q", i, m.Channel)
		}
		seen[m.Channel] = true
		status := m.Status
		if status == "" {
			status = domain.MessageStatusDraft
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
			campaignID, string(m.Channel), m.Content, subject, string(status))
		if err != nil {
			return fmt.Errorf("insert %s message: %w", m.Channel, err)
		}
		if m.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		m.CampaignID, m.Status = campaignID, status
	}
	return tx.Commit()
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
