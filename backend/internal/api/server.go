package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/dispatch"
	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/segment"
)

// SegmentService turns briefs into validated rules and audiences
// (*segment.Engine).
type SegmentService interface {
	Preview(ctx context.Context, brief string) (*segment.Preview, error)
	PreviewJSON(ctx context.Context, rawRule []byte) (*segment.Preview, error)
	Validate(rawRule []byte) (*segment.Node, error)
	CountRule(ctx context.Context, rule *segment.Node) (segment.AudienceCounts, error)
	MatchingIDs(ctx context.Context, rule *segment.Node) ([]int64, error)
}

// ComposerService writes and validates campaign content (*composer.Composer).
type ComposerService interface {
	Stream(ctx context.Context, brief string) (<-chan composer.MessageChunk, error)
	Check(m composer.Message) (composer.Message, composer.Report, error)
}

// CampaignStore persists campaigns (*repository.CampaignRepository).
type CampaignStore interface {
	CreateApproved(ctx context.Context, brief string, rule json.RawMessage, msgs []domain.CampaignMessage, now time.Time) (domain.Campaign, error)
	GetByID(ctx context.Context, id int64) (domain.Campaign, error)
	Messages(ctx context.Context, campaignID int64) ([]domain.CampaignMessage, error)
}

// DispatchService delivers approved campaigns (*dispatch.Dispatcher).
type DispatchService interface {
	Start(ctx context.Context, campaignID int64, audience dispatch.AudienceSource) (*dispatch.Run, error)
	Resume(ctx context.Context, campaignID int64, audience dispatch.AudienceSource) (*dispatch.Run, error)
	Stats(ctx context.Context, campaignID int64) (dispatch.Stats, error)
	ActiveRuns() int
}

// MetricsStore aggregates system-wide totals for GET /metrics
// (*repository.MetricsRepository).
type MetricsStore interface {
	Totals(ctx context.Context) (domain.SystemTotals, error)
}

// HealthCheck is one dependency probe of GET /healthz.
type HealthCheck struct {
	Name string
	// Critical checks make /healthz answer 503 when they fail (the
	// database); non-critical ones only mark the status "degraded" (the
	// provider: dispatch retries through its outages).
	Critical bool
	Check    func(ctx context.Context) error
}

// Config wires a Server. Segments, Composer, Campaigns, Users and Dispatcher
// are required.
type Config struct {
	Segments   SegmentService
	Composer   ComposerService
	Campaigns  CampaignStore
	Users      dispatch.UserLoader
	Dispatcher DispatchService
	Health     []HealthCheck
	// Metrics backs GET /metrics. Optional: without it the endpoint answers
	// 503 metrics_unavailable.
	Metrics MetricsStore

	// BaseContext is the server's lifetime context. Dispatch runs outlive
	// the request that starts them, so they use this instead of
	// r.Context(); cancelling it interrupts runs (they stay resumable).
	BaseContext context.Context

	CORS CORSConfig
	// StreamInterval is the tick of the dispatch SSE stream (default 500ms).
	StreamInterval time.Duration
	// LLMMode ("anthropic" or "mock") and Version are reported by /healthz.
	LLMMode string
	Version string

	Logger *slog.Logger
	Now    func() time.Time
}

// Server is the HTTP API.
type Server struct {
	cfg     Config
	log     *slog.Logger
	now     func() time.Time
	started time.Time
	handler http.Handler
}

// New validates cfg and builds the routes.
func New(cfg Config) (*Server, error) {
	if cfg.Segments == nil || cfg.Composer == nil || cfg.Campaigns == nil || cfg.Users == nil || cfg.Dispatcher == nil {
		return nil, errors.New("api: segments, composer, campaigns, users and dispatcher are required")
	}
	if cfg.BaseContext == nil {
		cfg.BaseContext = context.Background()
	}
	if cfg.StreamInterval <= 0 {
		cfg.StreamInterval = 500 * time.Millisecond
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.CORS.AllowedOrigins == nil {
		cfg.CORS.AllowedOrigins = DefaultCORSOrigins()
	}
	s := &Server{cfg: cfg, log: cfg.Logger, now: cfg.Now, started: cfg.Now()}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("POST /api/campaigns/preview-segment", s.handlePreviewSegment)
	mux.HandleFunc("POST /api/campaigns/compose/stream", s.handleComposeStream)
	mux.HandleFunc("POST /api/campaigns", s.handleCreateCampaign)
	mux.HandleFunc("GET /api/campaigns/{id}", s.handleGetCampaign)
	mux.HandleFunc("POST /api/campaigns/{id}/dispatch", s.handleDispatch)
	mux.HandleFunc("GET /api/campaigns/{id}/dispatch", s.handleDispatchStats)
	mux.HandleFunc("GET /api/campaigns/{id}/dispatch/stream", s.handleDispatchStream)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, &apiError{Status: http.StatusNotFound, Code: "not_found", Message: "no such endpoint"})
	})

	s.handler = recoverer(s.log, requestLogger(s.log, cors(cfg.CORS, mux)))
	return s, nil
}

// Handler returns the HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler { return s.handler }
