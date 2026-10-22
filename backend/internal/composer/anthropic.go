package composer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// AnthropicMessageClient implements MessageLLMClient with the Claude
// Messages API and a forced tool call, in both blocking and streaming
// (server-sent events, input_json_delta) modes. Plain net/http keeps the
// dependency footprint at zero.
type AnthropicMessageClient struct {
	APIKey    string
	Model     string
	BaseURL   string // default https://api.anthropic.com
	MaxTokens int
	// HTTPClient is used for both modes. Leave its Timeout at 0 for
	// streaming; the Composer bounds every call with a context deadline.
	HTTPClient *http.Client
}

const (
	defaultAnthropicURL   = "https://api.anthropic.com"
	defaultAnthropicModel = "claude-sonnet-5-5"
	anthropicVersion      = "2023-06-01"
	maxLLMResponseBytes   = 1 << 20
)

// NewAnthropicMessageClientFromEnv reads ANTHROPIC_API_KEY and optional
// ANTHROPIC_MODEL.
func NewAnthropicMessageClientFromEnv() (*AnthropicMessageClient, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, errors.New("ANTHROPIC_API_KEY is not set")
	}
	return &AnthropicMessageClient{APIKey: key, Model: os.Getenv("ANTHROPIC_MODEL")}, nil
}

type anthropicRequest struct {
	Model      string             `json:"model"`
	MaxTokens  int                `json:"max_tokens"`
	System     string             `json:"system"`
	Messages   []anthropicMessage `json:"messages"`
	Tools      []anthropicTool    `json:"tools"`
	ToolChoice map[string]string  `json:"tool_choice"`
	Stream     bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type anthropicResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string          `json:"stop_reason"`
	Error      *anthropicError `json:"error"`
}

// GenerateMessages implements MessageLLMClient.
func (c *AnthropicMessageClient) GenerateMessages(ctx context.Context, req MessageRequest) (json.RawMessage, error) {
	resp, err := c.do(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxLLMResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("anthropic: read body: %w", err)
	}
	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}
	if ar.StopReason == "max_tokens" {
		return nil, errors.New("anthropic: output truncated (stop_reason=max_tokens)")
	}
	for _, blk := range ar.Content {
		if blk.Type == "tool_use" && blk.Name == req.ToolName {
			return blk.Input, nil
		}
	}
	return nil, fmt.Errorf("anthropic: no %s tool_use in response (stop_reason=%s)", req.ToolName, ar.StopReason)
}

// sseEvent is the subset of streaming event payloads we use.
type sseEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Error *anthropicError `json:"error"`
}

// StreamMessages implements MessageLLMClient. HTTP-level failures are
// returned synchronously; failures after the stream started arrive as the
// last StreamEvent.
func (c *AnthropicMessageClient) StreamMessages(ctx context.Context, req MessageRequest) (<-chan StreamEvent, error) {
	resp, err := c.do(ctx, req, true)
	if err != nil {
		return nil, err
	}
	out := make(chan StreamEvent)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		if err := readToolStream(resp.Body, req.ToolName, func(partial string) bool {
			return send(ctx, out, StreamEvent{PartialJSON: partial})
		}); err != nil && ctx.Err() == nil {
			send(ctx, out, StreamEvent{Err: err})
		}
	}()
	return out, nil
}

var errStreamStopped = errors.New("stream consumer stopped")

// readToolStream parses the SSE body and calls emit with every
// input_json_delta of the requested tool_use block.
func readToolStream(body io.Reader, toolName string, emit func(string) bool) error {
	r := bufio.NewReader(io.LimitReader(body, 8*maxLLMResponseBytes))
	var (
		data      bytes.Buffer
		toolIndex = -1
		sawTool   bool
		stop      string
	)
	handle := func() error {
		if data.Len() == 0 {
			return nil
		}
		var ev sseEvent
		err := json.Unmarshal(data.Bytes(), &ev)
		data.Reset()
		if err != nil {
			return fmt.Errorf("anthropic: bad stream event: %w", err)
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" && ev.ContentBlock.Name == toolName {
				toolIndex, sawTool = ev.Index, true
			}
		case "content_block_delta":
			if ev.Index == toolIndex && ev.Delta != nil && ev.Delta.Type == "input_json_delta" && ev.Delta.PartialJSON != "" {
				if !emit(ev.Delta.PartialJSON) {
					return errStreamStopped
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stop = ev.Delta.StopReason
			}
		case "message_stop":
			return io.EOF
		case "error":
			msg := "unknown error"
			if ev.Error != nil {
				msg = ev.Error.Type + ": " + ev.Error.Message
			}
			return fmt.Errorf("anthropic: stream error: %s", msg)
		}
		return nil
	}
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if herr := handle(); herr != nil {
				if errors.Is(herr, io.EOF) {
					return finishStream(sawTool, stop, toolName)
				}
				if errors.Is(herr, errStreamStopped) {
					return nil
				}
				return herr
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		// "event:" lines are redundant with the payload's "type"; ":" lines
		// are comments.
		if err != nil {
			if errors.Is(err, io.EOF) {
				if herr := handle(); herr != nil && !errors.Is(herr, io.EOF) {
					return herr
				}
				return errors.New("anthropic: stream ended before message_stop")
			}
			return fmt.Errorf("anthropic: read stream: %w", err)
		}
	}
}

func finishStream(sawTool bool, stop, toolName string) error {
	if stop == "max_tokens" {
		return errors.New("anthropic: output truncated (stop_reason=max_tokens)")
	}
	if !sawTool {
		return fmt.Errorf("anthropic: no %s tool_use in stream (stop_reason=%s)", toolName, stop)
	}
	return nil
}

// do sends the request and returns the response for a 200, or an error that
// never contains the API key.
func (c *AnthropicMessageClient) do(ctx context.Context, req MessageRequest, stream bool) (*http.Response, error) {
	base, model, maxTok, hc := c.BaseURL, c.Model, c.MaxTokens, c.HTTPClient
	if base == "" {
		base = defaultAnthropicURL
	}
	if model == "" {
		model = defaultAnthropicModel
	}
	if maxTok == 0 {
		maxTok = 2048
	}
	if hc == nil {
		hc = &http.Client{}
		if !stream {
			hc.Timeout = 60 * time.Second
		}
	}
	body, err := json.Marshal(anthropicRequest{
		Model:     model,
		MaxTokens: maxTok,
		System:    req.SystemPrompt,
		Messages:  []anthropicMessage{{Role: "user", Content: UserPrompt(req)}},
		Tools: []anthropicTool{{
			Name:        req.ToolName,
			Description: "Kampanya brief'i için doğrulanacak SMS ve e-posta içeriğini döndürür.",
			InputSchema: req.Schema,
		}},
		ToolChoice: map[string]string{"type": "tool", "name": req.ToolName},
		Stream:     stream,
	})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	if stream {
		httpReq.Header.Set("accept", "text/event-stream")
	}
	resp, err := hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxLLMResponseBytes))
	var ar anthropicResponse
	msg := http.StatusText(resp.StatusCode)
	if json.Unmarshal(raw, &ar) == nil && ar.Error != nil {
		msg = ar.Error.Type + ": " + ar.Error.Message
	}
	return nil, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, msg)
}
