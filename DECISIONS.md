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
