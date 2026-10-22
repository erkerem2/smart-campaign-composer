package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"smart-campaign-composer/backend/internal/domain"
)

// StatsHandler serves GET .../campaigns/{id}/dispatch: one Stats snapshot.
func (d *Dispatcher) StatsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := campaignID(w, r)
		if !ok {
			return
		}
		s, err := d.Stats(r.Context(), id)
		if err != nil {
			writeStatsError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(s)
	}
}

// StreamHandler serves GET .../campaigns/{id}/dispatch/stream as
// Server-Sent Events: an "event: stats" every interval while the run is
// active, a final snapshot, then "event: done".
func (d *Dispatcher) StreamHandler(interval time.Duration) http.HandlerFunc {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := campaignID(w, r)
		if !ok {
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		first, err := d.Stats(r.Context(), id)
		if err != nil {
			writeStatsError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		send := func(event string, v any) bool {
			b, err := json.Marshal(v)
			if err != nil {
				return false
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
				return false
			}
			flusher.Flush()
			return true
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		s := first
		for {
			if !send("stats", s) {
				return
			}
			if s.Final() {
				send("done", map[string]string{"state": s.State})
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
			if s, err = d.Stats(r.Context(), id); err != nil {
				return
			}
		}
	}
}

func campaignID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid campaign id"}` + "\n"))
		return 0, false
	}
	return id, true
}

func writeStatsError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	if errors.Is(err, domain.ErrNotFound) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"campaign not found"}` + "\n"))
		return
	}
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"error":"internal error"}` + "\n"))
}
