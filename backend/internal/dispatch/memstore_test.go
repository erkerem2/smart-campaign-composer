package dispatch

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/memstore"
)

// The in-memory persistence doubles live in internal/memstore so the API
// tests can use them too; this file adds the dispatcher-specific helpers.

// memSender accepts every message and records the idempotency keys it saw.
// delay, if set, makes every send take that long (or until ctx is done).
type memSender struct {
	mu    sync.Mutex
	keys  []string
	delay time.Duration
}

func (s *memSender) Send(ctx context.Context, m Message) (Receipt, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return Receipt{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, m.IdempotencyKey)
	return Receipt{ProviderMessageID: "msg-" + m.IdempotencyKey}, nil
}

func (s *memSender) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}

// newMem returns fresh in-memory persistence and a recording sender.
func newMem() (*memstore.Store, *memstore.Campaigns, *memSender) {
	return memstore.NewStore(), memstore.NewCampaigns(), &memSender{}
}

func (s *memSender) count(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, k := range s.keys {
		if k == key {
			n++
		}
	}
	return n
}

func memUsers(ids ...int64) []domain.User {
	out := make([]domain.User, len(ids))
	for i, id := range ids {
		out[i] = domain.User{ID: id, FirstName: fmt.Sprintf("Kullanıcı%d", id), Email: fmt.Sprintf("u%d@example.com", id),
			Phone: fmt.Sprintf("+90532000%04d", id), OptInSMS: true, OptInEmail: true}
	}
	return out
}

// memConfig takes rate limiting and backoff out of the picture.
func memConfig() Config {
	return Config{WorkersPerChannel: 4, RateLimit: 100000, RateBurst: 100000, RateWindow: time.Second,
		BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, SendTimeout: time.Second}
}

func newMemDispatcher(t *testing.T, store Store, campaigns CampaignStore, sender Sender) *Dispatcher {
	t.Helper()
	d, err := New(store, campaigns, sender, NewComposerRenderer(nil), memConfig(),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func awaitRun(t *testing.T, r *Run) {
	t.Helper()
	select {
	case <-r.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish")
	}
}

// assertConsistent checks the invariants the dashboard relies on.
func assertConsistent(t *testing.T, s Stats) {
	t.Helper()
	check := func(where string, total, processed, sent, failed, pending int64) {
		if pending < 0 || processed > total || processed != sent+failed || pending != total-processed {
			t.Errorf("%s: inconsistent counters total=%d processed=%d sent=%d failed=%d pending=%d",
				where, total, processed, sent, failed, pending)
		}
	}
	check("campaign", s.Total, s.Processed, s.Sent, s.Failed, s.Pending)
	for ch, c := range s.Channels {
		check(string(ch), c.Total, c.Processed, c.Sent, c.Failed, c.Pending)
	}
}
