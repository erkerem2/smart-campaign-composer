// Package httpserver builds the API's *http.Server, so its timeout policy is
// one tested function instead of literals in cmd/server.
//
// The API serves two long-lived Server-Sent Event streams (content
// generation and live dispatch stats). The policy is:
//
//   - ReadHeaderTimeout and ReadTimeout bound how long a client may take to
//     SEND its request (slowloris on headers or body). They do not limit
//     how long a response may stream: once the request has been read, an
//     SSE response can run past ReadTimeout without its request context
//     being cancelled, and a client disconnect is still noticed. That is
//     net/http behaviour rather than something this package controls, so
//     server_test.go pins it: an SSE stream cut after 30 s would break
//     generation with a real model.
//   - There is no WriteTimeout: it would cut every stream after a fixed
//     time. Streams end when the client disconnects (or the run finishes).
//   - IdleTimeout closes idle keep-alive connections.
package httpserver

import (
	"net/http"
	"time"
)

// Timeouts is the server's timeout policy (see the package comment).
type Timeouts struct {
	ReadHeader time.Duration
	Read       time.Duration
	Idle       time.Duration
}

// DefaultTimeouts are the production values.
var DefaultTimeouts = Timeouts{
	ReadHeader: 5 * time.Second,
	Read:       30 * time.Second,
	Idle:       120 * time.Second,
}

// New returns a server for handler on addr with DefaultTimeouts.
func New(addr string, handler http.Handler) *http.Server {
	return NewWithTimeouts(addr, handler, DefaultTimeouts)
}

// NewWithTimeouts returns a server with an explicit timeout policy (tests
// use short values).
func NewWithTimeouts(addr string, handler http.Handler, t Timeouts) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: t.ReadHeader,
		ReadTimeout:       t.Read,
		IdleTimeout:       t.Idle,
		MaxHeaderBytes:    64 << 10,
		// No WriteTimeout: SSE responses stay open for the whole generation
		// or dispatch run; they end when the client disconnects.
	}
}
