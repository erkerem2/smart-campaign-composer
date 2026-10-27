package api

import (
	"net/http"
	"time"
)

// metricsResponse is the body of GET /metrics.
type metricsResponse struct {
	UptimeSeconds float64 `json:"uptime_seconds"`
	// TotalCampaigns counts every campaign in the database, in any status.
	TotalCampaigns int64 `json:"total_campaigns"`
	// TotalSent / TotalFailed count deliveries (one per campaign, user and
	// channel) that reached a final state, over all campaigns.
	TotalSent   int64 `json:"total_sent"`
	TotalFailed int64 `json:"total_failed"`
	// TotalRetried counts retry attempts recorded on finished deliveries.
	TotalRetried int64 `json:"total_retried"`
	// ActiveDispatches is the number of runs live in this process now.
	ActiveDispatches int `json:"active_dispatches"`
}

// handleMetrics serves GET /metrics: global system numbers for dashboards
// and monitoring. The totals come from the database (durable across
// restarts, see MetricsStore); uptime and active dispatches describe this
// process. Unlike /healthz it never probes dependencies.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Metrics == nil {
		writeError(w, &apiError{Status: http.StatusServiceUnavailable, Code: "metrics_unavailable",
			Message: "metrics are not configured on this server"})
		return
	}
	totals, err := s.cfg.Metrics.Totals(r.Context())
	if err != nil {
		writeError(w, s.internal(r, err))
		return
	}
	writeJSON(w, http.StatusOK, metricsResponse{
		UptimeSeconds:    s.now().Sub(s.started).Round(time.Second).Seconds(),
		TotalCampaigns:   totals.Campaigns,
		TotalSent:        totals.Sent,
		TotalFailed:      totals.Failed,
		TotalRetried:     totals.Retries,
		ActiveDispatches: s.cfg.Dispatcher.ActiveRuns(),
	})
}
