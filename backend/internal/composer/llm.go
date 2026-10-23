package composer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ToolName is the tool Claude must call to return campaign content.
const ToolName = "emit_campaign_messages"

// MessageRequest is everything an LLM backend needs to write content.
// SystemPrompt and Schema are generated from the Validator's limits and
// variable whitelist, so real and mock backends share one contract.
type MessageRequest struct {
	Brief        string
	SystemPrompt string
	ToolName     string
	Schema       map[string]any
	// Retry context: the previous attempt and why it was rejected. Both are
	// empty on the first attempt.
	PreviousAttempt string
	Issues          []Issue
}

// StreamEvent is one fragment of the tool input as it is generated. The
// concatenation of all PartialJSON values is the complete tool input. A
// non-nil Err is always the last event.
type StreamEvent struct {
	PartialJSON string
	Err         error
}

// MessageLLMClient produces campaign content.
//
// Output is UNTRUSTED: implementations return the model's raw tool input and
// must not validate or "fix" it; the Composer does that.
//
// StreamMessages returns a channel that is closed when generation ends. It
// must stop and close the channel promptly when ctx is cancelled.
type MessageLLMClient interface {
	GenerateMessages(ctx context.Context, req MessageRequest) (json.RawMessage, error)
	StreamMessages(ctx context.Context, req MessageRequest) (<-chan StreamEvent, error)
}

// ReferenceBrief is the sample brief from CASE_SPEC.md.
const ReferenceBrief = "Son 30 günde sepetine ürün ekleyip alışveriş yapmayan İstanbul'daki kullanıcılara %30 indirim hatırlatması gönder. Samimi ama kısa olsun."

// ReferenceMessage is the content the reference brief should produce.
var ReferenceMessage = Message{
	SMSContent:   "{{first_name}}, sepetindeki ürünler seni bekliyor! Sana özel %30 indirimle alışverişini şimdi tamamla: https://example.com/sepet",
	EmailSubject: "{{first_name}}, sepetindekiler %30 indirimle seni bekliyor",
	EmailBody: "Merhaba {{first_name}},\n\n" +
		"Sepetine eklediğin ürünler hâlâ seni bekliyor. Sana özel **%30 indirim** ile alışverişini birkaç tıkla tamamlayabilirsin.\n\n" +
		"[Sepetime git](https://example.com/sepet)\n\n" +
		"İndirim sınırlı bir süre geçerli, kaçırma!\n\n" +
		"Sevgiler,\nExample Mağaza Ekibi",
}

// ReferenceMessageJSON is ReferenceMessage as the model's tool input.
var ReferenceMessageJSON = mustJSON(ReferenceMessage)

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// MockMessageLLMClient returns canned tool inputs keyed by brief, both in one
// piece and as a stream of small fragments (split like Anthropic's
// input_json_delta events, including inside escape sequences). It lets the
// whole composer run in tests and local development without an API key.
type MockMessageLLMClient struct {
	mu        sync.Mutex
	responses map[string][]string
	calls     map[string]int

	// Err, if set, makes every call fail immediately.
	Err error
	// StreamFailAfter, if > 0, makes a stream fail after that many events.
	StreamFailAfter int
	// ChunkBytes is the approximate fragment size (default 6).
	ChunkBytes int
	// ChunkDelay slows the stream down for demos.
	ChunkDelay time.Duration
	// Requests records every request, for assertions.
	Requests []MessageRequest

	fallback []string
}

// NewMockMessageLLMClient is pre-loaded with ReferenceBrief → ReferenceMessage.
func NewMockMessageLLMClient() *MockMessageLLMClient {
	m := &MockMessageLLMClient{responses: map[string][]string{}, calls: map[string]int{}}
	m.On(ReferenceBrief, ReferenceMessageJSON)
	return m
}

// On registers the raw JSON returned for a brief. With several responses,
// the n-th call gets the n-th response and the last one repeats — useful to
// simulate "invalid first, valid after feedback".
func (m *MockMessageLLMClient) On(brief string, rawJSON ...string) *MockMessageLLMClient {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses[mockKey(brief)] = rawJSON
	return m
}

// Default sets the responses used for briefs without canned ones (local
// development without an API key). Tests that need "unknown brief" errors
// simply do not set it.
func (m *MockMessageLLMClient) Default(rawJSON ...string) *MockMessageLLMClient {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fallback = rawJSON
	return m
}

func (m *MockMessageLLMClient) next(req MessageRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Requests = append(m.Requests, req)
	if m.Err != nil {
		return "", m.Err
	}
	key := mockKey(req.Brief)
	resps, ok := m.responses[key]
	if (!ok || len(resps) == 0) && len(m.fallback) > 0 {
		resps, ok = m.fallback, true
	}
	if !ok || len(resps) == 0 {
		return "", fmt.Errorf("mock llm: no canned response for brief %q", snippet(req.Brief))
	}
	n := m.calls[key]
	m.calls[key] = n + 1
	return resps[min(n, len(resps)-1)], nil
}

// GenerateMessages implements MessageLLMClient.
func (m *MockMessageLLMClient) GenerateMessages(ctx context.Context, req MessageRequest) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := m.next(req)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// StreamMessages implements MessageLLMClient.
func (m *MockMessageLLMClient) StreamMessages(ctx context.Context, req MessageRequest) (<-chan StreamEvent, error) {
	raw, err := m.next(req)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	size, delay, failAfter := m.ChunkBytes, m.ChunkDelay, m.StreamFailAfter
	m.mu.Unlock()
	if size <= 0 {
		size = 6
	}
	out := make(chan StreamEvent)
	go func() {
		defer close(out)
		for i, part := range splitUTF8(raw, size) {
			if failAfter > 0 && i == failAfter {
				send(ctx, out, StreamEvent{Err: errors.New("mock llm: stream interrupted")})
				return
			}
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return
				}
			}
			if !send(ctx, out, StreamEvent{PartialJSON: part}) {
				return
			}
		}
	}()
	return out, nil
}

// splitUTF8 cuts s into ~n-byte pieces without splitting a UTF-8 sequence
// (Anthropic deltas are JSON strings, so they are always valid UTF-8). It may
// split inside a JSON escape such as ç, which the extractor must handle.
func splitUTF8(s string, n int) []string {
	var parts []string
	for len(s) > 0 {
		cut := min(n, len(s))
		for cut < len(s) && !utf8.RuneStart(s[cut]) {
			cut++
		}
		parts = append(parts, s[:cut])
		s = s[cut:]
	}
	return parts
}

// send delivers v unless ctx is done first.
func send[T any](ctx context.Context, ch chan<- T, v T) bool {
	select {
	case ch <- v:
		return true
	case <-ctx.Done():
		return false
	}
}

func mockKey(s string) string { return strings.Join(strings.Fields(s), " ") }
