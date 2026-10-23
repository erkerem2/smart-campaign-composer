package api

import (
	"context"
	"net/http"
	"runtime"
	"sync"
	"time"
)

type checkResult struct {
	Status    string  `json:"status"` // ok | fail
	Critical  bool    `json:"critical"`
	LatencyMS float64 `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
}

type systemInfo struct {
	UptimeSeconds    float64 `json:"uptime_seconds"`
	GoVersion        string  `json:"go_version"`
	Goroutines       int     `json:"goroutines"`
	ActiveDispatches int     `json:"active_dispatches"`
	LLMMode          string  `json:"llm_mode,omitempty"`
	Version          string  `json:"version,omitempty"`
}

type healthResponse struct {
	// Status is "ok", "degraded" (a non-critical check failed; the API still
	// works) or "unhealthy" (a critical check failed; HTTP 503).
	Status string                 `json:"status"`
	Checks map[string]checkResult `json:"checks"`
	System systemInfo             `json:"system"`
}

// healthTimeout bounds every check; they run in parallel, so /healthz
// answers within this time even when a dependency hangs.
const healthTimeout = 2 * time.Second

// handleHealth serves GET /healthz for load balancers, docker compose and
// the dashboard. Only critical checks (the database) produce 503: dispatch
// retries through provider outages and the composer works without it.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()

	results := make([]checkResult, len(s.cfg.Health))
	var wg sync.WaitGroup
	for i, hc := range s.cfg.Health {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			err := hc.Check(ctx)
			res := checkResult{Status: "ok", Critical: hc.Critical,
				LatencyMS: float64(time.Since(start).Microseconds()) / 1000}
			if err != nil {
				res.Status, res.Error = "fail", err.Error()
			}
			results[i] = res
		}()
	}
	wg.Wait()

	resp := healthResponse{Status: "ok", Checks: make(map[string]checkResult, len(results)), System: systemInfo{
		UptimeSeconds:    s.now().Sub(s.started).Round(time.Second).Seconds(),
		GoVersion:        runtime.Version(),
		Goroutines:       runtime.NumGoroutine(),
		ActiveDispatches: s.cfg.Dispatcher.ActiveRuns(),
		LLMMode:          s.cfg.LLMMode,
		Version:          s.cfg.Version,
	}}
	code := http.StatusOK
	for i, hc := range s.cfg.Health {
		res := results[i]
		resp.Checks[hc.Name] = res
		if res.Status == "ok" {
			continue
		}
		if hc.Critical {
			resp.Status, code = "unhealthy", http.StatusServiceUnavailable
		} else if resp.Status == "ok" {
			resp.Status = "degraded"
		}
	}
	writeJSON(w, code, resp)
}
