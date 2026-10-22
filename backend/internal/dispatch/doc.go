// Package dispatch delivers approved campaigns to the SMS / e-mail provider
// through an asynchronous, rate-limited worker pool and reports live progress.
//
// Guarantees (see ADR 007 in DECISIONS.md):
//
//  1. Permission: a user is only sent a message on a channel they are opted
//     in to. Checked twice: on the loaded user (domain.User.CanReceive) and
//     again inside the SQL claim (INSERT ... SELECT ... WHERE opt_in_x = 1).
//  2. No duplicates: every delivery is claimed as a 'pending' dispatch_logs
//     row BEFORE the provider is called. UNIQUE(campaign_id, user_id, channel)
//     lets exactly one claim win, across workers, runs and processes. Every
//     attempt carries the idempotency key "campaign_id:user_id:channel", so a
//     retry or resume of a request whose response was lost is answered by the
//     provider with the original result instead of a second message.
//  3. Rate: each channel has one shared Limiter (token bucket + sliding-window
//     log) that admits at most 100 request starts per second, retries
//     included.
//  4. Retries: 429, 5xx and transport errors are retried at most 3 times with
//     exponential backoff + equal jitter, never sooner than Retry-After.
//     Other 4xx fail immediately.
//  5. Tracking: lock-free counters give total / processed / sent / failed /
//     retried per campaign and channel; dispatch_logs is the durable record.
//
// All external dependencies are interfaces (Sender, Store, CampaignStore,
// AudienceSource, Renderer, Clock); tests run against SQLite and the in-repo
// mock provider without network access.
package dispatch
