package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/dispatch"
	"smart-campaign-composer/backend/internal/domain"
)

func newMock(t *testing.T, cfg Config) (*MockServer, *Client) {
	t.Helper()
	m, err := NewMockServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	return m, NewClient(srv.URL, srv.Client())
}

// reliable is a mock without injected failures or latency.
func reliable() Config { return Config{RecordAttempts: true} }

func smsMsg(key, to, body string) dispatch.Message {
	return dispatch.Message{IdempotencyKey: key, Channel: domain.ChannelSMS, To: to, Body: body}
}

func emailMsg(key string) dispatch.Message {
	return dispatch.Message{IdempotencyKey: key, Channel: domain.ChannelEmail, To: "a@example.com",
		Subject: "Merhaba", Body: "Selam **Ayşe**", BodyFormat: "markdown"}
}

func TestMockAcceptsAndReplaysByIdempotencyKey(t *testing.T) {
	m, c := newMock(t, reliable())
	ctx := context.Background()

	first, err := c.Send(ctx, smsMsg("1:1:sms", "+905321234567", "Merhaba"))
	if err != nil || first.ProviderMessageID == "" || first.Replayed {
		t.Fatalf("first send = %+v, %v", first, err)
	}
	again, err := c.Send(ctx, smsMsg("1:1:sms", "+905321234567", "Merhaba"))
	if err != nil || again.ProviderMessageID != first.ProviderMessageID || !again.Replayed {
		t.Fatalf("replay = %+v, %v; want same id, Replayed", again, err)
	}
	if _, err := c.Send(ctx, emailMsg("1:1:email")); err != nil {
		t.Fatalf("email: %v", err)
	}
	st := m.Stats()
	if st["sms"].Accepted != 1 || st["sms"].Replayed != 1 || st["email"].Accepted != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// Concurrent duplicates of the same key are accepted exactly once.
func TestMockIdempotencyUnderConcurrency(t *testing.T) {
	m, c := newMock(t, reliable())
	var wg sync.WaitGroup
	ids := make([]string, 20)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.Send(context.Background(), smsMsg("7:7:sms", "+905321234567", "Hi"))
			if err != nil {
				t.Error(err)
			}
			ids[i] = r.ProviderMessageID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("different message ids for one key: %v", ids)
		}
	}
	if st := m.Stats()["sms"]; st.Accepted != 1 || st.Replayed != 19 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestMockValidation(t *testing.T) {
	_, c := newMock(t, reliable())
	ctx := context.Background()
	cases := map[string]struct {
		msg  dispatch.Message
		code int
	}{
		"bad phone":         {smsMsg("k1", "05321234567", "x"), http.StatusUnprocessableEntity},
		"sms too long":      {smsMsg("k2", "+905321234567", strings.Repeat("ş", SMSMaxRunes+1)), http.StatusUnprocessableEntity},
		"empty sms":         {smsMsg("k3", "+905321234567", ""), http.StatusUnprocessableEntity},
		"subject too long":  {func() dispatch.Message { m := emailMsg("k4"); m.Subject = strings.Repeat("a", 81); return m }(), http.StatusUnprocessableEntity},
		"multiline subject": {func() dispatch.Message { m := emailMsg("k5"); m.Subject = "a\r\nBcc: x@y.z"; return m }(), http.StatusUnprocessableEntity},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.Send(ctx, tc.msg)
			var se *dispatch.SendError
			if !errors.As(err, &se) || se.StatusCode != tc.code || se.Temporary {
				t.Fatalf("err = %v, want permanent %d", err, tc.code)
			}
		})
	}
	// 160 characters exactly is allowed (code points, not bytes).
	if _, err := c.Send(ctx, smsMsg("k6", "+905321234567", strings.Repeat("ş", SMSMaxRunes))); err != nil {
		t.Fatalf("160-char SMS rejected: %v", err)
	}
	// Reusing a key with another payload is a conflict, never a second send.
	_, err := c.Send(ctx, smsMsg("k6", "+905321234567", "different"))
	var se *dispatch.SendError
	if !errors.As(err, &se) || se.StatusCode != http.StatusConflict || se.Temporary {
		t.Fatalf("key reuse = %v", err)
	}
}

func TestMockRequiresIdempotencyKey(t *testing.T) {
	m, err := NewMockServer(reliable())
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/sms/send",
		strings.NewReader(`{"to":"+905321234567","body":"x"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

// Over many requests the injected 429 / 500 shares match the configuration,
// every 429 carries Retry-After: 1 and latency stays within 10-30 ms.
func TestMockFailureInjectionAndLatency(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seed = 42
	m, err := NewMockServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := m.Handler()
	const n = 4000
	// Latency is checked on a sample through a real server.
	srv := httptest.NewServer(h)
	defer srv.Close()
	for i := 0; i < 20; i++ {
		start := time.Now()
		resp, err := srv.Client().Post(srv.URL+"/v1/sms/send", "application/json",
			strings.NewReader(`{"to":"+905321234567","body":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if d := time.Since(start); d < cfg.MinLatency {
			t.Fatalf("latency %v below %v", d, cfg.MinLatency)
		}
	}

	// Ratios are checked in-process without latency to keep the test fast.
	fast := cfg
	fast.MinLatency, fast.MaxLatency = 0, 0
	m, err = NewMockServer(fast)
	if err != nil {
		t.Fatal(err)
	}
	h = m.Handler()
	var r429, r500 int
	for i := 0; i < n; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/email/send",
			strings.NewReader(`{"to":"a@example.com","subject":"s","body":"b"}`))
		req.Header.Set("Idempotency-Key", "k"+strconv.Itoa(i))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusTooManyRequests:
			r429++
			if rec.Header().Get("Retry-After") != "1" {
				t.Fatalf("429 without Retry-After: 1 (got %q)", rec.Header().Get("Retry-After"))
			}
		case http.StatusInternalServerError:
			r500++
		}
	}
	// Binomial(4000, p): ±4 standard deviations.
	if r429 < 320 || r429 > 480 {
		t.Errorf("429 count %d, want ~400 (10%%)", r429)
	}
	if r500 < 145 || r500 > 255 {
		t.Errorf("500 count %d, want ~200 (5%%)", r500)
	}
	if st := m.Stats()["email"]; st.RateLimited != int64(r429) || st.ServerErrors != int64(r500) {
		t.Errorf("stats %+v disagree with responses", st)
	}
}

func TestNewMockServerRejectsBadConfig(t *testing.T) {
	for _, cfg := range []Config{
		{RateLimitedRatio: 0.8, ServerErrorRatio: 0.3},
		{RateLimitedRatio: -0.1},
		{MinLatency: 20 * time.Millisecond, MaxLatency: 10 * time.Millisecond},
	} {
		if _, err := NewMockServer(cfg); err == nil {
			t.Errorf("config %+v accepted", cfg)
		}
	}
}

// ---------------------------------------------------------------------------
// Client error mapping (against hand-written handlers).
// ---------------------------------------------------------------------------

func TestClientErrorClassification(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		retryAfter string
		temporary  bool
		wantAfter  time.Duration
	}{
		{"429 with seconds", 429, "1", true, time.Second},
		{"429 without header", 429, "", true, 0},
		{"500", 500, "", true, 0},
		{"503 with retry-after", 503, "3", true, 3 * time.Second},
		{"422", 422, "", false, 0},
		{"409", 409, "", false, 0},
		{"401", 401, "", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"x"}`))
			}))
			defer srv.Close()
			_, err := NewClient(srv.URL, nil).Send(context.Background(), smsMsg("1:2:sms", "+905321234567", "x"))
			var se *dispatch.SendError
			if !errors.As(err, &se) {
				t.Fatalf("err = %T %v", err, err)
			}
			if se.StatusCode != tc.status || se.Temporary != tc.temporary || se.RetryAfter != tc.wantAfter {
				t.Fatalf("got %+v", se)
			}
		})
	}
}

func TestClientTransportErrorIsTemporary(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused from now on
	_, err := NewClient(url, nil).Send(context.Background(), smsMsg("1:2:sms", "+905321234567", "x"))
	var se *dispatch.SendError
	if !errors.As(err, &se) || !se.Temporary || se.StatusCode != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestClientRejectsMissingKeyAndUnknownChannel(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", nil)
	if _, err := c.Send(context.Background(), smsMsg("", "+905321234567", "x")); err == nil {
		t.Fatal("message without idempotency key sent")
	}
	if _, err := c.Send(context.Background(), dispatch.Message{IdempotencyKey: "k", Channel: "push"}); err == nil {
		t.Fatal("unknown channel sent")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"1":                             time.Second,
		" 2 ":                           2 * time.Second,
		"-5":                            0,
		"soon":                          0,
		"Thu, 22 Oct 2026 12:00:05 GMT": 5 * time.Second,
		"Thu, 22 Oct 2026 11:00:00 GMT": 0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}
