package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"smart-campaign-composer/backend/internal/composer"
	"smart-campaign-composer/backend/internal/segment"
)

type previewRequest struct {
	// Brief is the marketer's natural-language request. The rule is
	// generated from it by the LLM.
	Brief string `json:"brief"`
	// Rule, when set, is a rule edited in the UI: it is validated and
	// counted without an LLM call (Brief is then only echoed back).
	Rule json.RawMessage `json:"rule,omitempty"`
}

// handlePreviewSegment serves POST /api/campaigns/preview-segment:
//
//	{"brief": "..."}            → LLM → validated rule → audience counts
//	{"rule": {...}}             → validated rule → audience counts (no LLM)
//
// Response: {"brief", "rule", "audience": {"total", "sms_opt_in",
// "email_opt_in", "reachable"}}. The returned rule is the canonical,
// normalised form ("Istanbul" → "İstanbul"); the UI shows and approves it.
func (s *Server) handlePreviewSegment(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	if e := decodeJSON(w, r, &req); e != nil {
		writeError(w, e)
		return
	}
	var (
		p   *segment.Preview
		err error
	)
	switch {
	case len(req.Rule) > 0 && string(req.Rule) != "null":
		p, err = s.cfg.Segments.PreviewJSON(r.Context(), req.Rule)
		if p != nil {
			p.Brief = strings.TrimSpace(req.Brief)
		}
	case strings.TrimSpace(req.Brief) != "":
		p, err = s.cfg.Segments.Preview(r.Context(), req.Brief)
	default:
		writeError(w, badRequest("invalid_brief", "brief or rule is required"))
		return
	}
	if err != nil {
		writeError(w, s.segmentError(r, err))
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// segmentError maps segment-engine errors onto HTTP. LLM transport details
// are logged, not returned.
func (s *Server) segmentError(r *http.Request, err error) *apiError {
	var ve *segment.ValidationError
	switch {
	case errors.Is(err, segment.ErrEmptyBrief), errors.Is(err, segment.ErrBriefTooLong):
		return badRequest("invalid_brief", err.Error())
	case errors.Is(err, segment.ErrLLM):
		s.log.Warn("segment llm call failed", "path", r.URL.Path, "err", err)
		return &apiError{Status: http.StatusBadGateway, Code: "llm_unavailable",
			Message: "the segment model could not be reached; try again or edit the rule manually"}
	case errors.As(err, &ve):
		return &apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_rule", Message: ve.Error(), Path: ve.Path}
	case isRuleError(err):
		return &apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_rule", Message: err.Error()}
	}
	s.log.Error("segment request failed", "path", r.URL.Path, "err", err)
	return &apiError{Status: http.StatusInternalServerError, Code: "internal", Message: "internal error"}
}

func isRuleError(err error) bool {
	for _, kind := range []error{
		segment.ErrMalformedJSON, segment.ErrUnknownKey, segment.ErrInvalidNodeType, segment.ErrInvalidLogic,
		segment.ErrUnknownField, segment.ErrUnsupportedOperator, segment.ErrInvalidValue, segment.ErrLimitExceeded,
	} {
		if errors.Is(err, kind) {
			return true
		}
	}
	return false
}

type composeRequest struct {
	Brief string `json:"brief"`
}

// handleComposeStream serves POST /api/campaigns/compose/stream as
// Server-Sent Events (read it with fetch + a stream reader; EventSource
// cannot POST):
//
//	event: delta  data: {"type":"delta","attempt":1,"field":"sms_content","delta":"..."}
//	event: retry  data: {"type":"retry","attempt":1,"issues":[...]}   clear the fields
//	event: done   data: {"type":"done","attempt":1,"draft":{"message":{...},"report":{...}}}
//	event: error  data: {"type":"error","error":"...","issues":[...],"rejected":{...}}
//
// Deltas are display-only; the validated draft in "done" is what the UI must
// show for review and send back on approval. Invalid briefs are rejected
// with a JSON 400 before the stream starts.
func (s *Server) handleComposeStream(w http.ResponseWriter, r *http.Request) {
	var req composeRequest
	if e := decodeJSON(w, r, &req); e != nil {
		writeError(w, e)
		return
	}
	chunks, err := s.cfg.Composer.Stream(r.Context(), req.Brief)
	if err != nil {
		if errors.Is(err, composer.ErrEmptyBrief) || errors.Is(err, composer.ErrBriefTooLong) {
			writeError(w, badRequest("invalid_brief", err.Error()))
			return
		}
		s.log.Error("compose stream failed to start", "err", err)
		writeError(w, &apiError{Status: http.StatusInternalServerError, Code: "internal", Message: "internal error"})
		return
	}
	// Stream stops when the client disconnects (r.Context()).
	if err := composer.WriteSSE(r.Context(), w, chunks); err != nil && r.Context().Err() == nil {
		s.log.Warn("compose stream ended early", "err", err)
	}
}
