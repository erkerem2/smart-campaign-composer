# Smart Campaign Composer - Development Guidelines

## Role & Engineering Philosophy
You are an expert AI Software Engineer assisting in building a production-grade campaign system.
Follow Clean Architecture, idiomatic Go, and reactive Vue 3 (Composition API).

## Core Principles & Guardrails
1. **Never Trust LLM Output**:
   - Claude must NEVER generate raw SQL strings. Segment rules MUST be structured JSON AST/Rule objects validated against strict schemas before being translated to queries.
   - Message contents must strictly adhere to channel constraints: SMS <= 160 chars, Email Subject <= 80 chars.
2. **Deterministic & Test-Driven**:
   - Write tests BEFORE or ALONGSIDE code. Always write unit tests with mock LLM providers.
   - Every external call (Anthropic API, SMS Provider, Email Provider) MUST sit behind an interface.
3. **Resilience & Safe Messaging**:
   - Worker pool must enforce rate limiting (max 100 msg/sec per channel).
   - Mock provider returns 429s and 500s; workers must implement exponential backoff with jitter and idempotency keys (`campaign_id:user_id:channel`).
   - Check channel-specific user opt-in permissions before dispatching.

## Commands
- Run backend tests: `make test`
- Build & Run: `docker compose up --build`
- Lint: `golangci-lint run`