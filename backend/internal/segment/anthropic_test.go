package segment

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The real client is exercised against a fake Messages API, so request shape
// and response handling are covered without an API key.
func TestAnthropicClientRequestAndResponse(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing auth/version headers")
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"t1","name":"emit_segment_rule","input":`+ReferenceRule+`}],"stop_reason":"tool_use"}`)
	}))
	defer srv.Close()

	c := &AnthropicClient{APIKey: "test-key", BaseURL: srv.URL}
	reg := DefaultRegistry
	raw, err := c.GenerateRule(context.Background(), RuleRequest{
		Brief: ReferenceBrief, SystemPrompt: reg.SystemPrompt(), ToolName: ToolName, Schema: reg.JSONSchema(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewParser(reg).Parse(raw); err != nil {
		t.Fatalf("tool input should parse: %v", err)
	}

	tc, _ := got["tool_choice"].(map[string]any)
	if tc["type"] != "tool" || tc["name"] != ToolName {
		t.Errorf("tool_choice must force %s, got %v", ToolName, tc)
	}
	msgs, _ := got["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, "<brief>") || !strings.Contains(content, ReferenceBrief) {
		t.Errorf("brief must be fenced in <brief> tags: %q", content)
	}
	if sys, _ := got["system"].(string); !strings.Contains(sys, "cart_last_active_days") {
		t.Errorf("system prompt should list whitelisted fields")
	}
}

func TestAnthropicClientErrors(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"http error":  {401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, "authentication_error"},
		"no tool use": {200, `{"content":[{"type":"text","text":"SELECT * FROM users"}],"stop_reason":"end_turn"}`, "no emit_segment_rule"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := &AnthropicClient{APIKey: "secret-key-must-not-leak", BaseURL: srv.URL}
			_, err := c.GenerateRule(context.Background(), RuleRequest{Brief: "x", ToolName: ToolName, Schema: map[string]any{}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "secret-key") {
				t.Fatal("API key leaked into error")
			}
		})
	}
}

func TestJSONSchemaMatchesRegistry(t *testing.T) {
	s := DefaultRegistry.JSONSchema()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range DefaultRegistry.Fields() {
		if !strings.Contains(string(b), `"`+f.Name+`"`) {
			t.Errorf("schema missing field %s", f.Name)
		}
	}
	if strings.Contains(string(b), "password") {
		t.Error("schema must only expose whitelisted fields")
	}
}
