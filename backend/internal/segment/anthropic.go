package segment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"smart-campaign-composer/backend/internal/fence"
)

// AnthropicClient implements LLMClient with the Claude Messages API using a
// forced tool call, so the model's output is a structured tool input rather
// than free text. Plain net/http keeps the dependency footprint at zero.
type AnthropicClient struct {
	APIKey     string
	Model      string
	BaseURL    string // default https://api.anthropic.com
	MaxTokens  int
	HTTPClient *http.Client
}

const (
	defaultAnthropicURL   = "https://api.anthropic.com"
	defaultAnthropicModel = "claude-sonnet-5-5"
	anthropicVersion      = "2023-06-01"
	maxLLMResponseBytes   = 1 << 20
)

// NewAnthropicClientFromEnv reads ANTHROPIC_API_KEY and optional
// ANTHROPIC_MODEL.
func NewAnthropicClientFromEnv() (*AnthropicClient, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, errors.New("ANTHROPIC_API_KEY is not set")
	}
	return &AnthropicClient{APIKey: key, Model: os.Getenv("ANTHROPIC_MODEL")}, nil
}

type anthropicRequest struct {
	Model      string             `json:"model"`
	MaxTokens  int                `json:"max_tokens"`
	System     string             `json:"system"`
	Messages   []anthropicMessage `json:"messages"`
	Tools      []anthropicTool    `json:"tools"`
	ToolChoice map[string]string  `json:"tool_choice"`
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

type anthropicResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *AnthropicClient) GenerateRule(ctx context.Context, req RuleRequest) (json.RawMessage, error) {
	base, model, maxTok, hc := c.BaseURL, c.Model, c.MaxTokens, c.HTTPClient
	if base == "" {
		base = defaultAnthropicURL
	}
	if model == "" {
		model = defaultAnthropicModel
	}
	if maxTok == 0 {
		maxTok = 1024
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}

	body, err := json.Marshal(anthropicRequest{
		Model:     model,
		MaxTokens: maxTok,
		System:    req.SystemPrompt,
		// The brief is fenced so the model treats it as data (see prompt);
		// fence.Wrap escapes any fence tag inside it, so it cannot break out.
		Messages: []anthropicMessage{{Role: "user", Content: fence.Wrap("brief", req.Brief)}},
		Tools: []anthropicTool{{
			Name:        req.ToolName,
			Description: "Kampanya brief'ini doğrulanacak bir kitle kural ağacı olarak döndürür.",
			InputSchema: req.Schema,
		}},
		ToolChoice: map[string]string{"type": "tool", "name": req.ToolName},
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

	resp, err := hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxLLMResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("anthropic: read body: %w", err)
	}

	var ar anthropicResponse
	_ = json.Unmarshal(raw, &ar)
	if resp.StatusCode != http.StatusOK {
		msg := http.StatusText(resp.StatusCode)
		if ar.Error != nil {
			msg = ar.Error.Type + ": " + ar.Error.Message
		}
		return nil, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, msg)
	}
	for _, blk := range ar.Content {
		if blk.Type == "tool_use" && blk.Name == req.ToolName {
			return blk.Input, nil
		}
	}
	return nil, fmt.Errorf("anthropic: no %s tool_use in response (stop_reason=%s)", req.ToolName, ar.StopReason)
}
