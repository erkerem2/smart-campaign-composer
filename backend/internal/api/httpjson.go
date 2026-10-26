package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

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
// without a CORS preflight), limits the size, and is strict about the rest:
// the body must be valid UTF-8 without a byte order mark, be a single
// object (not null, an array or a scalar) with nothing after it, and every
// key must be one of dst's JSON names spelled exactly and given once.
// encoding/json alone would accept {"Brief": …} (it matches names
// case-insensitively), keep the last of duplicate keys and silently replace
// invalid UTF-8 with U+FFFD, so what the API validated could differ from
// what the client sent. Typos in the frontend fail loudly instead.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) *apiError {
	return decodeBody(w, r, dst, false)
}

// requireJSONBody is decodeJSON for endpoints that take no parameters in the
// body (POST .../dispatch): the body may be empty or "{}", but the request
// must still declare application/json. That is what makes it a "non-simple"
// request: a browser cannot send it cross-site without a CORS preflight,
// which the API refuses for unknown origins. Without this, any web page could
// start a campaign with a plain HTML form (CSRF).
func requireJSONBody(w http.ResponseWriter, r *http.Request) *apiError {
	var empty struct{}
	return decodeBody(w, r, &empty, true)
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) *apiError {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return &apiError{Status: http.StatusUnsupportedMediaType, Code: "unsupported_media_type",
			Message: "request body must be application/json"}
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return &apiError{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large",
				Message: fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes)}
		}
		return badRequest("invalid_json", "request body could not be read")
	}
	body := bytes.TrimLeft(raw, " \t\r\n")
	switch {
	case len(body) == 0:
		if allowEmpty {
			return nil
		}
		return badRequest("invalid_json", "request body is empty")
	case bytes.HasPrefix(body, []byte("\xef\xbb\xbf")):
		return badRequest("invalid_json", "request body must not start with a byte order mark")
	case !utf8.Valid(raw):
		return badRequest("invalid_json", "request body is not valid UTF-8")
	case body[0] != '{':
		return badRequest("invalid_json", "request body must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badRequest("invalid_json", "malformed JSON: "+err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return badRequest("invalid_json", "request body must contain a single JSON object")
	}
	if err := strictKeys(raw, reflect.TypeOf(dst)); err != nil {
		return badRequest("invalid_json", err.Error())
	}
	return nil
}

// strictKeys walks the decoded document again and checks every object that
// maps onto a struct: its keys must be the struct's JSON names, spelled
// exactly, each at most once. Values typed json.RawMessage (a segment rule)
// are skipped here; their own parser is strict in the same way.
func strictKeys(raw []byte, t reflect.Type) error {
	return walkKeys(json.NewDecoder(bytes.NewReader(raw)), t, "$")
}

func walkKeys(dec *json.Decoder, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		var skip json.RawMessage
		return dec.Decode(&skip)
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil { // null
		return nil
	}
	if tok != json.Delim('{') {
		return fmt.Errorf("%s must be an object", path)
	}
	fields := jsonFields(t)
	seen := make(map[string]bool, len(fields))
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := kt.(string)
		ft, ok := fields[key]
		if !ok {
			return fmt.Errorf("%s: unknown key %q (keys are case-sensitive)", path, shortKey(key))
		}
		if seen[key] {
			return fmt.Errorf("%s: duplicate key %q", path, key)
		}
		seen[key] = true
		if err := walkKeys(dec, ft, path+"."+key); err != nil {
			return err
		}
	}
	_, err = dec.Token() // closing '}'
	return err
}

// jsonFields maps a struct's JSON names to field types, as encoding/json
// names them (tag name, or the Go name without a tag; "-" is skipped).
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

func shortKey(k string) string {
	if utf8.RuneCountInString(k) <= 40 {
		return k
	}
	return string([]rune(k)[:40]) + "…"
}

func pathID(r *http.Request) (int64, *apiError) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, badRequest("invalid_id", "campaign id must be a positive integer")
	}
	return id, nil
}
