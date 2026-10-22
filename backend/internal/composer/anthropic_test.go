package composer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/composer"
)

// fakeAPI records the last request body and answers with handler.
func fakeAPI(t *testing.T, handler func(w http.ResponseWriter, body map[string]any)) (*composer.AnthropicMessageClient, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing auth/version headers")
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		handler(w, got)
	}))
	t.Cleanup(srv.Close)
	return &composer.AnthropicMessageClient{APIKey: "test-key", BaseURL: srv.URL}, &got
}

func baseRequest() composer.MessageRequest {
	v := composer.DefaultValidator
	return composer.MessageRequest{Brief: composer.ReferenceBrief, SystemPrompt: v.SystemPrompt(), ToolName: composer.ToolName, Schema: v.ToolSchema()}
}

func TestAnthropicGenerate(t *testing.T) {
	c, got := fakeAPI(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"content":[{"type":"text","text":"Hazır"},{"type":"tool_use","id":"t1","name":%q,"input":%s}],"stop_reason":"tool_use"}`,
			composer.ToolName, composer.ReferenceMessageJSON)
	})
	d, err := composer.NewComposer(c, nil).Generate(context.Background(), composer.ReferenceBrief)
	if err != nil {
		t.Fatal(err)
	}
	if d.Message != composer.ReferenceMessage {
		t.Fatalf("message = %+v", d.Message)
	}
	body := *got
	if tc, _ := body["tool_choice"].(map[string]any); tc["type"] != "tool" || tc["name"] != composer.ToolName {
		t.Errorf("tool_choice must force %s: %v", composer.ToolName, tc)
	}
	if _, streaming := body["stream"]; streaming {
		t.Error("blocking call must not set stream")
	}
	content := body["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(content, "<brief>\n"+composer.ReferenceBrief) {
		t.Errorf("brief must be fenced: %q", content)
	}
	tools := body["tools"].([]any)[0].(map[string]any)
	if tools["name"] != composer.ToolName || tools["input_schema"] == nil {
		t.Errorf("tool = %v", tools)
	}
}

func TestAnthropicGenerateErrors(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"http error": {401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, "authentication_error"},
		"no tool":    {200, `{"content":[{"type":"text","text":"SMS: Merhaba"}],"stop_reason":"end_turn"}`, "no emit_campaign_messages"},
		"truncated":  {200, `{"content":[],"stop_reason":"max_tokens"}`, "truncated"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := fakeAPI(t, func(w http.ResponseWriter, _ map[string]any) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			c.APIKey = "test-key"
			_, err := c.GenerateMessages(context.Background(), baseRequest())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "test-key") {
				t.Fatal("API key leaked into error")
			}
		})
	}
}

// sseBody renders a realistic Messages API stream whose tool input is split
// into the given partial_json fragments.
func sseBody(fragments []string, opts ...string) string {
	var b strings.Builder
	ev := func(name, data string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, data) }
	ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null}}`)
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"sms_content\":\"DECOY\"}"}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("ping", `{"type": "ping"}`)
	fmt.Fprintf(&b, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":%q,\"input\":{}}}\n\n", composer.ToolName)
	b.WriteString(": keep-alive comment\n\n")
	for _, f := range fragments {
		d, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 1,
			"delta": map[string]string{"type": "input_json_delta", "partial_json": f}})
		ev("content_block_delta", string(d))
	}
	ev("content_block_stop", `{"type":"content_block_stop","index":1}`)
	stop := "tool_use"
	for _, o := range opts {
		if strings.HasPrefix(o, "stop=") {
			stop = strings.TrimPrefix(o, "stop=")
		}
		if o == "error" {
			ev("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
		}
		if o == "cut" {
			return b.String()
		}
	}
	ev("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":120}}`, stop))
	ev("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

func fragments(s string, n int) []string {
	var out []string
	r := []rune(s)
	for len(r) > 0 {
		k := min(n, len(r))
		out = append(out, string(r[:k]))
		r = r[k:]
	}
	return out
}

func TestAnthropicStreamThroughComposer(t *testing.T) {
	c, got := fakeAPI(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(fragments(composer.ReferenceMessageJSON, 5)))
	})
	ch, err := composer.NewComposer(c, nil).Stream(context.Background(), composer.ReferenceBrief)
	if err != nil {
		t.Fatal(err)
	}
	chunks := drain(t, ch)
	last := chunks[len(chunks)-1]
	if last.Type != composer.ChunkDone || last.Draft.Message != composer.ReferenceMessage {
		t.Fatalf("last = %+v", last)
	}
	if s := joined(chunks, 1)[composer.FieldSMS]; s != composer.ReferenceMessage.SMSContent {
		t.Fatalf("streamed sms = %q (text blocks must be ignored)", s)
	}
	if (*got)["stream"] != true {
		t.Error("streaming call must set stream=true")
	}
}

func TestAnthropicStreamErrors(t *testing.T) {
	frags := fragments(composer.ReferenceMessageJSON, 7)
	cases := map[string]struct {
		status int
		body   string
		sync   bool // error returned by StreamMessages itself
		want   string
	}{
		"http 429":        {429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, true, "rate_limit_error"},
		"stream error":    {200, sseBody(frags[:3], "error"), false, "overloaded_error"},
		"max tokens":      {200, sseBody(frags[:3], "stop=max_tokens"), false, "truncated"},
		"connection drop": {200, sseBody(frags[:3], "cut"), false, "before message_stop"},
		"no tool block":   {200, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", false, "no emit_campaign_messages"},
		"garbage":         {200, "data: {not json\n\n", false, "bad stream event"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := fakeAPI(t, func(w http.ResponseWriter, _ map[string]any) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			events, err := c.StreamMessages(context.Background(), baseRequest())
			if tc.sync {
				if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "test-key") {
					t.Fatalf("want sync %q, got %v", tc.want, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var last composer.StreamEvent
			for ev := range events {
				last = ev
			}
			if last.Err == nil || !strings.Contains(last.Err.Error(), tc.want) {
				t.Fatalf("want stream error %q, got %+v", tc.want, last)
			}
		})
	}
}
