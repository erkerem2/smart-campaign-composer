package composer_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"smart-campaign-composer/backend/internal/composer"
)

func newComposer(mock *composer.MockMessageLLMClient) *composer.Composer {
	return composer.NewComposer(mock, nil)
}

func jsonOf(m composer.Message) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// invalidMessage breaks several rules at once.
var invalidMessage = composer.Message{
	SMSContent:   "{{first_name}}, " + strings.Repeat("çok uzun bir sms ", 12) + "{{credit_card}}",
	EmailSubject: strings.Repeat("Konu ", 20),
	EmailBody:    "<p onclick=\"x()\">Merhaba</p>",
}

const otherBrief = "Ankara'daki VIP müşterilere yeni sezon duyurusu"

func TestGenerateReference(t *testing.T) {
	mock := composer.NewMockMessageLLMClient()
	d, err := newComposer(mock).Generate(context.Background(), "  "+composer.ReferenceBrief+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if d.Message != composer.ReferenceMessage || d.Attempts != 1 {
		t.Fatalf("draft = %+v", d)
	}
	if d.Report.SMS.Runes != 128 || d.Report.SMS.EstimatedRunes != 132 || d.Report.EmailSubject.Runes > 80 {
		t.Errorf("report = %+v", d.Report)
	}
	req := mock.Requests[0]
	if req.Brief != composer.ReferenceBrief || req.ToolName != composer.ToolName || req.Schema == nil ||
		!strings.Contains(req.SystemPrompt, "{{first_name}}") || req.PreviousAttempt != "" || req.Issues != nil {
		t.Errorf("request = %+v", req)
	}
}

func TestGenerateNormalizesOutput(t *testing.T) {
	m := composer.ReferenceMessage
	m.SMSContent = "  " + m.SMSContent + "\r\n"
	mock := composer.NewMockMessageLLMClient().On(otherBrief, jsonOf(m))
	d, err := newComposer(mock).Generate(context.Background(), otherBrief)
	if err != nil {
		t.Fatal(err)
	}
	if d.Message.SMSContent != composer.ReferenceMessage.SMSContent {
		t.Fatalf("sms = %q", d.Message.SMSContent)
	}
}

func TestGenerateRetriesWithFeedback(t *testing.T) {
	mock := composer.NewMockMessageLLMClient().On(otherBrief, jsonOf(invalidMessage), composer.ReferenceMessageJSON)
	d, err := newComposer(mock).Generate(context.Background(), otherBrief)
	if err != nil {
		t.Fatalf("second attempt is valid: %v", err)
	}
	if d.Attempts != 2 || len(mock.Requests) != 2 {
		t.Fatalf("attempts=%d calls=%d", d.Attempts, len(mock.Requests))
	}
	retry := mock.Requests[1]
	codes := map[string]bool{}
	for _, is := range retry.Issues {
		codes[string(is.Field)+"/"+is.Code] = true
	}
	for _, want := range []string{"sms_content/too_long", "sms_content/unknown_variable", "email_subject/too_long", "email_body/unsafe_markup"} {
		if !codes[want] {
			t.Errorf("feedback missing %s: %v", want, codes)
		}
	}
	if !strings.Contains(retry.PreviousAttempt, "credit_card") {
		t.Errorf("previous attempt not sent back")
	}
	prompt := composer.UserPrompt(retry)
	for _, want := range []string{"<brief>", otherBrief, "<previous_attempt>", "<validation_errors>", "unknown_variable"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("retry prompt missing %q", want)
		}
	}
}

func TestGenerateRejectedAfterMaxAttempts(t *testing.T) {
	mock := composer.NewMockMessageLLMClient().On(otherBrief, jsonOf(invalidMessage))
	_, err := newComposer(mock).Generate(context.Background(), otherBrief)
	var rej *composer.RejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("want RejectedError, got %v", err)
	}
	if rej.Attempts != 2 || rej.Last == nil || rej.Last.EmailBody != invalidMessage.EmailBody || len(mock.Requests) != 2 {
		t.Fatalf("rejected = %+v calls=%d", rej, len(mock.Requests))
	}
	if !errors.Is(err, composer.ErrInvalidContent) || !errors.Is(err, composer.ErrTooLong) || !errors.Is(err, composer.ErrUnknownVariable) {
		t.Fatalf("classification lost: %v", err)
	}
	if len(composer.IssuesOf(err)) < 4 {
		t.Fatalf("issues = %v", composer.IssuesOf(err))
	}
}

func TestGenerateRejectsMalformedOutput(t *testing.T) {
	for name, raw := range map[string]string{
		"extra key":   `{"sms_content":"a","email_subject":"b","email_body":"c","segment_sql":"DROP TABLE users"}`,
		"plain text":  `Tabii! İşte SMS metniniz: ...`,
		"wrong types": `{"sms_content":["a"],"email_subject":"b","email_body":"c"}`,
	} {
		t.Run(name, func(t *testing.T) {
			mock := composer.NewMockMessageLLMClient().On(otherBrief, raw)
			c := newComposer(mock)
			_, err := c.Generate(context.Background(), otherBrief)
			var rej *composer.RejectedError
			if !errors.As(err, &rej) || !errors.Is(err, composer.ErrMalformedOutput) || rej.Last != nil {
				t.Fatalf("want malformed RejectedError, got %v", err)
			}
			if is := mock.Requests[1].Issues; len(is) != 1 || is[0].Code != "malformed_output" {
				t.Fatalf("decode errors must be fed back too: %+v", is)
			}
		})
	}
}

func TestGenerateLLMFailure(t *testing.T) {
	mock := composer.NewMockMessageLLMClient()
	mock.Err = errors.New("overloaded")
	_, err := newComposer(mock).Generate(context.Background(), composer.ReferenceBrief)
	if !errors.Is(err, composer.ErrLLM) {
		t.Fatalf("want ErrLLM, got %v", err)
	}
	if len(mock.Requests) != 1 {
		t.Fatal("transport errors are not retried by the composer")
	}
}

func TestBriefValidation(t *testing.T) {
	c := newComposer(composer.NewMockMessageLLMClient())
	if _, err := c.Generate(context.Background(), " \n "); !errors.Is(err, composer.ErrEmptyBrief) {
		t.Errorf("empty: %v", err)
	}
	if _, err := c.Stream(context.Background(), strings.Repeat("ş", composer.MaxBriefRunes+1)); !errors.Is(err, composer.ErrBriefTooLong) {
		t.Errorf("too long: %v", err)
	}
}

func TestCheckEditedContent(t *testing.T) {
	c := newComposer(composer.NewMockMessageLLMClient())
	edited := composer.ReferenceMessage
	edited.SMSContent = " " + edited.SMSContent + "\r\n"
	m, rep, err := c.Check(edited)
	if err != nil || m.SMSContent != composer.ReferenceMessage.SMSContent || rep.SMS.Runes != 128 {
		t.Fatalf("check: %v %+v", err, rep.SMS)
	}
	edited.EmailSubject = strings.Repeat("x", 81)
	if _, _, err := c.Check(edited); !errors.Is(err, composer.ErrTooLong) {
		t.Fatalf("edits get the same rules: %v", err)
	}
}

func TestSystemPromptAndSchema(t *testing.T) {
	v := composer.DefaultValidator
	p := v.SystemPrompt()
	for _, want := range []string{composer.ToolName, "160", "80", "{{first_name}}", "{{last_name}}", "<brief>", "credit_card", "https://"} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	s := v.ToolSchema()
	if s["additionalProperties"] != false {
		t.Error("schema must forbid extra properties")
	}
	req, _ := s["required"].([]string)
	if strings.Join(req, ",") != "sms_content,email_subject,email_body" {
		t.Errorf("required = %v", req)
	}
	props := s["properties"].(map[string]any)
	if props["sms_content"].(map[string]any)["maxLength"] != 160 || props["email_subject"].(map[string]any)["maxLength"] != 80 {
		t.Error("schema maxLength must mirror limits")
	}
	if _, err := json.Marshal(s); err != nil {
		t.Fatal(err)
	}
}

// ---- streaming ----

func drain(t *testing.T, ch <-chan composer.MessageChunk) []composer.MessageChunk {
	t.Helper()
	var out []composer.MessageChunk
	timeout := time.After(5 * time.Second)
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, c)
		case <-timeout:
			t.Fatal("stream did not finish")
		}
	}
}

// joined concatenates delta text per field for one attempt.
func joined(chunks []composer.MessageChunk, attempt int) map[composer.Field]string {
	got := map[composer.Field]string{}
	for _, c := range chunks {
		if c.Type == composer.ChunkDelta && c.Attempt == attempt {
			got[c.Field] += c.Delta
		}
	}
	return got
}

func TestStreamReference(t *testing.T) {
	mock := composer.NewMockMessageLLMClient()
	ch, err := newComposer(mock).Stream(context.Background(), composer.ReferenceBrief)
	if err != nil {
		t.Fatal(err)
	}
	chunks := drain(t, ch)
	last := chunks[len(chunks)-1]
	if last.Type != composer.ChunkDone || last.Draft == nil || last.Draft.Message != composer.ReferenceMessage {
		t.Fatalf("last chunk = %+v", last)
	}
	deltas := 0
	for i, c := range chunks[:len(chunks)-1] {
		if c.Type != composer.ChunkDelta {
			t.Fatalf("chunk %d: unexpected %s", i, c.Type)
		}
		deltas++
	}
	if deltas < 20 {
		t.Fatalf("content should arrive in many small deltas, got %d", deltas)
	}
	if chunks[0].Field != composer.FieldSMS {
		t.Errorf("SMS streams first, got %s", chunks[0].Field)
	}
	got := joined(chunks, 1)
	for _, f := range composer.Fields {
		if got[f] != composer.ReferenceMessage.Get(f) {
			t.Errorf("%s streamed %q, want %q", f, got[f], composer.ReferenceMessage.Get(f))
		}
	}
}

func TestStreamRetryThenDone(t *testing.T) {
	mock := composer.NewMockMessageLLMClient().On(otherBrief, jsonOf(invalidMessage), composer.ReferenceMessageJSON)
	ch, _ := newComposer(mock).Stream(context.Background(), otherBrief)
	chunks := drain(t, ch)
	var types []composer.ChunkType
	for _, c := range chunks {
		if len(types) == 0 || types[len(types)-1] != c.Type {
			types = append(types, c.Type)
		}
	}
	if strings.Join(chunkTypes(types), ",") != "delta,retry,delta,done" {
		t.Fatalf("sequence = %v", types)
	}
	for _, c := range chunks {
		if c.Type == composer.ChunkRetry && (c.Attempt != 1 || len(c.Issues) < 4) {
			t.Fatalf("retry chunk = %+v", c)
		}
	}
	if got := joined(chunks, 1)[composer.FieldEmailBody]; got != invalidMessage.EmailBody {
		t.Errorf("attempt 1 body = %q", got)
	}
	last := chunks[len(chunks)-1]
	if last.Attempt != 2 || last.Draft.Attempts != 2 {
		t.Fatalf("done = %+v", last)
	}
}

func chunkTypes(ts []composer.ChunkType) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return out
}

func TestStreamRejected(t *testing.T) {
	mock := composer.NewMockMessageLLMClient().On(otherBrief, jsonOf(invalidMessage))
	ch, _ := newComposer(mock).Stream(context.Background(), otherBrief)
	chunks := drain(t, ch)
	last := chunks[len(chunks)-1]
	if last.Type != composer.ChunkError || last.Rejected == nil || len(last.Issues) < 4 || last.Attempt != 2 {
		t.Fatalf("last = %+v", last)
	}
	if !strings.Contains(last.Error, "rejected after 2 attempt") {
		t.Errorf("error = %q", last.Error)
	}
}

func TestStreamMidStreamFailure(t *testing.T) {
	mock := composer.NewMockMessageLLMClient()
	mock.StreamFailAfter = 3
	ch, _ := newComposer(mock).Stream(context.Background(), composer.ReferenceBrief)
	chunks := drain(t, ch)
	last := chunks[len(chunks)-1]
	if last.Type != composer.ChunkError || !strings.Contains(last.Error, "llm call failed") || last.Draft != nil {
		t.Fatalf("last = %+v", last)
	}
	if len(mock.Requests) != 1 {
		t.Fatal("transport errors are not retried by the composer")
	}
}

func TestStreamLLMErrorBeforeStart(t *testing.T) {
	mock := composer.NewMockMessageLLMClient()
	mock.Err = errors.New("401 unauthorized")
	ch, err := newComposer(mock).Stream(context.Background(), composer.ReferenceBrief)
	if err != nil {
		t.Fatal(err)
	}
	chunks := drain(t, ch)
	if len(chunks) != 1 || chunks[0].Type != composer.ChunkError {
		t.Fatalf("chunks = %+v", chunks)
	}
}

func TestStreamStopsWhenClientGoesAway(t *testing.T) {
	mock := composer.NewMockMessageLLMClient()
	mock.ChunkDelay = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := newComposer(mock).Stream(ctx, composer.ReferenceBrief)
	<-ch
	<-ch
	cancel()
	chunks := drain(t, ch) // must close promptly
	for _, c := range chunks {
		if c.Type == composer.ChunkDone {
			t.Fatal("a cancelled stream must not report done")
		}
	}
}

func TestStreamTimeoutIsReported(t *testing.T) {
	mock := composer.NewMockMessageLLMClient()
	mock.ChunkDelay = 20 * time.Millisecond
	c := newComposer(mock)
	c.Timeout = 50 * time.Millisecond
	ch, _ := c.Stream(context.Background(), composer.ReferenceBrief)
	chunks := drain(t, ch)
	last := chunks[len(chunks)-1]
	if last.Type != composer.ChunkError || !strings.Contains(last.Error, "deadline") {
		t.Fatalf("last = %+v", last)
	}
}

func TestChunkJSONShape(t *testing.T) {
	b, err := json.Marshal(composer.MessageChunk{Type: composer.ChunkDelta, Attempt: 1, Field: composer.FieldSMS, Delta: "Ayşe"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"type":"delta","attempt":1,"field":"sms_content","delta":"Ayşe"}` {
		t.Fatalf("json = %s", b)
	}
	_, verr := composer.DefaultValidator.Validate(withSMS(strings.Repeat("a", 161)))
	b, _ = json.Marshal(composer.MessageChunk{Type: composer.ChunkRetry, Issues: composer.IssuesOf(verr)})
	if !strings.Contains(string(b), `"field":"sms_content","code":"too_long"`) {
		t.Fatalf("issue json = %s", b)
	}
}

func TestMockDefaultResponse(t *testing.T) {
	ctx := context.Background()
	mock := composer.NewMockMessageLLMClient()
	if _, err := newComposer(mock).Generate(ctx, otherBrief); !errors.Is(err, composer.ErrLLM) {
		t.Fatalf("unknown brief without a default must fail: %v", err)
	}
	mock.Default(composer.ReferenceMessageJSON)
	d, err := newComposer(mock).Generate(ctx, otherBrief)
	if err != nil || d.Message != composer.ReferenceMessage {
		t.Fatalf("default not used: %+v, %v", d, err)
	}
}
