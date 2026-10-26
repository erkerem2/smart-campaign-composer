package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"smart-campaign-composer/backend/internal/dispatch"
	"smart-campaign-composer/backend/internal/domain"
)

type dispatchResponse struct {
	CampaignID int64  `json:"campaign_id"`
	State      string `json:"state"`
	// Resumed is true when an interrupted run (campaign left in "sending",
	// e.g. by a restart) was continued instead of a new one started.
	Resumed bool `json:"resumed"`
	// AudienceSize is the number of users the approved rule matches now,
	// before per-channel opt-in filtering.
	AudienceSize int    `json:"audience_size"`
	StatsURL     string `json:"stats_url"`
	StreamURL    string `json:"stream_url"`
}

// handleDispatch serves POST /api/campaigns/{id}/dispatch. It answers 202 as
// soon as the run is registered; delivery continues in the background on
// the server's lifetime context, not the request's.
//
// The recipients are resolved from the stored rule at this moment (the rule
// is re-validated first: the database is not trusted with SQL either), and
// users are loaded at dispatch time, so opt-in changes since approval are
// honoured. An approved campaign is started; one left in "sending" by an
// interrupted run is resumed without sending anything twice; a campaign with
// an active run, or in any other status, gets 409.
func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	id, e := pathID(r)
	if e != nil {
		writeError(w, e)
		return
	}
	// This request sends real messages: it must not be forgeable from
	// another site (see requireJSONBody; the origin check is in cors).
	if e := requireJSONBody(w, r); e != nil {
		writeError(w, e)
		return
	}
	c, err := s.cfg.Campaigns.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, s.lookupError(r, err))
		return
	}
	resume := false
	switch c.Status {
	case domain.CampaignStatusApproved:
	case domain.CampaignStatusSending:
		resume = true
	default:
		writeError(w, conflict("invalid_state", fmt.Sprintf("campaign is %s; only approved campaigns can be dispatched", c.Status)))
		return
	}
	if len(c.SegmentRule) == 0 {
		writeError(w, conflict("missing_rule", "campaign has no approved segment rule"))
		return
	}
	rule, err := s.cfg.Segments.Validate(c.SegmentRule)
	if err != nil {
		s.log.Error("stored segment rule no longer validates", "campaign_id", id, "err", err)
		writeError(w, conflict("invalid_rule", "the stored segment rule is no longer valid: "+err.Error()))
		return
	}
	ids, err := s.cfg.Segments.MatchingIDs(r.Context(), rule)
	if err != nil {
		writeError(w, s.internal(r, err))
		return
	}
	audience := dispatch.IDAudience(s.cfg.Users, ids)

	start := s.cfg.Dispatcher.Start
	if resume {
		start = s.cfg.Dispatcher.Resume
	}
	if _, err := start(s.cfg.BaseContext, id, audience); err != nil {
		switch {
		case errors.Is(err, dispatch.ErrAlreadyRunning):
			writeError(w, conflict("already_running", "campaign is already being dispatched"))
		case errors.Is(err, dispatch.ErrInvalidState):
			writeError(w, conflict("invalid_state", err.Error()))
		case errors.Is(err, dispatch.ErrNoApprovedMessages):
			writeError(w, conflict("no_approved_messages", "campaign has no approved messages"))
		default:
			writeError(w, s.lookupError(r, err))
		}
		return
	}
	base := fmt.Sprintf("/api/campaigns/%d/dispatch", id)
	w.Header().Set("Location", base)
	writeJSON(w, http.StatusAccepted, dispatchResponse{
		CampaignID: id, State: dispatch.StatePlanning, Resumed: resume, AudienceSize: len(ids),
		StatsURL: base, StreamURL: base + "/stream",
	})
}

func conflict(code, msg string) *apiError {
	return &apiError{Status: http.StatusConflict, Code: code, Message: msg}
}

// handleDispatchStats serves GET /api/campaigns/{id}/dispatch: one snapshot.
func (s *Server) handleDispatchStats(w http.ResponseWriter, r *http.Request) {
	id, e := pathID(r)
	if e != nil {
		writeError(w, e)
		return
	}
	st, err := s.cfg.Dispatcher.Stats(r.Context(), id)
	if err != nil {
		writeError(w, s.lookupError(r, err))
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleDispatchStream serves GET /api/campaigns/{id}/dispatch/stream as
// Server-Sent Events:
//
//	event: stats  data: {"state":"sending","total":1150,"sent":420,"failed":1,"retried":57,"speed":94.8,"channels":{...}}
//	...           (every StreamInterval while the run is live)
//	event: done   data: {"state":"completed"}
//
// A campaign without a live run gets one snapshot from the database and
// "done". EventSource reconnects automatically when a stream ends, so the
// client should close it on "done".
func (s *Server) handleDispatchStream(w http.ResponseWriter, r *http.Request) {
	id, e := pathID(r)
	if e != nil {
		writeError(w, e)
		return
	}
	st, err := s.cfg.Dispatcher.Stats(r.Context(), id)
	if err != nil {
		writeError(w, s.lookupError(r, err))
		return
	}
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	ticker := time.NewTicker(s.cfg.StreamInterval)
	defer ticker.Stop()
	for {
		if !send("stats", st) {
			return
		}
		if st.Final() {
			send("done", map[string]string{"state": st.State})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
		if st, err = s.cfg.Dispatcher.Stats(r.Context(), id); err != nil {
			send("error", map[string]string{"error": "stats unavailable"})
			return
		}
	}
}
