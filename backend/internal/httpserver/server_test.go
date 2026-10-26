package httpserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Production timeouts scaled down 100×: a 300 ms ReadTimeout stands for 30 s.
var testTimeouts = Timeouts{ReadHeader: 100 * time.Millisecond, Read: 300 * time.Millisecond, Idle: time.Second}

func start(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewWithTimeouts("", h, testTimeouts)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// sseHandler reads the whole JSON body (like api.decodeJSON) and then streams
// n events, 100 ms apart, as long as the request context is alive.
func sseHandler(n int, cancelled chan<- bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < n; i++ {
			select {
			case <-r.Context().Done():
				cancelled <- true
				return
			case <-time.After(100 * time.Millisecond):
			}
			fmt.Fprintf(w, "data: %d\n\n", i)
			w.(http.Flusher).Flush()
		}
		cancelled <- false
	}
}

func countEvents(t *testing.T, resp *http.Response) int {
	t.Helper()
	defer resp.Body.Close()
	n := 0
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") {
			n++
		}
	}
	return n
}

// A compose stream (POST with a body) and a dispatch stream (GET) may last
// much longer than ReadTimeout: the request context must stay alive and every
// event must arrive.
func TestStreamsOutliveReadTimeout(t *testing.T) {
	const events = 12 // ≈1.2 s, four times the ReadTimeout
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			cancelled := make(chan bool, 1)
			addr := start(t, sseHandler(events, cancelled))
			var (
				resp *http.Response
				err  error
			)
			if method == http.MethodPost {
				resp, err = http.Post("http://"+addr, "application/json", strings.NewReader(`{"brief":"x"}`))
			} else {
				resp, err = http.Get("http://" + addr)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := countEvents(t, resp); got != events {
				t.Fatalf("received %d/%d events", got, events)
			}
			if <-cancelled {
				t.Fatal("request context was cancelled while streaming")
			}
		})
	}
}

// After ReadTimeout has passed, a client that goes away must still be
// noticed promptly, so an abandoned generation stops calling the model.
func TestDisconnectAfterReadTimeoutIsDetected(t *testing.T) {
	seen := make(chan time.Duration, 1)
	addr := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		begin := time.Now()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select { // waiting for the model, writing nothing
		case <-r.Context().Done():
			seen <- time.Since(begin)
		case <-time.After(3 * time.Second):
			seen <- -1
		}
	}))
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"brief":"x"}`
	fmt.Fprintf(c, "POST / HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	_, _ = c.Read(make([]byte, 512)) // response headers
	time.Sleep(800 * time.Millisecond)
	_ = c.Close()
	if d := <-seen; d < 0 || d > 1300*time.Millisecond {
		t.Fatalf("disconnect at 800 ms noticed after %v (-1: never)", d)
	}
}

// ReadTimeout is kept for what it is for: a client trickling its request
// body is cut off instead of holding a connection and a goroutine forever.
func TestSlowRequestBodyIsCutOff(t *testing.T) {
	readErr := make(chan error, 1)
	addr := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		readErr <- err
	}))
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST / HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("a body that never completes must fail to read")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow body was not cut off by ReadTimeout")
	}
}

func TestDefaultTimeouts(t *testing.T) {
	s := New(":0", http.NotFoundHandler())
	if s.WriteTimeout != 0 {
		t.Fatal("WriteTimeout would cut every SSE stream")
	}
	if s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.IdleTimeout <= 0 {
		t.Fatalf("missing timeouts: %+v", s)
	}
}
