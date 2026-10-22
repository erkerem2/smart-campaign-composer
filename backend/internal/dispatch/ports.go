package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"smart-campaign-composer/backend/internal/domain"
)

// ---------------------------------------------------------------------------
// Provider port. Every SMS / e-mail provider call sits behind Sender
// (CLAUDE.md: "every external call MUST sit behind an interface").
// ---------------------------------------------------------------------------

// Message is one personalised delivery handed to a provider.
type Message struct {
	// IdempotencyKey is "campaign_id:user_id:channel". It is identical for
	// every attempt of the same delivery (retries, resume after a crash), so
	// the provider can drop a request it has already accepted.
	IdempotencyKey string         `json:"-"`
	CampaignID     int64          `json:"-"`
	UserID         int64          `json:"-"`
	Channel        domain.Channel `json:"-"`

	To         string `json:"to"`
	Subject    string `json:"subject,omitempty"`     // e-mail only
	Body       string `json:"body"`                  // rendered, never a template
	BodyFormat string `json:"body_format,omitempty"` // e-mail only: markdown | html
}

// Receipt is the provider's acknowledgement of an accepted message.
type Receipt struct {
	ProviderMessageID string
	// Replayed is true when the provider recognised the idempotency key and
	// returned the original result instead of sending again.
	Replayed bool
}

// Sender delivers one message. Implementations must return *SendError for
// HTTP-level failures so the dispatcher can tell transient from permanent
// errors; any other error is treated as a transient transport failure.
type Sender interface {
	Send(ctx context.Context, m Message) (Receipt, error)
}

// SenderFunc adapts a function to Sender.
type SenderFunc func(ctx context.Context, m Message) (Receipt, error)

// Send calls f.
func (f SenderFunc) Send(ctx context.Context, m Message) (Receipt, error) { return f(ctx, m) }

// SendError describes a failed provider call.
type SendError struct {
	StatusCode int           // 0 for transport errors (timeout, connection reset)
	RetryAfter time.Duration // from the Retry-After header, 0 if absent
	Temporary  bool          // retrying the same request may succeed
	Err        error
}

func (e *SendError) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("provider transport error: %v", e.Err)
	}
	return fmt.Sprintf("provider http %d: %v", e.StatusCode, e.Err)
}

func (e *SendError) Unwrap() error { return e.Err }

// classify decides whether err is worth retrying and how long the provider
// asked us to wait.
func classify(err error) (retryable bool, retryAfter time.Duration) {
	var se *SendError
	if errors.As(err, &se) {
		return se.Temporary, se.RetryAfter
	}
	// Unknown errors (including a per-attempt timeout) are treated as
	// transient transport failures. Retrying is safe because every attempt
	// carries the same idempotency key, and it is bounded by MaxRetries.
	return true, 0
}

// ---------------------------------------------------------------------------
// Persistence ports, implemented by internal/repository.
// ---------------------------------------------------------------------------

// Store persists per-recipient delivery state (dispatch_logs).
type Store interface {
	// ClaimBatch atomically reserves (campaign, user, channel) for every user
	// that is opted in to ch right now and has no row yet; it returns the
	// reserved ids. Only reserved deliveries may be sent.
	ClaimBatch(ctx context.Context, campaignID int64, ch domain.Channel, userIDs []int64, now time.Time) ([]int64, error)
	// PendingUserIDs lists claimed but unfinished deliveries (for Resume).
	PendingUserIDs(ctx context.Context, campaignID int64, ch domain.Channel) ([]int64, error)
	MarkSent(ctx context.Context, campaignID, userID int64, ch domain.Channel, providerMessageID string, retries int, at time.Time) error
	MarkFailed(ctx context.Context, campaignID, userID int64, ch domain.Channel, retries int, reason string, at time.Time) error
	Counts(ctx context.Context, campaignID int64) (map[domain.Channel]domain.DispatchCounts, error)
}

// CampaignStore reads campaigns and moves them through their lifecycle.
type CampaignStore interface {
	GetByID(ctx context.Context, id int64) (domain.Campaign, error)
	Messages(ctx context.Context, campaignID int64) ([]domain.CampaignMessage, error)
	// TransitionStatus is a compare-and-set; it fails if the campaign is not
	// in status from (so only one caller can start a campaign).
	TransitionStatus(ctx context.Context, id int64, from, to domain.CampaignStatus) error
}

// AudienceSource streams the recipients of a campaign. It is resolved from
// the approved segment rule by the caller; the dispatcher still re-checks
// opt-in for every user and channel, and the Store re-checks it again in SQL.
type AudienceSource interface {
	Recipients(ctx context.Context, campaignID int64, yield func(domain.User) error) error
}

// AudienceFunc adapts a function to AudienceSource.
type AudienceFunc func(ctx context.Context, campaignID int64, yield func(domain.User) error) error

// Recipients calls f.
func (f AudienceFunc) Recipients(ctx context.Context, campaignID int64, yield func(domain.User) error) error {
	return f(ctx, campaignID, yield)
}

// Renderer personalises an approved message template for one user.
type Renderer interface {
	Render(m domain.CampaignMessage, u domain.User) (Message, error)
}
