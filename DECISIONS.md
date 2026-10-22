# Architectural Decisions Log

## ADR 001: Technology Stack & Language Selection
- **Status:** Accepted
- **Context:** Case requires building an end-to-end campaign composer with real-time streaming, asynchronous message queuing, strict rate limiting (100 msg/sec), and mock providers. Allowed backends: Go or PHP.
- **Decision:** Selected **Go** for the backend and **Vue 3 (Composition API + Tailwind)** for the frontend.
- **Rationale:** 
  - Go provides first-class concurrency primitives (goroutines, channels) ideal for high-throughput dispatching and precise token-bucket rate limiting.
  - Strong typing and lightweight footprint ensure predictable performance during bulk message processing.
  - Vue 3 with Server-Sent Events (SSE) allows responsive token streaming for campaign generation and live dashboard metric updates without heavy WebSocket overhead.

## ADR 002: SQLite with a Pure-Go Driver
- **Status:** Accepted
- **Context:** Local development and `make test` must work without installing or running a database server. The primary dev machine is Windows, where CGO-based drivers require a separate C toolchain.
- **Decision:** Use **SQLite** via **`modernc.org/sqlite`** (pure Go, registers as driver `"sqlite"`), behind `database/sql`.
- **Rationale:**
  - Zero setup: no server, no gcc, identical behaviour on Windows/macOS/Linux and in a `CGO_ENABLED=0` Docker image.
  - Per-connection settings (`foreign_keys`, `busy_timeout`) are applied by a small `driver.Connector` wrapper on **every** pooled connection, not once after `Open`; a test holds several connections at once to prove it. The wrapper uses only `database/sql/driver` interfaces, so swapping drivers is a one-line change.
  - WAL journal mode allows dashboard reads while workers write.
- **Trade-offs:** SQLite has a single writer. Acceptable for the case's volume (a few thousand recipients, 100 msg/s per channel); `busy_timeout` absorbs short contention. Postgres would be the next step for multi-instance deployment.

## ADR 003: Storage Conventions
- **Status:** Accepted
- **Decision:**
  - **Timestamps** are stored as TEXT in fixed-width UTC (`YYYY-MM-DDTHH:MM:SSZ`), converted explicitly in the repository rather than by the driver. Fixed width makes lexicographic order equal chronological order, so segment filters such as `cart_last_active_at >= ?` use plain indexes and never depend on driver time formats.
  - **Booleans** are `INTEGER 0/1`; **enums** (`channel`, campaign/message/dispatch `status`) are guarded by `CHECK` constraints mirroring `internal/domain`.
  - **`dispatch_logs` has `UNIQUE(campaign_id, user_id, channel)`**, the same tuple as the provider idempotency key (`campaign_id:user_id:channel`). "The same person never gets the same message twice" is therefore enforced by the database, not only by application logic.
  - `campaign_messages` enforces "email has a subject, SMS never does" with a `CHECK`.
  - `total_spend` is `REAL` (TRY): it is a segmentation attribute compared with thresholds, not a ledger balance.
  - Schema is an embedded, idempotent `schema.sql` (`CREATE ... IF NOT EXISTS`); a migration tool is deferred until the schema needs to evolve.

## ADR 004: Deterministic, Cohort-Based Seed Data
- **Status:** Accepted
- **Context:** The seed must make the sample brief ("son 30 günde sepetine ürün ekleyip alışveriş yapmayan İstanbul'daki kullanıcılar") testable, including permission filtering.
- **Decision:** `cmd/seed` generates 3,000 users (configurable) from explicit cohorts instead of uniform randomness:
  - **True positives (~22%)**: İstanbul, cart activity in the last 30 days, no purchase in the last 30 days.
  - **Near misses**, each failing exactly one condition: converted after carting, cart older than 30 days, never used the cart, and the same abandoned-cart behaviour in other cities.
  - **Ambiguous case**: purchased within 30 days, then left a newer cart. Excluded by the strict reading of the brief; documents a real interpretation choice the segment prompt must make.
  - Every cohort contains SMS-only, email-only and both-channel opt-outs.
- **Rationale:**
  - A fixed RNG seed makes the dataset reproducible; dates are relative to run time so "last 30 days" stays meaningful.
  - `seed.MatchesSampleBrief` is a Go reference oracle. Segment-engine tests can compare their SQL results against it (verified: SQL and oracle both return the same count).
  - Emails use RFC 2606 reserved domains (`example.com/.net/.org`), so no generated address can belong to a real mailbox.
  - Canonical city spelling is `İstanbul`; the segment validator must normalise LLM output (`Istanbul`, `istanbul`) to it.
  - The command is idempotent (skips when users exist, `-reset` to reseed), so it can run on every `docker compose up`.

## ADR 005: Safe Segment Engine & AST Validation (Zero Raw SQL from LLM)
- **Status:** Accepted
- **Context:** Marketers describe campaign briefs in natural language. Executing LLM-generated raw SQL directly against the database introduces severe SQL injection vulnerabilities and unpredictable query execution.
- **Decision:** 
  - Constrained Claude to output a structured JSON Abstract Syntax Tree (AST) using Anthropic Tool Calling / Structured Output.
  - Built an in-engine whitelist validator in Go that only permits known attributes (`city`, `age`, `total_spend`, `cart_last_active_days`, `has_purchased_since_cart`) and comparison operators (`EQUALS`, `LESS_THAN_OR_EQUAL`, `IN`, etc.).
  - Implemented custom Turkish character normalization (case folding for I/İ/ı, diacritics, and 81 city aliases) to handle inputs safely without raw text matching.
  - Parameterized SQL compiler builds queries (`?`) strictly from internal Go constants, ensuring placeholder and argument counts match under fuzz testing.
  - Implemented `MockLLMClient` returning deterministic ASTs for zero-cost automated unit and integration tests without external API dependencies.

## ADR 006: Campaign Message Composer — Structured Output, Streaming & Validation
- **Status:** Accepted
- **Context:** Claude writes SMS and e-mail (subject + body) for a brief; the UI must show the text as it is generated, and nothing that breaks channel rules may reach approval or dispatch.
- **Decision:**
  - **Forced tool call** `emit_campaign_messages` with exactly `sms_content`, `email_subject`, `email_body`. The final tool input is decoded strictly (unknown/duplicate keys, non-strings, trailing data rejected). System prompt and JSON Schema are generated from the validator's limits and variable whitelist, so they cannot drift apart.
  - **Two-layer streaming:** the LLM client exposes `<-chan StreamEvent` with raw `input_json_delta` fragments; an incremental JSON field extractor turns them into per-field `MessageChunk{delta}` events (robust to fragments split inside escapes, surrogate pairs and UTF-8 sequences). Deltas are display-only; the `done` event carries the validated draft, which the UI must use instead of the streamed text. `composer.WriteSSE` relays chunks over Server-Sent Events.
  - **Validator** counts Unicode code points (not bytes) and enforces SMS ≤ 160 / subject ≤ 80 both on the template as written and on an estimated rendered length (`{{first_name}}` ≈ 10). It reports every issue at once (field, stable code, message). Subjects must be single-line (header injection); control and bidi-override characters are rejected; the e-mail body is Markdown or a small whitelisted HTML subset with balanced tags and `https/http/mailto` links only.
  - **Bounded self-repair:** an invalid attempt is sent back once with the issues (`MaxAttempts = 2`). If it still fails, a `RejectedError` carries the last attempt so the marketer can fix it by hand — the human stays in control rather than the system looping.
  - **Renderer** fills `{{variables}}` from `domain.User` in a single pass (user data is never re-parsed as a template), cleans and caps values, escapes per format, and re-checks the hard limits on the real text, because a real name can exceed the estimate.
  - **Persistence:** `Message.CampaignMessages` maps to `domain.CampaignMessage`; `CampaignRepository.SaveDraftMessages` writes SMS subjects as `NULL` (the schema CHECK rejects `''`) and refuses to change campaigns that left `draft`.
- **Trade-offs / not done:**
  - SMS length follows the case's 160-character rule. Real carriers bill by encoding (GSM-7 vs UCS-2; most Turkish letters force UCS-2 with 70-char segments); a segment counter is a natural next step.
  - The HTML check is a purpose-built whitelist tokenizer on the standard library rather than `golang.org/x/net/html`, to keep zero new dependencies. It rejects anything it does not understand instead of sanitising.
  - The Anthropic transport is duplicated between `segment` and `composer` (~60 lines); extracting a shared `internal/llm` client is deferred until a third caller appears.
