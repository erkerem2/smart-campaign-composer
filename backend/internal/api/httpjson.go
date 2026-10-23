package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"

	"smart-campaign-composer/backend/internal/composer"
)

// maxBodyBytes bounds every JSON request. The largest legitimate body is an
// approval: a rule (≤ 16 KB, segment limits) plus an e-mail body.
const maxBodyBytes = 256 << 10

// apiError is the single error shape of the API:
//
//	{"error": {"code": "invalid_rule", "message": "...", "path": "$.rules[0].field"}}
//
// code is stable and meant for the UI; message is for humans.
type apiError struct {
	Status  int              `json:"-"`
	Code    string           `json:"code"`
	Message string           `json:"message"`
	Path    string           `json:"path,omitempty"`   // rule validation: where in the tree
	Issues  []composer.Issue `json:"issues,omitempty"` // content validation: every finding
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func badRequest(code, msg string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: code, Message: msg}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.Status, map[string]*apiError{"error": e})
}

// decodeJSON reads exactly one JSON object into dst. It requires an
// application/json content type (a cross-origin form post cannot send one
// without a CORS preflight), limits the size and rejects unknown fields and
// trailing data, so typos in the frontend fail loudly instead of being
// silently ignored.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) *apiError {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return &apiError{Status: http.StatusUnsupportedMediaType, Code: "unsupported_media_type",
			Message: "request body must be application/json"}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return &apiError{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large",
				Message: fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes)}
		}
		if errors.Is(err, io.EOF) {
			return badRequest("invalid_json", "request body is empty")
		}
		return badRequest("invalid_json", "malformed JSON: "+err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return badRequest("invalid_json", "request body must contain a single JSON object")
	}
	return nil
}

func pathID(r *http.Request) (int64, *apiError) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, badRequest("invalid_id", "campaign id must be a positive integer")
	}
	return id, nil
}
