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

## ADR 007: Dispatch Engine — Claim-Before-Send, Strict Per-Channel Rate Limit, Bounded Retries
- **Status:** Accepted
- **Context:** Approved campaigns are delivered asynchronously to a mock provider that behaves like a real gateway (~10% `429` with `Retry-After: 1`, ~5% `500`, 10-30 ms latency). The case requires ≤ 100 msg/s per channel, no duplicate delivery to the same person, no delivery without channel permission, and live progress for the dashboard.
- **Decision:**
  - **Mock provider** (`internal/provider`, standalone binary `cmd/mock-provider`): `POST /v1/sms/send`, `POST /v1/email/send`, `GET /v1/stats`, `/healthz`. Failure injection happens before any processing (nothing is accepted on a 429/500, so retrying is always safe). It enforces the `Idempotency-Key` header: the same key returns the original `message_id` (`200`, `Idempotent-Replayed: true`), the same key with a different payload is `409`. It also validates E.164 / e-mail, SMS ≤ 160 and subject ≤ 80 code points, so a limit bug in our code would surface as `422` in tests.
  - **Claim before send.** Planning inserts one `pending` `dispatch_logs` row per delivery *before* the provider is called: `INSERT ... SELECT ... FROM users WHERE id = ? AND opt_in_<channel> = 1 ON CONFLICT (campaign_id, user_id, channel) DO NOTHING`. Only the caller whose insert affected a row may send. Rows move `pending → sent | failed` with `UPDATE ... WHERE status = 'pending'`, so a final state is never overwritten.
  - **Idempotency key `campaign_id:user_id:channel`** on every attempt. The claim prevents a second *logical* delivery; the key prevents a second *physical* one when the outcome of an attempt is unknown (timeout after the provider accepted, crash between accept and `MarkSent`). Retries and `Resume` reuse the key and get the original result back.
  - **Permission checked twice:** on the loaded user (`domain.User.CanReceive`) and inside the claim SQL, so a user who opts out between audience resolution and sending is not claimed. Pending rows of users who opted out before a resume are closed as `failed`, never sent.
  - **Rate limiter** (`dispatch.Limiter`, one per channel, shared by all campaigns, every attempt including retries takes a slot): a token bucket (rate = limit/window, burst 10) for smooth pacing, plus a sliding-window log of the last 100 grant times — a grant is issued only when the 100th previous grant is at least one window old, which bounds *any* window, not just the average. Implemented on the standard library instead of `golang.org/x/time/rate` because a token bucket alone admits `limit + burst` requests in some windows, and the log is what makes the cap hard; no new dependency is needed. The enforced window is 1 s + 50 ms (`RateSafetyMargin`, ≈ 95 msg/s) so the cap also holds as observed by the provider after network/scheduling jitter; the end-to-end test asserts it on the provider's arrival timestamps.
  - **Retries:** `429`, `408`, `5xx` and transport errors are retried at most **3** times (4 attempts) with exponential backoff and *equal jitter* (`[exp/2, exp)`, base 200 ms, cap 5 s) — unlike full jitter it never retries immediately. `Retry-After` (seconds or HTTP-date, capped at 30 s) is a floor, plus up to one base of jitter so workers that were throttled together do not return together. Other `4xx` fail at once. A per-attempt timeout (5 s) bounds a hung connection.
  - **Worker pool:** per run and channel, 16 goroutines read a shared queue; channels proceed in parallel. Backoff sleeps happen inside the worker (simple, bounded by the pool size; at ~10% 429 roughly 10 workers sleep at a time, so 16 sustain the limit).
  - **Lifecycle:** `Start` requires `approved` and moves to `sending` with a compare-and-set (`UPDATE ... WHERE status = 'approved'`), so two clicks or two processes cannot both start a campaign. The run ends as `completed` (or `failed` when nothing could be delivered). A cancelled context (shutdown) leaves the campaign in `sending`; `Resume` is an explicit operation that re-sends pending rows with the same key and skips finished ones. Final DB writes use `context.WithoutCancel`, so a message the provider accepted is recorded even during shutdown.
  - **Tracking:** lock-free per-channel counters (`total`, `processed`, `sent`, `failed`, `retried` = retry attempts, plus `pending`, `opted_out`, `skipped`), exposed as `GET /api/campaigns/{id}/dispatch` and an SSE stream `/dispatch/stream`. Without a live run the numbers are aggregated from `dispatch_logs` in one indexed query. `/healthz` reports the provider but only the database decides 503.
  - **Schema version 2** (`PRAGMA user_version`): `dispatch_logs` gains `pending`, `last_error`, `created_at`, `updated_at` and `CHECK (status <> 'sent' OR sent_at IS NOT NULL)`. SQLite cannot alter a CHECK in place, so `Migrate` rebuilds a v1 table in one transaction (rename, recreate from `schema.sql`, copy, drop), keeping `schema.sql` the single source of the DDL. `domain.ErrNotFound` / `ErrStatusConflict` are now domain sentinels so use cases can match them without importing the repository.
- **Trade-offs / not done:**
  - Planning claims and renders the whole audience in memory before sending: fine for thousands of recipients and gives an exact `total` up front; millions would need a streaming planner with a durable queue (e.g. Postgres `SKIP LOCKED` or a broker) instead of in-process channels.
  - A `429` slows only the message that received it, not the whole channel. With random 429s (as in the mock) a channel-wide pause would cut throughput for no reason; a provider with real quota-based 429s would warrant an adaptive limiter (AIMD).
  - Rendering must be deterministic per user: if a user's name changes between an interrupted run and `Resume`, the provider answers `409` for the reused key and the delivery is closed as `failed` (never sent twice).
  - The campaign API does not persist the approved segment rule yet, so the start/resume HTTP endpoints arrive with that phase; the engine takes the audience as an `AudienceSource` (e.g. `IDAudience` over the matched user ids).
