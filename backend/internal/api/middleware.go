package api

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CORSConfig controls which browser origins may call the API.
type CORSConfig struct {
	// AllowedOrigins are exact origins ("http://localhost:5173"). "*"
	// allows any origin. nil means DefaultCORSOrigins; an empty non-nil
	// slice disables cross-origin access.
	AllowedOrigins []string
	// MaxAge lets browsers cache a preflight (default 10 minutes).
	MaxAge time.Duration
}

// DefaultCORSOrigins covers local development: the Vite dev server and
// preview (5173 / 4173, also on 127.0.0.1), and the dockerised frontend
// (8081) and a generic :3000. Production deployments set
// CORS_ALLOWED_ORIGINS, or serve the frontend and /api from one origin.
func DefaultCORSOrigins() []string {
	return []string{
		"http://localhost:5173", "http://127.0.0.1:5173",
		"http://localhost:4173", "http://127.0.0.1:4173",
		"http://localhost:8081", "http://127.0.0.1:8081",
		"http://localhost:3000", "http://127.0.0.1:3000",
	}
}

// ParseOrigins splits a comma-separated CORS_ALLOWED_ORIGINS value,
// trimming spaces and trailing slashes (an Origin header never has one).
func ParseOrigins(s string) []string {
	var out []string
	for _, o := range strings.Split(s, ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			out = append(out, o)
		}
	}
	return out
}

const (
	corsMethods = "GET, POST, OPTIONS"
	corsHeaders = "Content-Type, Accept, Cache-Control, Last-Event-ID, X-Requested-With"
)

// cors answers preflight requests and adds CORS headers for allowed
// origins. The allowed origin is echoed (not "*") with Vary: Origin, so
// caches never serve one origin's response to another. Credentials are not
// used: the API has no cookies.
func cors(cfg CORSConfig, next http.Handler) http.Handler {
	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = 10 * time.Minute
	}
	anyOrigin := slices.Contains(cfg.AllowedOrigins, "*")
	allowed := func(origin string) bool { return anyOrigin || slices.Contains(cfg.AllowedOrigins, origin) }

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" { // same-origin or non-browser client
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
		if !allowed(origin) {
			if preflight {
				writeError(w, &apiError{Status: http.StatusForbidden, Code: "origin_not_allowed",
					Message: "origin " + origin + " is not allowed"})
				return
			}
			// No CORS headers: the browser hides the response from the page.
			next.ServeHTTP(w, r)
			return
		}
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Expose-Headers", "Location")
		if preflight {
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
			h.Set("Access-Control-Allow-Methods", corsMethods)
			h.Set("Access-Control-Allow-Headers", corsHeaders)
			h.Set("Access-Control-Max-Age", strconv.Itoa(int(maxAge/time.Second)))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusWriter records the status code for logging. It keeps streaming
// working: Flush is forwarded (dispatch SSE type-asserts http.Flusher) and
// Unwrap lets http.ResponseController reach the real writer.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func requestLogger(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		level := slog.LevelInfo
		if sw.status >= 500 {
			level = slog.LevelError
		}
		log.Log(r.Context(), level, "http", "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "bytes", sw.bytes, "duration", time.Since(start).Round(time.Microsecond))
	})
}

// recoverer turns a handler panic into a 500 instead of a dropped
// connection, and logs the stack.
func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Error("panic in handler", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				writeError(w, &apiError{Status: http.StatusInternalServerError, Code: "internal", Message: "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
