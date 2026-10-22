package composer_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"smart-campaign-composer/backend/internal/composer"
)

func TestWriteSSE(t *testing.T) {
	ch := make(chan composer.MessageChunk, 3)
	ch <- composer.MessageChunk{Type: composer.ChunkDelta, Attempt: 1, Field: composer.FieldSMS, Delta: "Satır\n\"iki\""}
	ch <- composer.MessageChunk{Type: composer.ChunkDone, Attempt: 1, Draft: &composer.Draft{Message: composer.ReferenceMessage, Attempts: 1}}
	close(ch)

	rec := httptest.NewRecorder()
	if err := composer.WriteSSE(context.Background(), rec, ch); err != nil {
		t.Fatal(err)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("headers = %v", rec.Header())
	}
	if !rec.Flushed {
		t.Error("events must be flushed")
	}
	events := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n\n"), "\n\n")
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d: %q", len(events), rec.Body.String())
	}
	wantTypes := []string{"delta", "done"}
	for i, e := range events {
		lines := strings.Split(e, "\n")
		if len(lines) != 2 || lines[0] != "event: "+wantTypes[i] || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("event %d = %q", i, e)
		}
		var c composer.MessageChunk
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &c); err != nil {
			t.Fatal(err)
		}
		if string(c.Type) != wantTypes[i] {
			t.Fatalf("payload type = %s", c.Type)
		}
	}
}

// End to end over a real HTTP connection: Composer.Stream → WriteSSE → client.
func TestSSEEndToEnd(t *testing.T) {
	c := composer.NewComposer(composer.NewMockMessageLLMClient(), nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunks, err := c.Stream(r.Context(), r.URL.Query().Get("brief"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = composer.WriteSSE(r.Context(), w, chunks)
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"?brief="+url.QueryEscape(composer.ReferenceBrief), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sms := ""
	var done *composer.Draft
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ch composer.MessageChunk
		if err := json.Unmarshal([]byte(line), &ch); err != nil {
			t.Fatal(err)
		}
		switch ch.Type {
		case composer.ChunkDelta:
			if ch.Field == composer.FieldSMS {
				sms += ch.Delta
			}
		case composer.ChunkDone:
			done = ch.Draft
		}
	}
	if done == nil || sms != done.Message.SMSContent || done.Message != composer.ReferenceMessage {
		t.Fatalf("sms=%q done=%+v", sms, done)
	}
}
