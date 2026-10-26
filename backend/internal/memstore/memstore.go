// Package memstore holds in-memory implementations of the persistence ports
// (dispatch.Store, dispatch.CampaignStore, api.CampaignStore,
// dispatch.UserLoader). They mirror the semantics of internal/repository —
// one row per (campaign, user, channel), final states never overwritten,
// compare-and-set status changes, the dispatch lease — so the dispatcher and
// the API can be tested, including races and database failures, without
// SQLite. They are test doubles: nothing here is persisted.
package memstore

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"smart-campaign-composer/backend/internal/domain"
)

// ---------------------------------------------------------------------------
// Dispatch logs
// ---------------------------------------------------------------------------

type key struct {
	campaignID, userID int64
	ch                 domain.Channel
}

// Row is one dispatch log.
type Row struct {
	Status     domain.DispatchStatus
	Retries    int
	ProviderID string
	LastError  string
}

// Store implements dispatch.Store.
type Store struct {
	mu   sync.Mutex
	rows map[key]*Row

	markSentErr func(userID int64, call int) error
	markSentN   map[int64]int
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{rows: map[key]*Row{}, markSentN: map[int64]int{}}
}

// SetMarkSentErr installs a fault: f is consulted before every MarkSent with
// the 1-based number of MarkSent calls made so far for that user. nil
// removes it.
func (s *Store) SetMarkSentErr(f func(userID int64, call int) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markSentErr = f
}

// AddPending inserts pending rows, as an earlier, interrupted run would have
// left them.
func (s *Store) AddPending(campaignID int64, ch domain.Channel, userIDs ...int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range userIDs {
		s.rows[key{campaignID, id, ch}] = &Row{Status: domain.DispatchStatusPending}
	}
}

// Row returns a copy of one dispatch log (ok false if there is none).
func (s *Store) Row(campaignID, userID int64, ch domain.Channel) (Row, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[key{campaignID, userID, ch}]
	if !ok {
		return Row{}, false
	}
	return *r, true
}

// Status returns the status of one dispatch log ("" if there is none).
func (s *Store) Status(campaignID, userID int64, ch domain.Channel) domain.DispatchStatus {
	r, _ := s.Row(campaignID, userID, ch)
	return r.Status
}

// ClaimBatch reserves every id without a row. Unlike the SQL claim it does
// not re-check opt-in (the store has no users); the dispatcher's own gate
// does.
func (s *Store) ClaimBatch(_ context.Context, campaignID int64, ch domain.Channel, userIDs []int64, _ time.Time) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var got []int64
	for _, id := range userIDs {
		k := key{campaignID, id, ch}
		if _, ok := s.rows[k]; ok {
			continue
		}
		s.rows[k] = &Row{Status: domain.DispatchStatusPending}
		got = append(got, id)
	}
	return got, nil
}

// PendingUserIDs lists claimed but unfinished deliveries, sorted.
func (s *Store) PendingUserIDs(_ context.Context, campaignID int64, ch domain.Channel) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for k, r := range s.rows {
		if k.campaignID == campaignID && k.ch == ch && r.Status == domain.DispatchStatusPending {
			ids = append(ids, k.userID)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// MarkSent finalises a pending row as sent.
func (s *Store) MarkSent(_ context.Context, campaignID, userID int64, ch domain.Channel, providerID string, retries int, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markSentN[userID]++
	if s.markSentErr != nil {
		if err := s.markSentErr(userID, s.markSentN[userID]); err != nil {
			return err
		}
	}
	r, ok := s.rows[key{campaignID, userID, ch}]
	if !ok || r.Status != domain.DispatchStatusPending {
		return domain.ErrDispatchNotPending
	}
	r.Status, r.ProviderID, r.Retries = domain.DispatchStatusSent, providerID, retries
	return nil
}

// MarkFailed finalises a pending row as failed.
func (s *Store) MarkFailed(_ context.Context, campaignID, userID int64, ch domain.Channel, retries int, reason string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[key{campaignID, userID, ch}]
	if !ok || r.Status != domain.DispatchStatusPending {
		return domain.ErrDispatchNotPending
	}
	r.Status, r.LastError, r.Retries = domain.DispatchStatusFailed, reason, retries
	return nil
}

// Counts aggregates the rows of a campaign per channel.
func (s *Store) Counts(_ context.Context, campaignID int64) (map[domain.Channel]domain.DispatchCounts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[domain.Channel]domain.DispatchCounts{}
	for k, r := range s.rows {
		if k.campaignID != campaignID {
			continue
		}
		c := out[k.ch]
		c.Total++
		c.Retries += int64(r.Retries)
		switch r.Status {
		case domain.DispatchStatusPending:
			c.Pending++
		case domain.DispatchStatusSent:
			c.Sent++
		case domain.DispatchStatusFailed:
			c.Failed++
		}
		out[k.ch] = c
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Campaigns
// ---------------------------------------------------------------------------

type campaign struct {
	c          domain.Campaign
	msgs       []domain.CampaignMessage
	owner      string
	leaseUntil time.Time
}

// Campaigns implements dispatch.CampaignStore and api.CampaignStore.
type Campaigns struct {
	mu     sync.Mutex
	byID   map[int64]*campaign
	nextID int64
}

// NewCampaigns returns an empty campaign store.
func NewCampaigns() *Campaigns { return &Campaigns{byID: map[int64]*campaign{}, nextID: 1000} }

// Add stores campaign id in status with approved content on channels and
// the reference segment rule.
func (s *Campaigns) Add(id int64, status domain.CampaignStatus, rule json.RawMessage, channels ...domain.Channel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := &campaign{c: domain.Campaign{ID: id, Brief: "brief", Status: status, SegmentRule: rule}}
	for _, ch := range channels {
		m := domain.CampaignMessage{CampaignID: id, Channel: ch, Content: "Merhaba {{first_name}}, %30 indirim!", Status: domain.MessageStatusApproved}
		if ch == domain.ChannelEmail {
			m.Subject = "{{first_name}}, fırsat"
		}
		cp.msgs = append(cp.msgs, m)
	}
	s.byID[id] = cp
}

// Status returns the status of a campaign ("" if unknown).
func (s *Campaigns) Status(id int64) domain.CampaignStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cp, ok := s.byID[id]; ok {
		return cp.c.Status
	}
	return ""
}

// Lease returns the current dispatch lease of a campaign.
func (s *Campaigns) Lease(id int64) (owner string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cp, ok := s.byID[id]; ok {
		return cp.owner, cp.leaseUntil
	}
	return "", time.Time{}
}

// SetLease overwrites the lease (simulates a crashed or competing worker).
func (s *Campaigns) SetLease(id int64, owner string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cp, ok := s.byID[id]; ok {
		cp.owner, cp.leaseUntil = owner, until
	}
}

func (s *Campaigns) get(id int64) (*campaign, error) {
	cp, ok := s.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return cp, nil
}

// GetByID returns a campaign or domain.ErrNotFound.
func (s *Campaigns) GetByID(_ context.Context, id int64) (domain.Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, err := s.get(id)
	if err != nil {
		return domain.Campaign{}, err
	}
	return cp.c, nil
}

// Messages returns the campaign's messages.
func (s *Campaigns) Messages(_ context.Context, id int64) ([]domain.CampaignMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, ok := s.byID[id]
	if !ok {
		return nil, nil
	}
	return append([]domain.CampaignMessage(nil), cp.msgs...), nil
}

// CreateApproved stores an approved campaign with its messages.
func (s *Campaigns) CreateApproved(_ context.Context, brief string, rule json.RawMessage, msgs []domain.CampaignMessage, now time.Time) (domain.Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := s.nextID
	cp := &campaign{c: domain.Campaign{ID: id, Brief: brief, Status: domain.CampaignStatusApproved, CreatedAt: now, SegmentRule: rule}}
	for _, m := range msgs {
		m.CampaignID, m.Status = id, domain.MessageStatusApproved
		cp.msgs = append(cp.msgs, m)
	}
	s.byID[id] = cp
	return cp.c, nil
}

// TransitionStatus is a compare-and-set status change.
func (s *Campaigns) TransitionStatus(_ context.Context, id int64, from, to domain.CampaignStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, err := s.get(id)
	if err != nil {
		return err
	}
	if cp.c.Status != from {
		return domain.ErrStatusConflict
	}
	cp.c.Status = to
	if to != domain.CampaignStatusSending {
		cp.owner, cp.leaseUntil = "", time.Time{}
	}
	return nil
}

// AcquireDispatch implements the dispatch lease (see
// repository.CampaignRepository.AcquireDispatch for the contract).
func (s *Campaigns) AcquireDispatch(_ context.Context, id int64, from domain.CampaignStatus, owner string, now, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, err := s.get(id)
	if err != nil {
		return err
	}
	if cp.c.Status != from {
		return domain.ErrStatusConflict
	}
	switch from {
	case domain.CampaignStatusApproved:
		cp.c.Status = domain.CampaignStatusSending
	case domain.CampaignStatusSending:
		if cp.owner != "" && cp.owner != owner && !cp.leaseUntil.Before(now) {
			return domain.ErrLeaseHeld
		}
	default:
		return errors.New("memstore: dispatch can only be acquired from approved or sending")
	}
	cp.owner, cp.leaseUntil = owner, until
	return nil
}

// RenewDispatch extends the lease of its owner.
func (s *Campaigns) RenewDispatch(_ context.Context, id int64, owner string, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, err := s.get(id)
	if err != nil {
		return err
	}
	if cp.owner != owner || cp.c.Status != domain.CampaignStatusSending {
		return domain.ErrLeaseLost
	}
	cp.leaseUntil = until
	return nil
}

// ReleaseDispatch ends the owner's run: status sending → to (to may be
// sending, for a run that stopped and can be resumed) and no owner.
func (s *Campaigns) ReleaseDispatch(_ context.Context, id int64, owner string, to domain.CampaignStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, err := s.get(id)
	if err != nil {
		return err
	}
	if cp.owner != owner || cp.c.Status != domain.CampaignStatusSending {
		return domain.ErrLeaseLost
	}
	cp.c.Status, cp.owner, cp.leaseUntil = to, "", time.Time{}
	return nil
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// Users implements dispatch.UserLoader.
type Users struct {
	mu   sync.Mutex
	byID map[int64]domain.User
}

// NewUsers returns a loader holding users.
func NewUsers(users ...domain.User) *Users {
	u := &Users{byID: map[int64]domain.User{}}
	for _, x := range users {
		u.byID[x.ID] = x
	}
	return u
}

// ForEachByIDs calls fn for every known id, in the order given.
func (u *Users) ForEachByIDs(ctx context.Context, ids []int64, fn func(domain.User) error) error {
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		u.mu.Lock()
		x, ok := u.byID[id]
		u.mu.Unlock()
		if !ok {
			continue
		}
		if err := fn(x); err != nil {
			return err
		}
	}
	return nil
}
