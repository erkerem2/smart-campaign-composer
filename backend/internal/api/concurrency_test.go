package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/api"
	"smart-campaign-composer/backend/internal/dispatch"
	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/memstore"
	"smart-campaign-composer/backend/internal/segment"
)

// countingSender records every idempotency key it is asked to deliver.
type countingSender struct {
	mu    sync.Mutex
	keys  map[string]int
	delay time.Duration
}

func (s *countingSender) Send(ctx context.Context, m dispatch.Message) (dispatch.Receipt, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return dispatch.Receipt{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[m.IdempotencyKey]++
	return dispatch.Receipt{ProviderMessageID: "id-" + m.IdempotencyKey}, nil
}

func (s *countingSender) snapshot() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.keys))
	for k, v := range s.keys {
		out[k] = v
	}
	return out
}

// B1. Twenty POST /dispatch for the same campaign at the same instant, over
// two API servers (two processes) sharing one store: one 202, nineteen 409,
// every recipient gets each channel's message exactly once.
func TestParallelDispatchRequests(t *testing.T) {
	campaigns, store := memstore.NewCampaigns(), memstore.NewStore()
	campaigns.Add(7, domain.CampaignStatusApproved, json.RawMessage(segment.ReferenceRule), domain.ChannelSMS, domain.ChannelEmail)
	users := memstore.NewUsers(
		domain.User{ID: 1, FirstName: "Ayşe", Phone: "+905320000001", Email: "a@example.com", OptInSMS: true, OptInEmail: true},
		domain.User{ID: 2, FirstName: "Can", Phone: "+905320000002", Email: "c@example.com", OptInSMS: true, OptInEmail: true},
	)
	sender := &countingSender{keys: map[string]int{}, delay: 30 * time.Millisecond}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	servers := make([]*httptest.Server, 2)
	dispatchers := make([]*dispatch.Dispatcher, 2)
	for i := range servers {
		d, err := dispatch.New(store, campaigns, sender, dispatch.NewComposerRenderer(nil), dispatch.Config{WorkersPerChannel: 1},
			dispatch.WithLogger(quiet))
		if err != nil {
			t.Fatal(err)
		}
		dispatchers[i] = d
		s, err := api.New(api.Config{Segments: csrfSegments{}, Composer: csrfComposer{}, Campaigns: campaigns,
			Users: users, Dispatcher: d, Logger: quiet})
		if err != nil {
			t.Fatal(err)
		}
		servers[i] = httptest.NewServer(s.Handler())
		t.Cleanup(servers[i].Close)
	}

	const n = 20
	statuses := make([]int, n)
	bodies := make([]string, n)
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			req, _ := http.NewRequest(http.MethodPost, servers[i%2].URL+"/api/campaigns/7/dispatch", strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			ready.Done()
			<-start
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			statuses[i], bodies[i] = resp.StatusCode, string(b)
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()

	accepted := 0
	for i, st := range statuses {
		switch {
		case st == http.StatusAccepted:
			accepted++
		case st == http.StatusConflict && (strings.Contains(bodies[i], `"already_running"`) || strings.Contains(bodies[i], `"invalid_state"`)):
		default:
			t.Errorf("request %d: %d %s", i, st, bodies[i])
		}
	}
	if accepted != 1 {
		t.Fatalf("%d requests were accepted, want exactly 1", accepted)
	}

	deadline := time.Now().Add(10 * time.Second)
	for campaigns.Status(7) != domain.CampaignStatusCompleted {
		if time.Now().After(deadline) {
			t.Fatalf("campaign did not complete: %s", campaigns.Status(7))
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, d := range dispatchers {
		d.Wait()
	}
	got := sender.snapshot()
	if len(got) != 4 {
		t.Fatalf("deliveries = %v, want 2 users × 2 channels", got)
	}
	for _, uid := range []int64{1, 2} {
		for _, ch := range []domain.Channel{domain.ChannelSMS, domain.ChannelEmail} {
			if k := domain.IdempotencyKey(7, uid, ch); got[k] != 1 {
				t.Errorf("%s delivered %d times", k, got[k])
			}
		}
	}
	if owner, _ := campaigns.Lease(7); owner != "" {
		t.Errorf("lease not released: %q", owner)
	}
}
