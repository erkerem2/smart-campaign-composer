package dispatch

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStatsAndStreamHandlers(t *testing.T) {
	env := newTestEnv(t, makeUsers(3, func(int) (bool, bool) { return true, true }))
	d := env.dispatcher(t, newRecordingSender(nil), unitConfig())
	run, err := d.Start(context.Background(), env.campaignID, env.audience())
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, run)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/campaigns/{id}/dispatch", d.StatsHandler())
	mux.HandleFunc("GET /api/campaigns/{id}/dispatch/stream", d.StreamHandler(0))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/campaigns/1/dispatch")
	if err != nil {
		t.Fatal(err)
	}
	var s Stats
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil || resp.StatusCode != 200 {
		t.Fatalf("status %d, %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	if s.Total != 6 || s.Sent != 6 || s.State != StateCompleted || s.Channels["sms"].Sent != 3 {
		t.Fatalf("stats = %+v", s)
	}

	for path, code := range map[string]int{"/api/campaigns/99/dispatch": 404, "/api/campaigns/x/dispatch": 400} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != code {
			t.Errorf("%s = %d, want %d", path, resp.StatusCode, code)
		}
	}

	// A finished run streams one snapshot and a done event.
	resp, err = http.Get(srv.URL + "/api/campaigns/1/dispatch/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if e, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
			events = append(events, e)
		}
	}
	if strings.Join(events, ",") != "stats,done" {
		t.Fatalf("events = %v", events)
	}
}
