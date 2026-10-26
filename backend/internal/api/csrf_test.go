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
	"sync/atomic"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/api"
	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/dispatch"
	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/segment"
)

// Minimal service stubs: these tests are about what reaches the handlers,
// not about the services behind them.

type csrfSegments struct{}

func (csrfSegments) Preview(context.Context, string) (*segment.Preview, error) {
	return nil, errors.New("unused")
}
func (csrfSegments) PreviewJSON(context.Context, []byte) (*segment.Preview, error) {
	return nil, errors.New("unused")
}
func (csrfSegments) Validate(raw []byte) (*segment.Node, error) {
	return segment.NewParser(segment.DefaultRegistry).Parse(raw)
}
func (csrfSegments) CountRule(context.Context, *segment.Node) (segment.AudienceCounts, error) {
	return segment.AudienceCounts{Total: 2, Reachable: 2}, nil
}
func (csrfSegments) MatchingIDs(context.Context, *segment.Node) ([]int64, error) {
	return []int64{1, 2}, nil
}

type csrfComposer struct{}

func (csrfComposer) Stream(context.Context, string) (<-chan composer.MessageChunk, error) {
	return nil, errors.New("unused")
}
func (csrfComposer) Check(m composer.Message) (composer.Message, composer.Report, error) {
	rep, err := composer.DefaultValidator.Validate(m)
	return m, rep, err
}

type csrfCampaigns struct{ created atomic.Int64 }

func (c *csrfCampaigns) CreateApproved(_ context.Context, brief string, rule json.RawMessage, _ []domain.CampaignMessage, now time.Time) (domain.Campaign, error) {
	c.created.Add(1)
	return domain.Campaign{ID: 2, Brief: brief, Status: domain.CampaignStatusApproved, SegmentRule: rule, CreatedAt: now}, nil
}
func (c *csrfCampaigns) GetByID(_ context.Context, id int64) (domain.Campaign, error) {
	return domain.Campaign{ID: id, Brief: "b", Status: domain.CampaignStatusApproved, SegmentRule: json.RawMessage(segment.ReferenceRule)}, nil
}
func (c *csrfCampaigns) Messages(context.Context, int64) ([]domain.CampaignMessage, error) {
	return nil, nil
}

type csrfUsers struct{}

func (csrfUsers) ForEachByIDs(context.Context, []int64, func(domain.User) error) error { return nil }

type csrfDispatcher struct{ starts atomic.Int64 }

func (d *csrfDispatcher) Start(context.Context, int64, dispatch.AudienceSource) (*dispatch.Run, error) {
	d.starts.Add(1)
	return nil, nil
}
func (d *csrfDispatcher) Resume(context.Context, int64, dispatch.AudienceSource) (*dispatch.Run, error) {
	d.starts.Add(1)
	return nil, nil
}
func (d *csrfDispatcher) Stats(context.Context, int64) (dispatch.Stats, error) {
	return dispatch.Stats{}, nil
}
func (d *csrfDispatcher) ActiveRuns() int { return 0 }

func csrfServer(t *testing.T, origins []string) (*httptest.Server, *csrfDispatcher, *csrfCampaigns) {
	t.Helper()
	disp, camps := &csrfDispatcher{}, &csrfCampaigns{}
	s, err := api.New(api.Config{
		Segments: csrfSegments{}, Composer: csrfComposer{}, Campaigns: camps, Users: csrfUsers{}, Dispatcher: disp,
		CORS:   api.CORSConfig{AllowedOrigins: origins},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, disp, camps
}

func csrfDo(t *testing.T, srv *httptest.Server, method, path, contentType, origin, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// C1. POST /api/campaigns/{id}/dispatch starts sending real messages, and it
// did not read its body at all, so the Content-Type check that protects the
// other POST endpoints did not apply. A page on any site could start a
// campaign with a "simple" cross-site request, which browsers send without a
// CORS preflight:
//
//	<form method="POST" action="http://localhost:8080/api/campaigns/1/dispatch">
//
// (CORS only hides the response from the attacker; the side effect happens.)
func TestDispatchRejectsCrossSiteRequests(t *testing.T) {
	srv, disp, _ := csrfServer(t, []string{"http://app.test"})
	const path = "/api/campaigns/1/dispatch"

	cases := []struct {
		name, contentType, origin, body string
		status                          int
		code                            string
	}{
		{"form post from another site", "application/x-www-form-urlencoded", "http://evil.test", "a=b", http.StatusForbidden, "origin_not_allowed"},
		{"text/plain post from another site", "text/plain", "http://evil.test", "{}", http.StatusForbidden, "origin_not_allowed"},
		{"json from another site", "application/json", "http://evil.test", "{}", http.StatusForbidden, "origin_not_allowed"},
		{"text/plain without origin", "text/plain", "", "{}", http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"form without origin", "application/x-www-form-urlencoded", "", "a=b", http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"no content type", "", "", "", http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"unexpected fields", "application/json", "", `{"campaign_id":2}`, http.StatusBadRequest, "invalid_json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := disp.starts.Load()
			status, body := csrfDo(t, srv, http.MethodPost, path, tc.contentType, tc.origin, tc.body)
			if status != tc.status || !strings.Contains(body, `"code":"`+tc.code+`"`) {
				t.Fatalf("got %d %s, want %d %s", status, body, tc.status, tc.code)
			}
			if disp.starts.Load() != before {
				t.Fatal("dispatch was started by a rejected request")
			}
		})
	}

	// The UI (allowed origin, JSON) and API clients (no Origin, JSON, empty
	// or {} body) still work.
	for _, tc := range []struct{ origin, body string }{{"http://app.test", "{}"}, {"", "{}"}, {"", ""}} {
		before := disp.starts.Load()
		status, body := csrfDo(t, srv, http.MethodPost, path, "application/json", tc.origin, tc.body)
		if status != http.StatusAccepted || disp.starts.Load() != before+1 {
			t.Fatalf("legitimate dispatch (origin %q, body %q) = %d %s", tc.origin, tc.body, status, body)
		}
	}
}

// Every state-changing request from a browser origin that is not allowed is
// refused before it reaches a handler; reads keep working (CORS hides their
// responses) and requests without an Origin header are unaffected.
func TestUnsafeMethodsFromDisallowedOriginsAreRefused(t *testing.T) {
	srv, _, camps := csrfServer(t, []string{"http://app.test"})
	approve := `{"brief":"b","segment_rule":` + segment.ReferenceRule + `,"messages":` + composer.ReferenceMessageJSON + `}`

	if status, body := csrfDo(t, srv, http.MethodPost, "/api/campaigns", "application/json", "http://evil.test", approve); status != http.StatusForbidden {
		t.Fatalf("cross-site approval = %d %s", status, body)
	}
	if camps.created.Load() != 0 {
		t.Fatal("a campaign was created by a cross-site request")
	}
	if status, body := csrfDo(t, srv, http.MethodPost, "/api/campaigns", "application/json", "http://app.test", approve); status != http.StatusCreated {
		t.Fatalf("approval from the app = %d %s", status, body)
	}
	if status, _ := csrfDo(t, srv, http.MethodGet, "/healthz", "", "http://evil.test", ""); status != http.StatusOK {
		t.Fatalf("cross-site GET /healthz = %d, reads are not blocked", status)
	}

	// "*" allows every origin, so only the JSON requirement protects dispatch.
	wild, disp, _ := csrfServer(t, []string{"*"})
	if status, _ := csrfDo(t, wild, http.MethodPost, "/api/campaigns/1/dispatch", "text/plain", "http://evil.test", "{}"); status != http.StatusUnsupportedMediaType || disp.starts.Load() != 0 {
		t.Fatalf("wildcard CORS + text/plain dispatch = %d (starts %d)", status, disp.starts.Load())
	}
}
