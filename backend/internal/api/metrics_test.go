package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/api"
	"smart-campaign-composer/backend/internal/domain"
)

// Unit tests of GET /metrics with stub services: no database, no provider,
// no LLM. The database-backed totals are covered by
// repository.TestMetricsTotals and by TestEndToEndCampaignFlow.

type stubMetrics struct {
	totals domain.SystemTotals
	err    error
	calls  int
}

func (m *stubMetrics) Totals(context.Context) (domain.SystemTotals, error) {
	m.calls++
	return m.totals, m.err
}

// activeDispatcher reports a fixed number of live runs.
type activeDispatcher struct {
	csrfDispatcher
	active int
}

func (d *activeDispatcher) ActiveRuns() int { return d.active }

// manualClock is a Config.Now the test moves forward.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func metricsServer(t *testing.T, store api.MetricsStore, disp api.DispatchService, now func() time.Time) *httptest.Server {
	t.Helper()
	s, err := api.New(api.Config{
		Segments: csrfSegments{}, Composer: csrfComposer{}, Campaigns: &csrfCampaigns{}, Users: csrfUsers{},
		Dispatcher: disp,
		Metrics:    store,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:        now,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func getMetrics(t *testing.T, srv *httptest.Server) (int, http.Header, []byte) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, b
}

func TestMetricsEndpointReportsGlobalTotals(t *testing.T) {
	clock := &manualClock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	store := &stubMetrics{totals: domain.SystemTotals{Campaigns: 7, Sent: 2290, Failed: 4, Retries: 381}}
	srv := metricsServer(t, store, &activeDispatcher{active: 2}, clock.Now)

	clock.Advance(90*time.Second + 400*time.Millisecond)
	code, header, body := getMetrics(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, body)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store (metrics must never be cached)", cc)
	}

	// Exactly the documented contract: these six keys, nothing else.
	var raw map[string]json.Number
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	want := map[string]string{
		"uptime_seconds":    "90",
		"total_campaigns":   "7",
		"total_sent":        "2290",
		"total_failed":      "4",
		"total_retried":     "381",
		"active_dispatches": "2",
	}
	if len(raw) != len(want) {
		t.Fatalf("keys = %v, want exactly %v", raw, want)
	}
	for k, v := range want {
		if got, ok := raw[k]; !ok || got.String() != v {
			t.Errorf("%s = %q (present %v), want %s", k, got, ok, v)
		}
	}
	if store.calls != 1 {
		t.Fatalf("store called %d times, want 1", store.calls)
	}

	// Uptime follows the server clock; totals are read fresh on every call.
	clock.Advance(30 * time.Second)
	store.totals.Sent = 2300
	_, _, body = getMetrics(t, srv)
	var m struct {
		Uptime float64 `json:"uptime_seconds"`
		Sent   int64   `json:"total_sent"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.Uptime != 120 || m.Sent != 2300 {
		t.Fatalf("second call: uptime %v sent %d, want 120 and 2300", m.Uptime, m.Sent)
	}
}

func TestMetricsEndpointEmptySystem(t *testing.T) {
	srv := metricsServer(t, &stubMetrics{}, &activeDispatcher{}, nil)
	code, _, body := getMetrics(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, body)
	}
	var m map[string]float64
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"total_campaigns", "total_sent", "total_failed", "total_retried", "active_dispatches"} {
		if v, ok := m[k]; !ok || v != 0 {
			t.Errorf("%s = %v (present %v), want 0", k, v, ok)
		}
	}
}

func TestMetricsEndpointStoreErrorIsInternal(t *testing.T) {
	store := &stubMetrics{err: errors.New("database is locked: /var/lib/secret/campaign.db")}
	srv := metricsServer(t, store, &activeDispatcher{}, nil)
	code, _, body := getMetrics(t, srv)
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", code, body)
	}
	if !strings.Contains(string(body), `"code":"internal"`) || strings.Contains(string(body), "secret") {
		t.Fatalf("body %s: want a generic internal error without the cause", body)
	}
}

func TestMetricsEndpointWithoutStore(t *testing.T) {
	srv := metricsServer(t, nil, &activeDispatcher{}, nil)
	code, _, body := getMetrics(t, srv)
	if code != http.StatusServiceUnavailable || !strings.Contains(string(body), `"code":"metrics_unavailable"`) {
		t.Fatalf("status = %d, body %s; want 503 metrics_unavailable", code, body)
	}
}
