// Package composer implements the Campaign Message Composer & Validation
// Layer: it turns a marketer's brief into SMS + e-mail (subject and body)
// content with Claude, streams it to the UI, and never lets content that
// breaks channel rules reach the approval step.
//
// Security model — the LLM is an untrusted input source:
//
//  1. Claude must answer through a forced tool call (emit_campaign_messages)
//     whose input is exactly {sms_content, email_subject, email_body}.
//  2. The final tool input is decoded strictly (unknown / duplicate keys,
//     non-string values and trailing data are rejected) — see decodeOutput.
//  3. Validator enforces the channel limits from internal/domain on rune
//     counts, both for the raw template and for the estimated rendered length
//     (each variable has an estimated width, {{first_name}} ≈ 10 runes).
//     Control and bidi-override characters are rejected, the subject must be
//     a single line (header-injection guard), and only whitelisted
//     {{variables}} are accepted.
//  4. The e-mail body may be Markdown or a small whitelisted HTML subset
//     (no scripts, styles, event handlers, comments, javascript: URLs; tags
//     must be balanced).
//  5. A rejected attempt is sent back to the model once with the validator's
//     findings (bounded self-repair). If it still fails, the caller gets a
//     *RejectedError carrying the last attempt so a human can fix it.
//  6. Renderer fills variables from domain.User in a single pass (user data is
//     never re-interpreted as a template), escapes per output format and
//     re-checks the hard limits on the rendered text.
//
// Flow:
//
//	brief ─► MessageLLMClient (Mock | Anthropic) ─► partial JSON stream
//	      ─► fieldExtractor ─► MessageChunk{delta} (UI typing effect, untrusted)
//	      ─► strict decode ─► Normalize ─► Validator ─► MessageChunk{done, Draft}
//	                                          └─ invalid ─► retry with feedback
package composer
