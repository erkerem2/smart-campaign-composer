# internal/composer — Campaign Message Composer & Validation Layer

```
brief ─► MessageLLMClient (Mock | Anthropic, forced tool call emit_campaign_messages)
      ├─ GenerateMessages  → raw JSON (untrusted)
      └─ StreamMessages    → <-chan StreamEvent{PartialJSON}  (input_json_delta)
             └─ fieldExtractor ─► MessageChunk{delta, field}   (display only)
      ─► decodeOutput (strict) ─► Normalize ─► Validator ─► Draft
                                                 └─ invalid → 1 retry with the issues as feedback
Draft ─► Renderer.Render(message, domain.User) at dispatch time (single pass, escaped, hard limits)
Draft ─► Message.CampaignMessages(id) ─► repository.CampaignRepository.SaveDraftMessages
```

## Usage

```go
llm, err := composer.NewAnthropicMessageClientFromEnv() // or composer.NewMockMessageLLMClient()
c := composer.NewComposer(llm, nil)                      // nil → DefaultValidator (160 / 80)

draft, err := c.Generate(ctx, brief)                     // one shot, validated

chunks, err := c.Stream(r.Context(), brief)              // streaming
_ = composer.WriteSSE(r.Context(), w, chunks)            // HTTP handler: SSE to Vue

msg, report, err := c.Check(editedByMarketer)            // same rules for human edits
rendered, err := composer.DefaultRenderer.Render(draft.Message, user)
```

## Output contract

```json
{"sms_content": "{{first_name}}, sepetindeki ürünler seni bekliyor! ...",
 "email_subject": "{{first_name}}, sepetindekiler %30 indirimle seni bekliyor",
 "email_body": "Merhaba {{first_name}},\n\n..."}
```

Decoding is strict: exactly these three keys, all strings, no duplicates, nothing after the object.

## Validation rules

| Field | Rule |
|---|---|
| `sms_content` | ≤ 160 runes as written **and** ≤ 160 estimated after personalization; newlines allowed; plain text |
| `email_subject` | ≤ 80 runes as written and estimated; single line (header-injection guard) |
| `email_body` | no business limit (64 KB safety cap); Markdown, or whitelisted HTML (`p br hr div span strong b em i u h1–h4 ul ol li blockquote a[href,title]`), balanced tags, links only `https://`, `http://`, `mailto:` |
| all | non-empty, valid UTF-8, no control or bidi-override characters, only whitelisted `{{variables}}` |

Lengths are Unicode code points, so `ş` counts as one. Estimated length swaps each placeholder for the
variable's `EstimatedRunes`: `{{first_name}}` is 14 runes as written and 10 when estimated.

Variables: `{{first_name}}` (≈10, fallback "Değerli müşterimiz"), `{{last_name}}` (≈10).
`{{ first_name }}` is accepted; `{{credit_card}}`, `{{First_Name}}`, `{first_name}}`, `{{{x}}}` are rejected.

Map errors with `errors.Is`: `ErrInvalidContent` (umbrella) and the field kinds `ErrTooLong`,
`ErrEstimatedTooLong`, `ErrUnknownVariable`, `ErrMalformedPlaceholder`, `ErrForbiddenCharacter`,
`ErrUnsafeMarkup`, `ErrEmptyField`, `ErrTooLarge` → 422; `ErrMalformedOutput` / `*RejectedError` → 422
with `Last` for manual editing; `ErrLLM` → 502; `ErrEmptyBrief` / `ErrBriefTooLong` → 400.
`composer.IssuesOf(err)` returns `[]Issue{field, code, message}` for the UI.

## Stream events (SSE)

| `event` | Payload | UI action |
|---|---|---|
| `delta` | `{"field":"sms_content","delta":"Ayşe, sep","attempt":1}` | append text to that field (unvalidated) |
| `retry` | `{"issues":[...],"attempt":1}` | clear fields, show "düzeltiliyor…" |
| `done` | `{"draft":{"message":…,"report":…,"attempts":n}}` | **replace** streamed text with `draft.message` |
| `error` | `{"error":"…","issues":[…],"rejected":{…}}` | show issues; `rejected` can be edited by hand |

The stream always ends with exactly one `done` or `error`, unless the client disconnects.

## Rendering

`Renderer` substitutes values in a single pass (a name like `{{credit_card}}` stays literal), strips
control/bidi characters, collapses whitespace (no line breaks into a subject), caps a value at 64 runes,
HTML-escapes for HTML bodies and backslash-escapes for Markdown, then re-checks 160 / 80 on the real
text (`ErrRenderedTooLong`). A test renders the reference content for all 3,000 seed users: the longest
SMS is 121/160, subject 51/80.

## Tests

```
make test                                   # whole backend
cd backend && go test -count=1 -v ./internal/composer/
```

No API key needed: `MockMessageLLMClient` serves canned output (and fragmented streams, failures,
"invalid first, valid after feedback" sequences); `AnthropicMessageClient` is tested against an
`httptest` fake of the Messages API, including SSE parsing and mid-stream errors.
