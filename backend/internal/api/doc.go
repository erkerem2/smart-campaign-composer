// Package api is the HTTP layer of Smart Campaign Composer: one standard
// library mux that serves every REST and Server-Sent Events endpoint the Vue
// frontend needs, behind CORS, logging and panic-recovery middleware.
//
//	GET  /healthz                               system, database, provider
//	GET  /metrics                               global totals: campaigns, sent, failed, retried
//	POST /api/campaigns/preview-segment         brief (or edited rule) → rule + audience counts
//	POST /api/campaigns/compose/stream          brief → SMS + e-mail, streamed (SSE)
//	POST /api/campaigns                         approve: rule + content → campaign (approved)
//	GET  /api/campaigns/{id}                    campaign, messages, dispatch stats
//	POST /api/campaigns/{id}/dispatch           start (or resume) asynchronous delivery
//	GET  /api/campaigns/{id}/dispatch           dispatch stats snapshot
//	GET  /api/campaigns/{id}/dispatch/stream    live dispatch stats (SSE)
//
// The handlers only translate HTTP to use-case calls; every dependency is an
// interface (SegmentService, ComposerService, CampaignStore,
// DispatchService, MetricsStore), so tests wire the real engines with a mock
// LLM and a mock provider, and nothing here knows about Anthropic or SQL.
//
// Trust boundaries:
//
//   - LLM output never reaches the database unvalidated: rules go through the
//     segment parser (whitelist, canonical JSON), content through the
//     composer validator (SMS ≤ 160, subject ≤ 80, variables, markup).
//   - The browser is not trusted either: the approval request carries the
//     rule and content the marketer saw (possibly edited), and both are
//     validated again with exactly the same rules before anything is stored.
//   - JSON bodies are size-limited, decoded strictly (unknown fields and
//     trailing data are rejected) and must be sent as application/json, which
//     also forces a CORS preflight for cross-origin writes.
package api
