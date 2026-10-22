// Package provider contains the mock SMS / e-mail provider and the HTTP
// client the dispatcher uses to talk to it (or to any provider exposing the
// same API).
//
// The mock behaves like a flaky real-world gateway so that retry, backoff,
// rate limiting and idempotency are exercised for real:
//
//	POST /v1/sms/send     {"to": "+905...", "body": "..."}
//	POST /v1/email/send   {"to": "a@example.com", "subject": "...", "body": "...", "body_format": "markdown"}
//	GET  /v1/stats        per-channel counters
//	GET  /healthz
//
// Every send request must carry an Idempotency-Key header. Responses:
//
//	202  {"message_id": "...", "status": "queued"}        first acceptance
//	200  {"message_id": "...", "status": "duplicate"}     same key again (Idempotent-Replayed: true)
//	429  Retry-After: 1                                   ~10% of requests
//	500                                                   ~5% of requests
//	400 / 409 / 422                                       malformed / key reused with another payload / invalid
//
// plus a random 10-30 ms latency on every request. The mock never contacts
// anyone: it only records what it accepted.
package provider

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Channel limits the mock enforces, mirroring CASE_SPEC.md.
const (
	SMSMaxRunes          = 160
	EmailSubjectMaxRunes = 80
	maxRequestBytes      = 64 << 10
)

// Config controls the mock's failure injection.
type Config struct {
	RateLimitedRatio float64       // share of requests answered with 429
	ServerErrorRatio float64       // share of requests answered with 500
	MinLatency       time.Duration // simulated network/processing latency
	MaxLatency       time.Duration
	RetryAfter       time.Duration // value of the Retry-After header on 429 (whole seconds)
	// Seed makes failure injection reproducible; 0 picks a random seed.
	Seed uint64
	// RecordAttempts keeps every request (time, key, status) in memory for
	// tests and debugging. Leave it off for long-running processes.
	RecordAttempts bool
}

// DefaultConfig is the behaviour required by the case: ~10% 429 with
// Retry-After: 1, ~5% 500, 10-30 ms latency.
func DefaultConfig() Config {
	return Config{
		RateLimitedRatio: 0.10,
		ServerErrorRatio: 0.05,
		MinLatency:       10 * time.Millisecond,
		MaxLatency:       30 * time.Millisecond,
		RetryAfter:       time.Second,
	}
}

// Attempt is one recorded request.
type Attempt struct {
	Channel        string
	IdempotencyKey string
	To             string
	At             time.Time // arrival time, before the simulated latency
	Status         int
}

// ChannelStats are the mock's per-channel counters.
type ChannelStats struct {
	Requests     int64 `json:"requests"`
	Accepted     int64 `json:"accepted"` // distinct messages accepted
	Replayed     int64 `json:"replayed"` // idempotent replays (would have been duplicates)
	RateLimited  int64 `json:"rate_limited"`
	ServerErrors int64 `json:"server_errors"`
	Rejected     int64 `json:"rejected"` // 4xx validation errors
}

type acceptedMessage struct {
	id          string
	payloadHash [32]byte
}

// MockServer is an in-process mock provider. It is safe for concurrent use.
type MockServer struct {
	cfg Config

	mu       sync.Mutex
	rng      *rand.Rand
	seq      int64
	accepted map[string]acceptedMessage // idempotency key -> first result
	stats    map[string]*ChannelStats
	attempts []Attempt
}

// NewMockServer validates cfg and returns a mock provider.
func NewMockServer(cfg Config) (*MockServer, error) {
	switch {
	case cfg.RateLimitedRatio < 0 || cfg.ServerErrorRatio < 0 || cfg.RateLimitedRatio+cfg.ServerErrorRatio > 1:
		return nil, errors.New("provider: failure ratios must be >= 0 and sum to <= 1")
	case cfg.MinLatency < 0 || cfg.MaxLatency < cfg.MinLatency:
		return nil, errors.New("provider: need 0 <= MinLatency <= MaxLatency")
	case cfg.RetryAfter < 0:
		return nil, errors.New("provider: RetryAfter must be >= 0")
	}
	seed := cfg.Seed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}
	return &MockServer{
		cfg:      cfg,
		rng:      rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		accepted: map[string]acceptedMessage{},
		stats:    map[string]*ChannelStats{"sms": {}, "email": {}},
	}, nil
}

// Handler returns the HTTP API of the mock.
func (s *MockServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sms/send", s.send("sms"))
	mux.HandleFunc("POST /v1/email/send", s.send("email"))
	mux.HandleFunc("GET /v1/stats", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.Stats())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// Stats returns a copy of the per-channel counters.
func (s *MockServer) Stats() map[string]ChannelStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]ChannelStats, len(s.stats))
	for ch, st := range s.stats {
		out[ch] = *st
	}
	return out
}

// Attempts returns a copy of the recorded requests (RecordAttempts only), in
// arrival order.
func (s *MockServer) Attempts() []Attempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Attempt(nil), s.attempts...)
}

type sendRequest struct {
	To         string `json:"to"`
	Subject    string `json:"subject"`
	Body       string `json:"body"`
	BodyFormat string `json:"body_format"`
}

var (
	e164Re  = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

func (s *MockServer) send(channel string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		arrived := time.Now()
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		raw, readErr := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))

		// Decide the injected outcome and latency up front, under the lock.
		s.mu.Lock()
		st := s.stats[channel]
		st.Requests++
		roll := s.rng.Float64()
		latency := s.cfg.MinLatency
		if span := s.cfg.MaxLatency - s.cfg.MinLatency; span > 0 {
			latency += time.Duration(s.rng.Int64N(int64(span) + 1))
		}
		s.mu.Unlock()

		status, body, headers := s.decide(channel, key, raw, readErr, roll)
		s.record(Attempt{Channel: channel, IdempotencyKey: key, To: peekTo(raw), At: arrived, Status: status})

		select {
		case <-time.After(latency):
		case <-r.Context().Done():
			return
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		writeJSON(w, status, body)
	}
}

// decide computes the response for one request and updates the counters.
func (s *MockServer) decide(channel, key string, raw []byte, readErr error, roll float64) (int, any, map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats[channel]

	// Injected failures happen before any processing, like an overloaded
	// gateway: nothing is accepted, so a retry is always safe.
	switch {
	case roll < s.cfg.RateLimitedRatio:
		st.RateLimited++
		secs := int((s.cfg.RetryAfter + time.Second - 1) / time.Second)
		return http.StatusTooManyRequests, errBody("rate_limited", "too many requests"),
			map[string]string{"Retry-After": strconv.Itoa(secs)}
	case roll < s.cfg.RateLimitedRatio+s.cfg.ServerErrorRatio:
		st.ServerErrors++
		return http.StatusInternalServerError, errBody("internal_error", "upstream failure"), nil
	}

	if key == "" {
		st.Rejected++
		return http.StatusBadRequest, errBody("missing_idempotency_key", "Idempotency-Key header is required"), nil
	}
	if readErr != nil || len(raw) > maxRequestBytes {
		st.Rejected++
		return http.StatusBadRequest, errBody("bad_request", "unreadable or oversized body"), nil
	}
	var req sendRequest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		st.Rejected++
		return http.StatusBadRequest, errBody("bad_request", "invalid JSON body"), nil
	}
	if msg := validate(channel, req); msg != "" {
		st.Rejected++
		return http.StatusUnprocessableEntity, errBody("invalid_message", msg), nil
	}

	hash := sha256.Sum256(raw)
	if prev, ok := s.accepted[key]; ok {
		if prev.payloadHash != hash {
			st.Rejected++
			return http.StatusConflict, errBody("idempotency_conflict", "key reused with a different payload"), nil
		}
		st.Replayed++
		return http.StatusOK, map[string]string{"message_id": prev.id, "status": "duplicate"},
			map[string]string{"Idempotent-Replayed": "true"}
	}
	s.seq++
	id := fmt.Sprintf("%s_%08d", channel, s.seq)
	s.accepted[key] = acceptedMessage{id: id, payloadHash: hash}
	st.Accepted++
	return http.StatusAccepted, map[string]string{"message_id": id, "status": "queued"}, nil
}

func validate(channel string, r sendRequest) string {
	switch channel {
	case "sms":
		if !e164Re.MatchString(r.To) {
			return "to must be an E.164 phone number"
		}
		if r.Subject != "" || r.BodyFormat != "" {
			return "sms has no subject or body_format"
		}
		if n := utf8.RuneCountInString(r.Body); n == 0 || n > SMSMaxRunes {
			return fmt.Sprintf("sms body must be 1-%d characters, got %d", SMSMaxRunes, n)
		}
	case "email":
		if !emailRe.MatchString(r.To) {
			return "to must be an e-mail address"
		}
		if n := utf8.RuneCountInString(r.Subject); n == 0 || n > EmailSubjectMaxRunes {
			return fmt.Sprintf("subject must be 1-%d characters, got %d", EmailSubjectMaxRunes, n)
		}
		if strings.ContainsAny(r.Subject, "\r\n") {
			return "subject must be a single line"
		}
		if strings.TrimSpace(r.Body) == "" {
			return "body is required"
		}
		if r.BodyFormat != "" && r.BodyFormat != "markdown" && r.BodyFormat != "html" {
			return "body_format must be markdown or html"
		}
	}
	return ""
}

func (s *MockServer) record(a Attempt) {
	if !s.cfg.RecordAttempts {
		return
	}
	s.mu.Lock()
	s.attempts = append(s.attempts, a)
	s.mu.Unlock()
}

// peekTo extracts the recipient for the attempt log without failing on bad
// input.
func peekTo(raw []byte) string {
	var v struct {
		To string `json:"to"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.To
}

func errBody(code, msg string) map[string]string {
	return map[string]string{"error": code, "message": msg}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
