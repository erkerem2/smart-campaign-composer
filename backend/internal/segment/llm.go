package segment

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// RuleRequest is everything an LLM backend needs to translate a brief.
// SystemPrompt and Schema are generated from the Registry by the Engine, so
// every backend (real or mock) works against the same contract.
type RuleRequest struct {
	Brief        string
	SystemPrompt string
	ToolName     string
	Schema       map[string]any
}

// LLMClient turns a campaign brief into a candidate rule tree.
//
// The returned JSON is UNTRUSTED: implementations must not attempt to
// validate or "fix" it — the Engine always passes it through Parser.Parse.
type LLMClient interface {
	GenerateRule(ctx context.Context, req RuleRequest) (json.RawMessage, error)
}

// ReferenceBrief is the canonical example brief used in docs and tests.
const ReferenceBrief = "Son 30 günde sepetine ürün ekleyip alışveriş yapmayan İstanbul'daki kullanıcılara özel indirim kampanyası"

// ReferenceRule is the rule tree the reference brief should produce.
// Note the ASCII "Istanbul": the normaliser must map it to "İstanbul".
const ReferenceRule = `{
  "type": "group",
  "logic": "AND",
  "rules": [
    {"type": "condition", "field": "city", "operator": "EQUALS", "value": "Istanbul"},
    {"type": "condition", "field": "cart_last_active_days", "operator": "LESS_THAN_OR_EQUAL", "value": 30},
    {"type": "condition", "field": "has_purchased_since_cart", "operator": "EQUALS", "value": false}
  ]
}`

// MockLLMClient returns canned responses keyed by brief. It lets the whole
// pipeline run in tests and local dev without an Anthropic API key, and lets
// tests simulate a compromised / hallucinating model by registering malicious
// JSON for a brief.
type MockLLMClient struct {
	mu        sync.Mutex
	responses map[string]json.RawMessage
	Err       error // if set, every call fails with it
	Requests  []RuleRequest
}

// NewMockLLMClient is pre-loaded with ReferenceBrief → ReferenceRule.
func NewMockLLMClient() *MockLLMClient {
	m := &MockLLMClient{responses: map[string]json.RawMessage{}}
	m.On(ReferenceBrief, ReferenceRule)
	return m
}

// On registers the raw JSON the mock returns for a brief.
func (m *MockLLMClient) On(brief, rawJSON string) *MockLLMClient {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses[mockKey(brief)] = json.RawMessage(rawJSON)
	return m
}

func (m *MockLLMClient) GenerateRule(_ context.Context, req RuleRequest) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Requests = append(m.Requests, req)
	if m.Err != nil {
		return nil, m.Err
	}
	resp, ok := m.responses[mockKey(req.Brief)]
	if !ok {
		return nil, fmt.Errorf("mock llm: no canned response for brief %q", truncate(req.Brief))
	}
	return append(json.RawMessage(nil), resp...), nil
}

func mockKey(s string) string { return strings.Join(strings.Fields(s), " ") }
