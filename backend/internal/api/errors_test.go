package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/api"
	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/dispatch"
	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/provider"
	"smart-campaign-composer/backend/internal/segment"
)

func smallStack(t *testing.T, o stackOpts) *stack {
	t.Helper()
	o.users = 200
	return newStack(t, o)
}

func TestPreviewSegmentErrors(t *testing.T) {
	s := smallStack(t, stackOpts{})
	const path = "/api/campaigns/preview-segment"

	expectError(t, s.postJSON(t, path, map[string]string{"brief": "   "}), http.StatusBadRequest, "invalid_brief")
	expectError(t, s.postJSON(t, path, map[string]string{"brief": strings.Repeat("ç", segment.MaxBriefRunes+1)}),
		http.StatusBadRequest, "invalid_brief")
	expectError(t, s.postJSON(t, path, map[string]any{"brief": "x", "sql": "DROP TABLE users"}), http.StatusBadRequest, "invalid_json")
	expectError(t, s.do(t, http.MethodPost, path, "application/json", []byte(`{"brief":"a"}{"brief":"b"}`), nil),
		http.StatusBadRequest, "invalid_json")
	expectError(t, s.do(t, http.MethodPost, path, "application/json", []byte(`{"brief":"`+strings.Repeat("a", 300<<10)+`"}`), nil),
		http.StatusRequestEntityTooLarge, "body_too_large")

	// A compromised / hallucinating model: the rule is rejected, never run.
	s.segLLM.On("kötü brief", `{"type":"group","logic":"AND","rules":[
		{"type":"condition","field":"password_hash","operator":"EQUALS","value":"x"}]}`)
	if e := expectError(t, s.postJSON(t, path, map[string]string{"brief": "kötü brief"}),
		http.StatusUnprocessableEntity, "invalid_rule"); e.Error.Path != "$.rules[0].field" {
		t.Fatalf("path = %q", e.Error.Path)
	}
	s.segLLM.On("sql brief", `SELECT * FROM users`)
	expectError(t, s.postJSON(t, path, map[string]string{"brief": "sql brief"}), http.StatusUnprocessableEntity, "invalid_rule")

	// The LLM is down: 502 without leaking transport details.
	s.segLLM.Err = errors.New("dial tcp: secret-internal-host:443: connection refused")
	e := expectError(t, s.postJSON(t, path, map[string]string{"brief": composer.ReferenceBrief}), http.StatusBadGateway, "llm_unavailable")
	if strings.Contains(e.Error.Message, "secret-internal-host") {
		t.Fatalf("LLM transport error leaked: %q", e.Error.Message)
	}
	s.segLLM.Err = nil

	// A rule edited in the UI is validated and counted without the LLM.
	calls := len(s.segLLM.Requests)
	r := s.postJSON(t, path, map[string]any{"brief": "elle düzenlendi", "rule": json.RawMessage(`{"type":"group","logic":"AND","rules":[
		{"type":"condition","field":"city","operator":"EQUALS","value":"istanbul"}]}`)})
	if r.status != http.StatusOK {
		t.Fatalf("edited rule = %d %s", r.status, r.body)
	}
	p := decode[previewBody](t, r)
	if p.Audience.Total == 0 || !strings.Contains(string(p.Rule), "İstanbul") || p.Brief != "elle düzenlendi" || len(s.segLLM.Requests) != calls {
		t.Fatalf("edited rule preview = %s (llm calls %d → %d)", r.body, calls, len(s.segLLM.Requests))
	}
	expectError(t, s.postJSON(t, path, map[string]any{"rule": json.RawMessage(`{"type":"group","logic":"XOR","rules":[]}`)}),
		http.StatusUnprocessableEntity, "invalid_rule")
}

func TestComposeStreamErrors(t *testing.T) {
	s := smallStack(t, stackOpts{})
	const path = "/api/campaigns/compose/stream"
	expectError(t, s.postJSON(t, path, map[string]string{"brief": ""}), http.StatusBadRequest, "invalid_brief")
	expectError(t, s.do(t, http.MethodPost, path, "", []byte(`{"brief":"x"}`), nil), http.StatusUnsupportedMediaType, "unsupported_media_type")

	// Failures after the stream started arrive as an SSE error event.
	s.msgLLM.Err = errors.New("upstream overloaded")
	resp := s.openSSE(t, http.MethodPost, path, map[string]string{"brief": composer.ReferenceBrief})
	events := readSSE(t, resp.Body, nil)
	if len(events) != 1 || events[0].Event != "error" || !strings.Contains(events[0].Data, "llm call failed") {
		t.Fatalf("events = %+v", events)
	}
	s.msgLLM.Err = nil

	// Content that stays invalid after the repair round ends with an error
	// event that carries the last attempt for manual editing.
	bad := composer.ReferenceMessage
	bad.SMSContent = strings.Repeat("uzun ", 40)
	b, _ := json.Marshal(bad)
	s.msgLLM.On("çok uzun sms", string(b))
	resp = s.openSSE(t, http.MethodPost, path, map[string]string{"brief": "çok uzun sms"})
	events = readSSE(t, resp.Body, nil)
	last := events[len(events)-1]
	var chunk composer.MessageChunk
	_ = json.Unmarshal([]byte(last.Data), &chunk)
	if last.Event != "error" || chunk.Rejected == nil || len(chunk.Issues) == 0 {
		t.Fatalf("last event = %+v", last)
	}
	sawRetry := false
	for _, ev := range events {
		sawRetry = sawRetry || ev.Event == "retry"
	}
	if !sawRetry {
		t.Fatal("expected one self-repair round (retry event)")
	}
}

func TestCampaignAndDispatchErrors(t *testing.T) {
	s := smallStack(t, stackOpts{})
	ctx := context.Background()

	expectError(t, s.get(t, "/api/campaigns/abc"), http.StatusBadRequest, "invalid_id")
	expectError(t, s.get(t, "/api/campaigns/0"), http.StatusBadRequest, "invalid_id")
	expectError(t, s.get(t, "/api/campaigns/999"), http.StatusNotFound, "campaign_not_found")
	expectError(t, s.postJSON(t, "/api/campaigns/999/dispatch", map[string]any{}), http.StatusNotFound, "campaign_not_found")
	expectError(t, s.get(t, "/api/campaigns/999/dispatch"), http.StatusNotFound, "campaign_not_found")
	expectError(t, s.get(t, "/api/campaigns/999/dispatch/stream"), http.StatusNotFound, "campaign_not_found")
	expectError(t, s.get(t, "/api/nope"), http.StatusNotFound, "not_found")
	expectError(t, s.postJSON(t, "/api/campaigns", map[string]any{"brief": "b", "messages": composer.ReferenceMessage}),
		http.StatusBadRequest, "missing_rule")
	expectError(t, s.postJSON(t, "/api/campaigns", map[string]any{"brief": "b", "segment_rule": json.RawMessage(segment.ReferenceRule)}),
		http.StatusBadRequest, "missing_messages")

	// A draft campaign (never approved) cannot be dispatched.
	draft, err := s.campaigns.Create(ctx, "taslak", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	expectError(t, s.postJSON(t, fmt.Sprintf("/api/campaigns/%d/dispatch", draft.ID), map[string]any{}), http.StatusConflict, "invalid_state")

	// A campaign approved before schema v3 has no stored rule.
	legacy, _ := s.campaigns.Create(ctx, "eski", time.Now())
	if err := s.campaigns.SaveDraftMessages(ctx, legacy.ID, composer.ReferenceMessage.CampaignMessages(legacy.ID)); err != nil {
		t.Fatal(err)
	}
	if err := s.campaigns.Approve(ctx, legacy.ID); err != nil {
		t.Fatal(err)
	}
	expectError(t, s.postJSON(t, fmt.Sprintf("/api/campaigns/%d/dispatch", legacy.ID), map[string]any{}), http.StatusConflict, "missing_rule")

	// A stored rule that no longer validates (e.g. the whitelist shrank) is
	// refused at dispatch instead of being compiled.
	tampered, err := s.campaigns.CreateApproved(ctx, "bozuk", json.RawMessage(`{"type":"group","logic":"AND","rules":[{"type":"condition","field":"email","operator":"EQUALS","value":"x"}]}`),
		composer.ReferenceMessage.CampaignMessages(0), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	expectError(t, s.postJSON(t, fmt.Sprintf("/api/campaigns/%d/dispatch", tampered.ID), map[string]any{}), http.StatusConflict, "invalid_rule")

	// A finished campaign streams one snapshot and "done".
	if _, err := s.db.Exec("UPDATE campaigns SET status = 'completed' WHERE id = ?", tampered.ID); err != nil {
		t.Fatal(err)
	}
	resp := s.openSSE(t, http.MethodGet, fmt.Sprintf("/api/campaigns/%d/dispatch/stream", tampered.ID), nil)
	events := readSSE(t, resp.Body, nil)
	if len(events) != 2 || events[0].Event != "stats" || events[1].Event != "done" || !strings.Contains(events[1].Data, dispatch.StateCompleted) {
		t.Fatalf("events = %+v", events)
	}
}

func TestDispatchResumesInterruptedCampaign(t *testing.T) {
	s := smallStack(t, stackOpts{providerCfg: &provider.Config{}}) // reliable provider
	ctx := context.Background()
	c, err := s.campaigns.CreateApproved(ctx, composer.ReferenceBrief, json.RawMessage(segment.ReferenceRule),
		composer.ReferenceMessage.CampaignMessages(0), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a process that died mid-run: campaign stuck in 'sending'.
	if err := s.campaigns.TransitionStatus(ctx, c.ID, domain.CampaignStatusApproved, domain.CampaignStatusSending); err != nil {
		t.Fatal(err)
	}
	r := s.postJSON(t, fmt.Sprintf("/api/campaigns/%d/dispatch", c.ID), map[string]any{})
	if r.status != http.StatusAccepted || !decode[dispatchBody](t, r).Resumed {
		t.Fatalf("resume = %d %s", r.status, r.body)
	}
	resp := s.openSSE(t, http.MethodGet, fmt.Sprintf("/api/campaigns/%d/dispatch/stream", c.ID), nil)
	events := readSSE(t, resp.Body, nil)
	if last := events[len(events)-1]; last.Event != "done" || !strings.Contains(last.Data, dispatch.StateCompleted) {
		t.Fatalf("last event = %+v", last)
	}
}

func TestCORS(t *testing.T) {
	s := smallStack(t, stackOpts{cors: []string{"http://app.test"}})
	pre := func(origin string) response {
		return s.do(t, http.MethodOptions, "/api/campaigns", "", nil, map[string]string{
			"Origin": origin, "Access-Control-Request-Method": "POST"})
	}
	if r := pre("http://app.test"); r.status != http.StatusNoContent || r.header.Get("Access-Control-Allow-Origin") != "http://app.test" ||
		r.header.Get("Access-Control-Max-Age") == "" || !strings.Contains(strings.Join(r.header.Values("Vary"), ","), "Origin") {
		t.Fatalf("allowed preflight = %d %v", r.status, r.header)
	}
	expectError(t, pre("http://evil.test"), http.StatusForbidden, "origin_not_allowed")

	// Actual requests: allowed origins get the header, others do not (the
	// browser then hides the response from the page).
	r := s.do(t, http.MethodGet, "/healthz", "", nil, map[string]string{"Origin": "http://evil.test"})
	if r.status != http.StatusOK || r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("disallowed origin = %d %v", r.status, r.header)
	}
	r = s.do(t, http.MethodGet, "/healthz", "", nil, map[string]string{"Origin": "http://app.test"})
	if r.header.Get("Access-Control-Allow-Origin") != "http://app.test" || r.header.Get("Access-Control-Expose-Headers") != "Location" {
		t.Fatalf("allowed origin headers = %v", r.header)
	}
	// Non-browser clients (no Origin) are unaffected.
	if r := s.do(t, http.MethodGet, "/healthz", "", nil, nil); r.status != http.StatusOK || r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("no-origin request = %d %v", r.status, r.header)
	}

	wild := smallStack(t, stackOpts{cors: []string{"*"}})
	if r := wild.do(t, http.MethodOptions, "/api/campaigns", "", nil, map[string]string{
		"Origin": "http://anything.test", "Access-Control-Request-Method": "POST"}); r.header.Get("Access-Control-Allow-Origin") != "http://anything.test" {
		t.Fatalf("wildcard preflight = %d %v", r.status, r.header)
	}
	if got := api.ParseOrigins(" http://a.test/, ,http://b.test "); strings.Join(got, "|") != "http://a.test|http://b.test" {
		t.Fatalf("ParseOrigins = %q", got)
	}
}

func TestHealthStatuses(t *testing.T) {
	providerDown := func(db *sql.DB, _ *provider.Client) []api.HealthCheck {
		dead := provider.NewClient("http://127.0.0.1:1", nil)
		return []api.HealthCheck{
			{Name: "database", Critical: true, Check: db.PingContext},
			{Name: "provider", Check: dead.Ping},
		}
	}
	s := smallStack(t, stackOpts{health: providerDown})
	r := s.get(t, "/healthz")
	h := decode[map[string]any](t, r)
	if r.status != http.StatusOK || h["status"] != "degraded" {
		t.Fatalf("provider down = %d %s", r.status, r.body)
	}

	dbDown := func(_ *sql.DB, p *provider.Client) []api.HealthCheck {
		return []api.HealthCheck{
			{Name: "database", Critical: true, Check: func(context.Context) error { return errors.New("disk I/O error") }},
			{Name: "provider", Check: p.Ping},
		}
	}
	s = smallStack(t, stackOpts{health: dbDown})
	r = s.get(t, "/healthz")
	if h := decode[map[string]any](t, r); r.status != http.StatusServiceUnavailable || h["status"] != "unhealthy" {
		t.Fatalf("db down = %d %s", r.status, r.body)
	}
}

// panickingSegments makes every segment call panic.
type panickingSegments struct{ api.SegmentService }

func (panickingSegments) Preview(context.Context, string) (*segment.Preview, error) { panic("boom") }

func TestPanicBecomes500(t *testing.T) {
	s := smallStack(t, stackOpts{})
	srv, err := api.New(api.Config{
		Segments: panickingSegments{}, Composer: composer.NewComposer(composer.NewMockMessageLLMClient(), nil),
		Campaigns: s.campaigns, Users: nopUsers{}, Dispatcher: nopDispatcher{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/campaigns/preview-segment", "application/json", strings.NewReader(`{"brief":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("panic = %d", resp.StatusCode)
	}
	if _, err := api.New(api.Config{}); err == nil {
		t.Fatal("missing dependencies accepted")
	}
}

type nopUsers struct{}

func (nopUsers) ForEachByIDs(context.Context, []int64, func(domain.User) error) error { return nil }

type nopDispatcher struct{}

func (nopDispatcher) Start(context.Context, int64, dispatch.AudienceSource) (*dispatch.Run, error) {
	return nil, errors.New("nop")
}
func (nopDispatcher) Resume(context.Context, int64, dispatch.AudienceSource) (*dispatch.Run, error) {
	return nil, errors.New("nop")
}
func (nopDispatcher) Stats(context.Context, int64) (dispatch.Stats, error) {
	return dispatch.Stats{}, nil
}
func (nopDispatcher) ActiveRuns() int { return 0 }
