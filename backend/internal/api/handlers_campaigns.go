package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/dispatch"
	"smart-campaign-composer/backend/internal/domain"
	"smart-campaign-composer/backend/internal/segment"
	"smart-campaign-composer/backend/internal/textguard"
)

type createCampaignRequest struct {
	Brief string `json:"brief"`
	// SegmentRule is the rule the marketer reviewed (preview response
	// "rule", possibly edited).
	SegmentRule json.RawMessage `json:"segment_rule"`
	// Messages is the content the marketer reviewed (compose "done"
	// draft.message, possibly edited).
	Messages *composer.Message `json:"messages"`
}

type campaignResponse struct {
	Campaign domain.Campaign          `json:"campaign"`
	Messages []domain.CampaignMessage `json:"messages"`
	Report   *composer.Report         `json:"report,omitempty"`
	Audience *segment.AudienceCounts  `json:"audience,omitempty"`
	Dispatch *dispatch.Stats          `json:"dispatch,omitempty"`
}

// handleCreateCampaign serves POST /api/campaigns: the marketer's approval.
//
// The rule and the content come from the browser, so both are validated
// again exactly like LLM output (segment parser, composer validator) before
// the campaign is stored with status "approved" in one transaction. The
// stored rule is the canonical form returned by the parser, not the bytes
// the client sent. An audience nobody can be reached in (no opt-ins) is
// refused: there would be nothing to send.
func (s *Server) handleCreateCampaign(w http.ResponseWriter, r *http.Request) {
	var req createCampaignRequest
	if e := decodeJSON(w, r, &req); e != nil {
		writeError(w, e)
		return
	}
	// The stored brief is cleaned like the one sent to the models: no
	// invisible characters, trimmed; one made only of those is empty.
	brief := textguard.CleanBrief(req.Brief)
	switch {
	case brief == "":
		writeError(w, badRequest("invalid_brief", "brief is required"))
		return
	case textguard.BriefRunes(brief) > segment.MaxBriefRunes:
		writeError(w, badRequest("invalid_brief", fmt.Sprintf("brief exceeds %d characters", segment.MaxBriefRunes)))
		return
	case len(req.SegmentRule) == 0 || string(req.SegmentRule) == "null":
		writeError(w, badRequest("missing_rule", "segment_rule is required"))
		return
	case req.Messages == nil:
		writeError(w, badRequest("missing_messages", "messages is required"))
		return
	}

	rule, err := s.cfg.Segments.Validate(req.SegmentRule)
	if err != nil {
		writeError(w, s.segmentError(r, err))
		return
	}
	canonical, err := json.Marshal(rule)
	if err != nil {
		writeError(w, s.internal(r, err))
		return
	}
	msg, report, err := s.cfg.Composer.Check(*req.Messages)
	if err != nil {
		writeError(w, &apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_content",
			Message: "content breaks channel rules", Issues: composer.IssuesOf(err)})
		return
	}
	audience, err := s.cfg.Segments.CountRule(r.Context(), rule)
	if err != nil {
		writeError(w, s.internal(r, err))
		return
	}
	if audience.Reachable == 0 {
		writeError(w, &apiError{Status: http.StatusUnprocessableEntity, Code: "empty_audience",
			Message: "no user matching this rule has opted in to SMS or e-mail"})
		return
	}

	c, err := s.cfg.Campaigns.CreateApproved(r.Context(), brief, canonical, msg.CampaignMessages(0), s.now())
	if err != nil {
		writeError(w, s.internal(r, err))
		return
	}
	msgs, err := s.cfg.Campaigns.Messages(r.Context(), c.ID)
	if err != nil {
		writeError(w, s.internal(r, err))
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/api/campaigns/%d", c.ID))
	writeJSON(w, http.StatusCreated, campaignResponse{Campaign: c, Messages: msgs, Report: &report, Audience: &audience})
}

// handleGetCampaign serves GET /api/campaigns/{id}: the campaign, its
// approved messages and its dispatch stats.
func (s *Server) handleGetCampaign(w http.ResponseWriter, r *http.Request) {
	id, e := pathID(r)
	if e != nil {
		writeError(w, e)
		return
	}
	c, err := s.cfg.Campaigns.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, s.lookupError(r, err))
		return
	}
	msgs, err := s.cfg.Campaigns.Messages(r.Context(), id)
	if err != nil {
		writeError(w, s.internal(r, err))
		return
	}
	stats, err := s.cfg.Dispatcher.Stats(r.Context(), id)
	if err != nil {
		writeError(w, s.lookupError(r, err))
		return
	}
	writeJSON(w, http.StatusOK, campaignResponse{Campaign: c, Messages: msgs, Dispatch: &stats})
}

func (s *Server) lookupError(r *http.Request, err error) *apiError {
	if errors.Is(err, domain.ErrNotFound) {
		return &apiError{Status: http.StatusNotFound, Code: "campaign_not_found", Message: "campaign not found"}
	}
	return s.internal(r, err)
}

func (s *Server) internal(r *http.Request, err error) *apiError {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	return &apiError{Status: http.StatusInternalServerError, Code: "internal", Message: "internal error"}
}
