package api_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/api"
	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/dispatch"
	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/provider"
	"smart-campaign-composer/backend/internal/repository"
	"smart-campaign-composer/backend/internal/seed"
	"smart-campaign-composer/backend/internal/segment"
)

const devOrigin = "http://localhost:5173"

// stack is the whole backend wired like cmd/server, with the two external
// systems replaced: a mock LLM (segment + composer) and the in-repo mock
// provider over real loopback HTTP.
type stack struct {
	url       string
	db        *sql.DB
	users     []domain.User
	provider  *provider.MockServer
	segLLM    *segment.MockLLMClient
	msgLLM    *composer.MockMessageLLMClient
	campaigns *repository.CampaignRepository
}

type stackOpts struct {
	users       int // seed size; 0 = seed.DefaultCount (3,000)
	providerCfg *provider.Config
	health      func(db *sql.DB, p *provider.Client) []api.HealthCheck
	cors        []string
}

func newStack(t *testing.T, o stackOpts) *stack {
	t.Helper()
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := repository.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := repository.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if o.users == 0 {
		o.users = seed.DefaultCount
	}
	users, err := seed.Generate(seed.Config{Count: o.users, Seed: seed.DefaultSeed, Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	userRepo := repository.NewUserRepository(db)
	if err := userRepo.InsertBatch(ctx, users); err != nil {
		t.Fatal(err)
	}

	pcfg := provider.DefaultConfig() // ~10% 429 (Retry-After: 1), ~5% 500, 10-30 ms
	if o.providerCfg != nil {
		pcfg = *o.providerCfg
	}
	pcfg.RecordAttempts = true
	mock, err := provider.NewMockServer(pcfg)
	if err != nil {
		t.Fatal(err)
	}
	providerSrv := httptest.NewServer(mock.Handler())
	t.Cleanup(providerSrv.Close)
	providerClient := provider.NewClient(providerSrv.URL, nil)

	campaigns := repository.NewCampaignRepository(db)
	d, err := dispatch.New(repository.NewDispatchRepository(db), campaigns, providerClient,
		dispatch.NewComposerRenderer(nil), dispatch.DefaultConfig(), dispatch.WithLogger(quiet))
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(ctx)
	t.Cleanup(func() { cancel(); d.Wait() }) // runs first: stop runs before the DB closes

	segLLM := segment.NewMockLLMClient().On(composer.ReferenceBrief, segment.ReferenceRule)
	msgLLM := composer.NewMockMessageLLMClient()

	health := []api.HealthCheck{
		{Name: "database", Critical: true, Check: db.PingContext},
		{Name: "provider", Check: providerClient.Ping},
	}
	if o.health != nil {
		health = o.health(db, providerClient)
	}
	srv, err := api.New(api.Config{
		Segments:       segment.NewEngine(segLLM, db, segment.DefaultRegistry, segment.SQLite{}),
		Composer:       composer.NewComposer(msgLLM, nil),
		Campaigns:      campaigns,
		Users:          userRepo,
		Dispatcher:     d,
		Health:         health,
		BaseContext:    base,
		CORS:           api.CORSConfig{AllowedOrigins: o.cors},
		StreamInterval: 100 * time.Millisecond,
		LLMMode:        "mock",
		Logger:         quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	apiSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(apiSrv.Close)

	return &stack{url: apiSrv.URL, db: db, users: users, provider: mock, segLLM: segLLM, msgLLM: msgLLM, campaigns: campaigns}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (s *stack) do(t *testing.T, method, path, contentType string, body []byte, header map[string]string) response {
	t.Helper()
	req, err := http.NewRequest(method, s.url+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: b}
}

func (s *stack) postJSON(t *testing.T, path string, v any) response {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return s.do(t, http.MethodPost, path, "application/json", b, map[string]string{"Origin": devOrigin})
}

func (s *stack) get(t *testing.T, path string) response {
	t.Helper()
	return s.do(t, http.MethodGet, path, "", nil, map[string]string{"Origin": devOrigin})
}

func decode[T any](t *testing.T, r response) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("decode %T from %q: %v", v, r.body, err)
	}
	return v
}

type errorBody struct {
	Error struct {
		Code    string           `json:"code"`
		Message string           `json:"message"`
		Path    string           `json:"path"`
		Issues  []composer.Issue `json:"issues"`
	} `json:"error"`
}

func expectError(t *testing.T, r response, status int, code string) errorBody {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d; body %s", r.status, status, r.body)
	}
	e := decode[errorBody](t, r)
	if e.Error.Code != code {
		t.Fatalf("error code = %q, want %q; body %s", e.Error.Code, code, r.body)
	}
	return e
}

type sseEvent struct {
	Event string
	Data  string
}

// readSSE reads a text/event-stream until the server closes it, calling
// each(event) as events arrive (it may return false to stop early).
func readSSE(t *testing.T, body io.Reader, each func(sseEvent) bool) []sseEvent {
	t.Helper()
	var (
		events []sseEvent
		cur    sseEvent
	)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.Event != "" || cur.Data != "" {
				events = append(events, cur)
				if each != nil && !each(cur) {
					return events
				}
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data += strings.TrimPrefix(line, "data: ")
		}
	}
	return events
}

// openSSE starts a streaming request and returns the response.
func (s *stack) openSSE(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.url+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Origin", devOrigin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// referenceAudience evaluates the reference rule in Go, the way the
// v_users_segment view defines it: İstanbul, cart activity within the last
// 30×24h (days rounded up), and no purchase at or after that cart activity.
func referenceAudience(users []domain.User, now time.Time) (segment.AudienceCounts, map[int64]bool) {
	var c segment.AudienceCounts
	ids := map[int64]bool{}
	for _, u := range users {
		if u.City != seed.CityIstanbul || u.CartLastActiveAt == nil {
			continue
		}
		if days := max(0, (now.Unix()-u.CartLastActiveAt.Unix()+86399)/86400); days > 30 {
			continue
		}
		if u.LastPurchaseAt != nil && !u.LastPurchaseAt.Before(*u.CartLastActiveAt) {
			continue
		}
		ids[u.ID] = true
		c.Total++
		if u.OptInSMS {
			c.SMSOptIn++
		}
		if u.OptInEmail {
			c.EmailOptIn++
		}
		if u.OptInSMS || u.OptInEmail {
			c.Reachable++
		}
	}
	return c, ids
}
