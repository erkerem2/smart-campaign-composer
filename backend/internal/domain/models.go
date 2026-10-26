// Package domain holds the core business entities of Smart Campaign Composer.
// It has no dependencies on storage, transport or third-party libraries.
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors shared by storage adapters and use cases, so callers can
// use errors.Is without depending on a concrete repository.
var (
	ErrNotFound       = errors.New("not found")
	ErrStatusConflict = errors.New("campaign status changed concurrently")
	// ErrDispatchNotPending: a delivery's final state was already written
	// (by this run or another one); final states are never overwritten.
	ErrDispatchNotPending = errors.New("dispatch log is not pending")
	// ErrLeaseHeld: another dispatcher owns the campaign's run and its
	// lease has not expired.
	ErrLeaseHeld = errors.New("campaign is being dispatched by another worker")
	// ErrLeaseLost: the caller no longer owns the campaign's run (its lease
	// expired and another dispatcher took it over, or it was released).
	ErrLeaseLost = errors.New("dispatch lease lost")
)

// Channel limits mandated by the case (see CASE_SPEC.md).
const (
	SMSMaxLength          = 160
	EmailSubjectMaxLength = 80
)

// Channel is a delivery channel for campaign messages.
type Channel string

const (
	ChannelSMS   Channel = "sms"
	ChannelEmail Channel = "email"
)

// Channels lists every supported channel in a stable order.
var Channels = []Channel{ChannelSMS, ChannelEmail}

// Valid reports whether c is a supported channel.
func (c Channel) Valid() bool {
	switch c {
	case ChannelSMS, ChannelEmail:
		return true
	}
	return false
}

// CampaignStatus is the lifecycle state of a campaign.
type CampaignStatus string

const (
	CampaignStatusDraft     CampaignStatus = "draft"
	CampaignStatusApproved  CampaignStatus = "approved"
	CampaignStatusSending   CampaignStatus = "sending"
	CampaignStatusCompleted CampaignStatus = "completed"
	CampaignStatusFailed    CampaignStatus = "failed"
)

// Valid reports whether s is a known campaign status.
func (s CampaignStatus) Valid() bool {
	switch s {
	case CampaignStatusDraft, CampaignStatusApproved, CampaignStatusSending,
		CampaignStatusCompleted, CampaignStatusFailed:
		return true
	}
	return false
}

// campaignTransitions encodes the allowed lifecycle:
//
//	draft -> approved -> sending -> completed | failed
//
// A human must approve before anything is sent, so draft can never jump
// straight to sending.
var campaignTransitions = map[CampaignStatus][]CampaignStatus{
	CampaignStatusDraft:    {CampaignStatusApproved},
	CampaignStatusApproved: {CampaignStatusSending},
	CampaignStatusSending:  {CampaignStatusCompleted, CampaignStatusFailed},
}

// CanTransitionTo reports whether a campaign in status s may move to next.
func (s CampaignStatus) CanTransitionTo(next CampaignStatus) bool {
	for _, allowed := range campaignTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// MessageStatus is the review state of a generated campaign message.
type MessageStatus string

const (
	MessageStatusDraft    MessageStatus = "draft"
	MessageStatusApproved MessageStatus = "approved"
)

// Valid reports whether s is a known message status.
func (s MessageStatus) Valid() bool {
	return s == MessageStatusDraft || s == MessageStatusApproved
}

// DispatchStatus is the delivery state of one message to one user.
//
//	pending -> sent | failed
//
// A pending row is a claim: it is written before the provider is called, so
// the (campaign, user, channel) slot is reserved exactly once even when
// several workers or processes race. sent and failed are terminal.
type DispatchStatus string

const (
	DispatchStatusPending DispatchStatus = "pending"
	DispatchStatusSent    DispatchStatus = "sent"
	DispatchStatusFailed  DispatchStatus = "failed"
)

// Valid reports whether s is a known dispatch status.
func (s DispatchStatus) Valid() bool {
	switch s {
	case DispatchStatusPending, DispatchStatusSent, DispatchStatusFailed:
		return true
	}
	return false
}

// Final reports whether s is a terminal dispatch state.
func (s DispatchStatus) Final() bool {
	return s == DispatchStatusSent || s == DispatchStatusFailed
}

// User is a (fake) end customer that campaigns can target.
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"email"`
	Phone     string `json:"phone"` // E.164, e.g. +905321234567
	City      string `json:"city"`
	Age       int    `json:"age"`
	// TotalSpend is the lifetime spend in TRY. It is an analytics aggregate
	// used for segmentation, not a ledger balance.
	TotalSpend float64 `json:"total_spend"`
	// CartLastActiveAt is the last time the user added an item to the cart.
	// Nil means the user has never used the cart.
	CartLastActiveAt *time.Time `json:"cart_last_active_at"`
	// LastPurchaseAt is nil when the user has never purchased.
	LastPurchaseAt *time.Time `json:"last_purchase_at"`
	OptInSMS       bool       `json:"opt_in_sms"`
	OptInEmail     bool       `json:"opt_in_email"`
	CreatedAt      time.Time  `json:"created_at"`
}

// CanReceive reports whether the user has opted in to the given channel.
// Dispatchers must call this before sending anything.
func (u User) CanReceive(ch Channel) bool {
	switch ch {
	case ChannelSMS:
		return u.OptInSMS
	case ChannelEmail:
		return u.OptInEmail
	}
	return false
}

// Campaign is a marketing campaign created from a natural-language brief.
type Campaign struct {
	ID        int64          `json:"id"`
	Brief     string         `json:"brief"`
	Status    CampaignStatus `json:"status"`
	CreatedAt time.Time      `json:"created_at"`
	// SegmentRule is the canonical JSON rule tree (never SQL) the marketer
	// approved. It is opaque here; internal/segment validates it on read.
	SegmentRule json.RawMessage `json:"segment_rule,omitempty"`
}

// CampaignMessage is the per-channel content of a campaign.
type CampaignMessage struct {
	ID         int64   `json:"id"`
	CampaignID int64   `json:"campaign_id"`
	Channel    Channel `json:"channel"`
	Content    string  `json:"content"`
	// Subject is only used by email; it is empty for SMS.
	Subject string        `json:"subject,omitempty"`
	Status  MessageStatus `json:"status"`
}

// DispatchLog records the delivery of one campaign message to one user on
// one channel. (CampaignID, UserID, Channel) is unique, which is what makes
// "the same person never receives the same message twice" enforceable at the
// storage layer.
type DispatchLog struct {
	ID                int64          `json:"id"`
	CampaignID        int64          `json:"campaign_id"`
	UserID            int64          `json:"user_id"`
	Channel           Channel        `json:"channel"`
	Status            DispatchStatus `json:"status"`
	ProviderMessageID string         `json:"provider_message_id,omitempty"`
	RetryCount        int            `json:"retry_count"`
	// LastError is the reason of the final failure (empty when sent).
	LastError string     `json:"last_error,omitempty"`
	SentAt    *time.Time `json:"sent_at,omitempty"`
}

// DispatchCounts aggregates dispatch_logs rows of one campaign and channel.
type DispatchCounts struct {
	Total   int64 `json:"total"`
	Pending int64 `json:"pending"`
	Sent    int64 `json:"sent"`
	Failed  int64 `json:"failed"`
	// Retries is the number of retry attempts (not messages) recorded on
	// finished rows.
	Retries int64 `json:"retries"`
}

// IdempotencyKey returns the key sent to providers for this delivery.
func (d DispatchLog) IdempotencyKey() string {
	return IdempotencyKey(d.CampaignID, d.UserID, d.Channel)
}

// IdempotencyKey builds the "campaign_id:user_id:channel" key used to
// deduplicate deliveries across retries and restarts.
func IdempotencyKey(campaignID, userID int64, ch Channel) string {
	return fmt.Sprintf("%d:%d:%s", campaignID, userID, ch)
}
