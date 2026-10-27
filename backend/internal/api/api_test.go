package api_test

// End-to-end API test: brief → audience preview → streamed content →
// approval → asynchronous dispatch → live tracking, over real HTTP, real
// SQLite (3,000 seed users through the v_users_segment view), the mock LLM
// and the mock provider with its default chaos (429 + Retry-After, 500,
// latency). Assertions are made on what the client sees AND on what the
// provider received.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/dispatch"
	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/segment"
)

type previewBody struct {
	Brief    string                 `json:"brief"`
	Rule     json.RawMessage        `json:"rule"`
	Audience segment.AudienceCounts `json:"audience"`
}

type campaignBody struct {
	Campaign domain.Campaign          `json:"campaign"`
	Messages []domain.CampaignMessage `json:"messages"`
	Report   *composer.Report         `json:"report"`
	Audience *segment.AudienceCounts  `json:"audience"`
	Dispatch *dispatch.Stats          `json:"dispatch"`
}

type dispatchBody struct {
	CampaignID   int64  `json:"campaign_id"`
	State        string `json:"state"`
	Resumed      bool   `json:"resumed"`
	AudienceSize int    `json:"audience_size"`
	StreamURL    string `json:"stream_url"`
	StatsURL     string `json:"stats_url"`
}

func compact(t *testing.T, raw []byte) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}
	return b.String()
}

func TestEndToEndCampaignFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end dispatch runs at the real 100 msg/s/channel limit")
	}
	s := newStack(t, stackOpts{})

	// --- 0. Health and CORS preflight -------------------------------------
	health := s.get(t, "/healthz")
	if health.status != http.StatusOK {
		t.Fatalf("healthz = %d %s", health.status, health.body)
	}
	h := decode[struct {
		Status string                    `json:"status"`
		Checks map[string]map[string]any `json:"checks"`
		System map[string]any            `json:"system"`
	}](t, health)
	if h.Status != "ok" || h.Checks["database"]["status"] != "ok" || h.Checks["provider"]["status"] != "ok" || h.System["llm_mode"] != "mock" {
		t.Fatalf("health = %s", health.body)
	}

	pre := s.do(t, http.MethodOptions, "/api/campaigns/preview-segment", "", nil, map[string]string{
		"Origin": devOrigin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type",
	})
	if pre.status != http.StatusNoContent || pre.header.Get("Access-Control-Allow-Origin") != devOrigin ||
		!strings.Contains(pre.header.Get("Access-Control-Allow-Methods"), "POST") ||
		!strings.Contains(strings.ToLower(pre.header.Get("Access-Control-Allow-Headers")), "content-type") {
		t.Fatalf("preflight = %d %v", pre.status, pre.header)
	}

	// --- 1. Brief → rule → audience ---------------------------------------
	r := s.postJSON(t, "/api/campaigns/preview-segment", map[string]string{"brief": composer.ReferenceBrief})
	if r.status != http.StatusOK {
		t.Fatalf("preview = %d %s", r.status, r.body)
	}
	if r.header.Get("Access-Control-Allow-Origin") != devOrigin {
		t.Fatalf("CORS header missing on the actual request: %v", r.header)
	}
	preview := decode[previewBody](t, r)
	wantAudience, audienceIDs := referenceAudience(s.users, time.Now())
	if preview.Audience != wantAudience {
		t.Fatalf("preview audience %+v, Go reference %+v", preview.Audience, wantAudience)
	}
	if preview.Audience.Total < 600 || preview.Audience.SMSOptIn == 0 || preview.Audience.EmailOptIn == 0 {
		t.Fatalf("implausible audience on the 3,000-user seed: %+v", preview.Audience)
	}
	if !strings.Contains(string(preview.Rule), `"İstanbul"`) {
		t.Fatalf("rule must be canonical (Istanbul → İstanbul): %s", preview.Rule)
	}
	t.Logf("audience: %+v", preview.Audience)

	// --- 2. Brief → streamed content -------------------------------------
	resp := s.openSSE(t, http.MethodPost, "/api/campaigns/compose/stream", map[string]string{"brief": composer.ReferenceBrief})
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("compose stream = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := readSSE(t, resp.Body, nil)
	streamed := map[composer.Field]string{}
	deltas := 0
	var final composer.MessageChunk
	for i, ev := range events {
		var c composer.MessageChunk
		if err := json.Unmarshal([]byte(ev.Data), &c); err != nil || string(c.Type) != ev.Event {
			t.Fatalf("event %d: %q / %s (%v)", i, ev.Event, ev.Data, err)
		}
		switch c.Type {
		case composer.ChunkDelta:
			deltas++
			streamed[c.Field] += c.Delta
		case composer.ChunkDone:
			if i != len(events)-1 {
				t.Fatal("done must be the last event")
			}
			final = c
		default:
			t.Fatalf("unexpected %s event: %s", c.Type, ev.Data)
		}
	}
	if final.Draft == nil || deltas < 10 {
		t.Fatalf("want many deltas and a done draft; got %d deltas, final %+v", deltas, final)
	}
	draft := final.Draft
	if draft.Message != composer.ReferenceMessage {
		t.Fatalf("draft = %+v", draft.Message)
	}
	for _, f := range composer.Fields {
		if streamed[f] != draft.Message.Get(f) {
			t.Fatalf("%s: streamed text %q != validated %q", f, streamed[f], draft.Message.Get(f))
		}
	}
	if draft.Report.SMS.Runes > domain.SMSMaxLength || draft.Report.EmailSubject.Runes > domain.EmailSubjectMaxLength {
		t.Fatalf("limits not reported/enforced: %+v", draft.Report)
	}

	// --- 3. Approval: invalid requests store nothing ----------------------
	approve := func(rule json.RawMessage, msg composer.Message) response {
		return s.postJSON(t, "/api/campaigns", map[string]any{
			"brief": composer.ReferenceBrief, "segment_rule": rule, "messages": msg,
		})
	}
	tooLong := draft.Message
	tooLong.SMSContent = strings.Repeat("a", domain.SMSMaxLength+1)
	e := expectError(t, approve(preview.Rule, tooLong), http.StatusUnprocessableEntity, "invalid_content")
	if len(e.Error.Issues) == 0 || e.Error.Issues[0].Field != composer.FieldSMS || e.Error.Issues[0].Code != "too_long" {
		t.Fatalf("issues = %+v", e.Error.Issues)
	}
	longSubject := draft.Message
	longSubject.EmailSubject = strings.Repeat("ş", domain.EmailSubjectMaxLength+1)
	expectError(t, approve(preview.Rule, longSubject), http.StatusUnprocessableEntity, "invalid_content")

	smuggled := json.RawMessage(`{"type":"group","logic":"AND","rules":[{"type":"condition","field":"password_hash","operator":"EQUALS","value":"x"}]}`)
	if e := expectError(t, approve(smuggled, draft.Message), http.StatusUnprocessableEntity, "invalid_rule"); e.Error.Path != "$.rules[0].field" {
		t.Fatalf("path = %q", e.Error.Path)
	}
	expectError(t, approve(json.RawMessage(`"SELECT * FROM users"`), draft.Message), http.StatusUnprocessableEntity, "invalid_rule")
	nobody := json.RawMessage(`{"type":"group","logic":"AND","rules":[{"type":"condition","field":"age","operator":"EQUALS","value":120}]}`)
	expectError(t, approve(nobody, draft.Message), http.StatusUnprocessableEntity, "empty_audience")
	expectError(t, s.postJSON(t, "/api/campaigns", map[string]any{"brief": "x", "segment_rule": preview.Rule,
		"messages": draft.Message, "status": "completed"}), http.StatusBadRequest, "invalid_json")
	expectError(t, s.do(t, http.MethodPost, "/api/campaigns", "text/plain", []byte(`{}`), nil),
		http.StatusUnsupportedMediaType, "unsupported_media_type")
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM campaigns").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rejected approvals stored %d campaigns (%v)", n, err)
	}

	// --- 4. Approval ------------------------------------------------------
	r = approve(preview.Rule, draft.Message)
	if r.status != http.StatusCreated {
		t.Fatalf("approve = %d %s", r.status, r.body)
	}
	created := decode[campaignBody](t, r)
	c := created.Campaign
	if c.ID == 0 || c.Status != domain.CampaignStatusApproved || r.header.Get("Location") != fmt.Sprintf("/api/campaigns/%d", c.ID) {
		t.Fatalf("created = %+v, location %q", c, r.header.Get("Location"))
	}
	if compact(t, c.SegmentRule) != compact(t, preview.Rule) {
		t.Fatalf("stored rule %s != previewed %s", c.SegmentRule, preview.Rule)
	}
	if created.Audience == nil || *created.Audience != preview.Audience || len(created.Messages) != 2 {
		t.Fatalf("created = %s", r.body)
	}
	for _, m := range created.Messages {
		if m.Status != domain.MessageStatusApproved {
			t.Fatalf("message not approved: %+v", m)
		}
	}
	got := decode[campaignBody](t, s.get(t, fmt.Sprintf("/api/campaigns/%d", c.ID)))
	if got.Campaign.Status != domain.CampaignStatusApproved || got.Dispatch == nil || got.Dispatch.State != dispatch.StateIdle {
		t.Fatalf("get campaign = %+v", got)
	}

	// --- 5. Dispatch -------------------------------------------------------
	r = s.postJSON(t, fmt.Sprintf("/api/campaigns/%d/dispatch", c.ID), map[string]any{})
	if r.status != http.StatusAccepted {
		t.Fatalf("dispatch = %d %s", r.status, r.body)
	}
	started := decode[dispatchBody](t, r)
	if started.AudienceSize != int(preview.Audience.Total) || started.Resumed ||
		started.StreamURL != fmt.Sprintf("/api/campaigns/%d/dispatch/stream", c.ID) {
		t.Fatalf("dispatch response = %+v", started)
	}
	// A double click cannot start a second run.
	expectError(t, s.postJSON(t, fmt.Sprintf("/api/campaigns/%d/dispatch", c.ID), map[string]any{}),
		http.StatusConflict, "already_running")

	// --- 6. Live tracking --------------------------------------------------
	resp = s.openSSE(t, http.MethodGet, started.StreamURL, nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != devOrigin {
		t.Fatalf("dispatch stream = %d %v", resp.StatusCode, resp.Header)
	}
	var (
		snapshots []dispatch.Stats
		doneState string
	)
	readSSE(t, resp.Body, func(ev sseEvent) bool {
		switch ev.Event {
		case "stats":
			var st dispatch.Stats
			if err := json.Unmarshal([]byte(ev.Data), &st); err != nil {
				t.Fatalf("stats event %s: %v", ev.Data, err)
			}
			snapshots = append(snapshots, st)
		case "done":
			var d map[string]string
			_ = json.Unmarshal([]byte(ev.Data), &d)
			doneState = d["state"]
			return false
		default:
			t.Fatalf("unexpected event %q", ev.Event)
		}
		return true
	})
	if doneState != dispatch.StateCompleted || len(snapshots) < 3 {
		t.Fatalf("stream ended with %q after %d snapshots", doneState, len(snapshots))
	}
	sawProgress := false
	for i, st := range snapshots {
		if i > 0 && st.Processed < snapshots[i-1].Processed {
			t.Fatalf("processed went backwards: %d → %d", snapshots[i-1].Processed, st.Processed)
		}
		if st.Live && st.State == dispatch.StateSending && st.Sent > 0 && st.Processed < st.Total && st.Speed > 0 {
			sawProgress = true
		}
	}
	if !sawProgress {
		t.Fatal("no live in-progress snapshot with a speed was streamed")
	}
	fin := snapshots[len(snapshots)-1]
	t.Logf("final: total=%d sent=%d failed=%d retried=%d speed=%.1f msg/s elapsed=%.1fs", fin.Total, fin.Sent, fin.Failed, fin.Retried, fin.Speed, fin.Elapsed)
	if fin.State != dispatch.StateCompleted || fin.Total != preview.Audience.SMSOptIn+preview.Audience.EmailOptIn ||
		fin.Processed != fin.Total || fin.Sent+fin.Failed != fin.Total || fin.Pending != 0 {
		t.Fatalf("final stats = %+v (audience %+v)", fin, preview.Audience)
	}
	if fin.Channels[domain.ChannelSMS].Total != preview.Audience.SMSOptIn || fin.Channels[domain.ChannelEmail].Total != preview.Audience.EmailOptIn {
		t.Fatalf("per-channel totals must equal the opted-in audience: %+v", fin.Channels)
	}
	if fin.Retried == 0 || fin.Failed > fin.Total/100 {
		t.Fatalf("with ~15%% transient errors expect retries and ~no failures: %+v", fin)
	}
	for ch, cs := range fin.Channels {
		if cs.Speed <= 0 || cs.Speed > 100 {
			t.Fatalf("%s speed %.1f msg/s outside (0, 100]", ch, cs.Speed)
		}
	}

	// --- 7. What the provider received -------------------------------------
	byID := map[int64]domain.User{}
	for _, u := range s.users {
		byID[u.ID] = u
	}
	var accepted int64
	for _, st := range s.provider.Stats() {
		accepted += st.Accepted
	}
	if accepted != fin.Sent {
		t.Fatalf("provider accepted %d distinct messages, API reports %d sent", accepted, fin.Sent)
	}
	acceptedKeys := map[string]bool{}
	for _, a := range s.provider.Attempts() {
		parts := strings.Split(a.IdempotencyKey, ":")
		if len(parts) != 3 || parts[0] != strconv.FormatInt(c.ID, 10) || parts[2] != a.Channel {
			t.Fatalf("bad idempotency key %q", a.IdempotencyKey)
		}
		uid, _ := strconv.ParseInt(parts[1], 10, 64)
		u := byID[uid]
		if !audienceIDs[uid] {
			t.Fatalf("user %d is not in the approved audience but was contacted", uid)
		}
		if !u.CanReceive(domain.Channel(a.Channel)) {
			t.Fatalf("user %d has no %s opt-in but was contacted", uid, a.Channel)
		}
		if a.Status == http.StatusAccepted {
			if acceptedKeys[a.IdempotencyKey] {
				t.Fatalf("%s accepted twice", a.IdempotencyKey)
			}
			acceptedKeys[a.IdempotencyKey] = true
		}
	}

	// --- 8. Final state ----------------------------------------------------
	got = decode[campaignBody](t, s.get(t, fmt.Sprintf("/api/campaigns/%d", c.ID)))
	if got.Campaign.Status != domain.CampaignStatusCompleted || got.Dispatch.Sent != fin.Sent {
		t.Fatalf("after dispatch: %+v / %+v", got.Campaign, got.Dispatch)
	}
	snap := decode[dispatch.Stats](t, s.get(t, started.StatsURL))
	if snap.State != dispatch.StateCompleted || snap.Sent != fin.Sent || snap.Failed != fin.Failed {
		t.Fatalf("snapshot = %+v", snap)
	}
	var logs, pending int
	_ = s.db.QueryRow("SELECT count(*), COALESCE(SUM(status = 'pending'), 0) FROM dispatch_logs WHERE campaign_id = ?", c.ID).Scan(&logs, &pending)
	if int64(logs) != fin.Total || pending != 0 {
		t.Fatalf("dispatch_logs: %d rows, %d pending; want %d, 0", logs, pending, fin.Total)
	}
	expectError(t, s.postJSON(t, fmt.Sprintf("/api/campaigns/%d/dispatch", c.ID), map[string]any{}),
		http.StatusConflict, "invalid_state")

	// --- 9. Global metrics agree with the campaign's final stats ----------
	type metricsBody struct {
		Uptime    float64 `json:"uptime_seconds"`
		Campaigns int64   `json:"total_campaigns"`
		Sent      int64   `json:"total_sent"`
		Failed    int64   `json:"total_failed"`
		Retried   int64   `json:"total_retried"`
	}
	mr := s.get(t, "/metrics")
	if mr.status != http.StatusOK {
		t.Fatalf("GET /metrics = %d %s", mr.status, mr.body)
	}
	m := decode[metricsBody](t, mr)
	if m.Campaigns != 1 || m.Sent != fin.Sent || m.Failed != fin.Failed || m.Retried != fin.Retried || m.Uptime < 0 {
		t.Fatalf("metrics = %+v, want 1 campaign and sent/failed/retried = %d/%d/%d", m, fin.Sent, fin.Failed, fin.Retried)
	}
}
