package composer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"smart-campaign-composer/backend/internal/textguard"
)

// MaxBriefRunes caps the brief length sent to the LLM, in code points after
// textguard.CleanBrief.
const MaxBriefRunes = textguard.MaxBriefRunes

// maxOutputBytes caps the accumulated tool input of one attempt.
const maxOutputBytes = 256 << 10

// Composer wires LLM → strict decode → Normalize → Validator, with one
// bounded self-repair round by default.
type Composer struct {
	LLM       MessageLLMClient
	Validator *Validator
	// MaxAttempts is the total number of LLM calls per request (default 2:
	// the first try plus one retry that receives the validator's findings).
	MaxAttempts int
	// Timeout bounds the whole request, retries included (default 60s).
	Timeout time.Duration

	systemPrompt string
	schema       map[string]any
}

// NewComposer returns a Composer. A nil validator means DefaultValidator.
func NewComposer(llm MessageLLMClient, v *Validator) *Composer {
	if v == nil {
		v = DefaultValidator
	}
	return &Composer{
		LLM:          llm,
		Validator:    v,
		MaxAttempts:  2,
		Timeout:      60 * time.Second,
		systemPrompt: v.SystemPrompt(),
		schema:       v.ToolSchema(),
	}
}

// Generate produces a validated draft in one round-trip (no streaming).
func (c *Composer) Generate(ctx context.Context, brief string) (*Draft, error) {
	brief, err := checkBrief(brief)
	if err != nil {
		return nil, err
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	var (
		lastMsg *Message
		lastErr error
		req     = c.request(brief)
	)
	for attempt := 1; attempt <= c.attempts(); attempt++ {
		raw, err := c.LLM.GenerateMessages(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrLLM, err)
		}
		draft, msg, err := c.finalize(raw, attempt)
		if err == nil {
			return draft, nil
		}
		lastMsg, lastErr = msg, err
		req = c.retryRequest(brief, raw, err)
	}
	return nil, &RejectedError{Attempts: c.attempts(), Last: lastMsg, Err: lastErr}
}

// Check normalises and validates content edited by the marketer, with the
// same rules as LLM output.
func (c *Composer) Check(m Message) (Message, Report, error) {
	m = Normalize(m)
	rep, err := c.Validator.Validate(m)
	return m, rep, err
}

// ChunkType tags a MessageChunk.
type ChunkType string

const (
	// ChunkDelta carries new text for one field. Display only: it is NOT
	// validated and must be replaced by the Draft from ChunkDone.
	ChunkDelta ChunkType = "delta"
	// ChunkRetry means the attempt was rejected (Issues says why); the UI
	// should clear the streamed text, a new attempt follows.
	ChunkRetry ChunkType = "retry"
	// ChunkDone carries the validated Draft. Always the last chunk on success.
	ChunkDone ChunkType = "done"
	// ChunkError ends the stream with a failure. Rejected (if set) is the last
	// attempt, so a human can still edit it.
	ChunkError ChunkType = "error"
)

// MessageChunk is one event of a streaming generation.
type MessageChunk struct {
	Type     ChunkType `json:"type"`
	Attempt  int       `json:"attempt"`
	Field    Field     `json:"field,omitempty"`
	Delta    string    `json:"delta,omitempty"`
	Draft    *Draft    `json:"draft,omitempty"`
	Issues   []Issue   `json:"issues,omitempty"`
	Error    string    `json:"error,omitempty"`
	Rejected *Message  `json:"rejected,omitempty"`
}

// Stream generates content and emits it field by field as the model writes
// it. The returned channel always ends with exactly one ChunkDone or
// ChunkError and is then closed. If ctx is cancelled the stream stops and the
// channel is closed (possibly without a final chunk); callers must cancel ctx
// when they stop reading.
func (c *Composer) Stream(ctx context.Context, brief string) (<-chan MessageChunk, error) {
	brief, err := checkBrief(brief)
	if err != nil {
		return nil, err
	}
	out := make(chan MessageChunk, 8)
	go func() {
		defer close(out)
		work, cancel := c.withTimeout(ctx)
		defer cancel()
		c.stream(ctx, work, brief, out)
	}()
	return out, nil
}

// stream sends on the caller's context (so a timeout of our own still
// reaches the client as an error chunk) and does the LLM work on work, which
// also carries the Composer's Timeout.
func (c *Composer) stream(ctx, work context.Context, brief string, out chan<- MessageChunk) {
	req := c.request(brief)
	fail := func(attempt int, err error, rejected *Message) {
		send(ctx, out, MessageChunk{Type: ChunkError, Attempt: attempt, Error: err.Error(), Issues: IssuesOf(err), Rejected: rejected})
	}
	for attempt := 1; attempt <= c.attempts(); attempt++ {
		raw, err := c.streamAttempt(ctx, work, req, attempt, out)
		if err != nil {
			if ctx.Err() == nil { // the client is still there
				fail(attempt, fmt.Errorf("%w: %v", ErrLLM, err), nil)
			}
			return
		}
		draft, msg, err := c.finalize(raw, attempt)
		if err == nil {
			send(ctx, out, MessageChunk{Type: ChunkDone, Attempt: attempt, Draft: draft})
			return
		}
		if attempt == c.attempts() {
			fail(attempt, &RejectedError{Attempts: attempt, Last: msg, Err: err}, msg)
			return
		}
		if !send(ctx, out, MessageChunk{Type: ChunkRetry, Attempt: attempt, Issues: feedbackIssues(err), Error: err.Error()}) {
			return
		}
		req = c.retryRequest(brief, raw, err)
	}
}

// streamAttempt runs one streaming LLM call, forwarding field deltas, and
// returns the complete raw tool input.
func (c *Composer) streamAttempt(ctx, work context.Context, req MessageRequest, attempt int, out chan<- MessageChunk) ([]byte, error) {
	// A child context lets us stop the producer if we bail out early.
	work, cancel := context.WithCancel(work)
	defer cancel()
	events, err := c.LLM.StreamMessages(work, req)
	if err != nil {
		return nil, err
	}
	var raw bytes.Buffer
	ex := newFieldExtractor()
	for ev := range events {
		if ev.Err != nil {
			return nil, ev.Err
		}
		if raw.Len()+len(ev.PartialJSON) > maxOutputBytes {
			return nil, fmt.Errorf("tool input exceeds %d bytes", maxOutputBytes)
		}
		raw.WriteString(ev.PartialJSON)
		for _, d := range ex.Feed(ev.PartialJSON) {
			if !send(ctx, out, MessageChunk{Type: ChunkDelta, Attempt: attempt, Field: d.Field, Delta: d.Text}) {
				return nil, ctx.Err()
			}
		}
	}
	// A producer that stopped because of cancellation closes its channel
	// without an error event; never treat that as a complete answer.
	if err := work.Err(); err != nil {
		return nil, err
	}
	return raw.Bytes(), nil
}

// finalize is the trust boundary: strict decode, normalise, validate.
func (c *Composer) finalize(raw []byte, attempt int) (*Draft, *Message, error) {
	msg, err := decodeOutput(raw)
	if err != nil {
		return nil, nil, err
	}
	msg = Normalize(msg)
	rep, err := c.Validator.Validate(msg)
	if err != nil {
		return nil, &msg, err
	}
	return &Draft{Message: msg, Report: rep, Attempts: attempt}, &msg, nil
}

// decodeOutput strictly decodes the tool input: a single JSON object whose
// keys are exactly the three fields (no unknown or duplicate keys), each a
// string, with nothing after the object.
func decodeOutput(raw []byte) (Message, error) {
	var m Message
	dec := json.NewDecoder(bytes.NewReader(raw))
	bad := func(format string, args ...any) (Message, error) {
		return Message{}, fmt.Errorf("%w: %s", ErrMalformedOutput, fmt.Sprintf(format, args...))
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return bad("expected a JSON object")
	}
	seen := map[Field]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return bad("%v", err)
		}
		key, _ := tok.(string)
		f := Field(key)
		if !f.valid() {
			return bad("unknown key %q", snippet(key))
		}
		if seen[f] {
			return bad("duplicate key %q", key)
		}
		seen[f] = true
		tok, err = dec.Token()
		if err != nil {
			return bad("%v", err)
		}
		s, ok := tok.(string)
		if !ok {
			return bad("%s must be a string", f)
		}
		m.set(f, s)
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return bad("%v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bad("trailing data after object")
	}
	for _, f := range Fields {
		if !seen[f] {
			return bad("missing key %q", f)
		}
	}
	return m, nil
}

func (c *Composer) request(brief string) MessageRequest {
	return MessageRequest{Brief: brief, SystemPrompt: c.systemPrompt, ToolName: ToolName, Schema: c.schema}
}

func (c *Composer) retryRequest(brief string, raw []byte, err error) MessageRequest {
	req := c.request(brief)
	prev := string(raw)
	if len(prev) > 8<<10 {
		prev = prev[:8<<10] + "…"
	}
	req.PreviousAttempt = strings.ToValidUTF8(prev, "")
	req.Issues = feedbackIssues(err)
	return req
}

// feedbackIssues turns any finalize error into issues for the model.
func feedbackIssues(err error) []Issue {
	if is := IssuesOf(err); len(is) > 0 {
		return is
	}
	return []Issue{{Field: "tool_input", Code: "malformed_output", Message: err.Error(), Kind: ErrMalformedOutput}}
}

func (c *Composer) attempts() int {
	if c.MaxAttempts < 1 {
		return 1
	}
	return c.MaxAttempts
}

func (c *Composer) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.Timeout > 0 {
		return context.WithTimeout(ctx, c.Timeout)
	}
	return context.WithCancel(ctx)
}

func checkBrief(brief string) (string, error) {
	brief = textguard.CleanBrief(brief) // see segment.Engine.Translate
	if brief == "" {
		return "", ErrEmptyBrief
	}
	if textguard.BriefRunes(brief) > MaxBriefRunes {
		return "", ErrBriefTooLong
	}
	return brief, nil
}
