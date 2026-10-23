# internal/dispatch — Rate-Limited Worker Pool & Dispatch Engine

```
Start(ctx, campaignID, audience)
  ├─ campaign must be 'approved'; CAS approved → sending (only one caller wins)
  ├─ plan  (before anything is sent; total is known up front)
  │    audience ─► gate 1: User.CanReceive(ch)            → opted_out
  │             ─► ClaimBatch: INSERT 'pending' ... WHERE opt_in_<ch> = 1
  │                           ON CONFLICT(campaign,user,channel) DO NOTHING
  │                                                       → skipped (already claimed / opted out since)
  │             ─► Renderer (composer, hard 160 / 80 limits) → render error ⇒ failed, never sent
  └─ deliver  (per channel, in parallel)
       queue ─► N workers ─► Limiter.Wait (shared per channel: ≤100 req/s, retries included)
                         ─► Sender.Send (Idempotency-Key: campaign_id:user_id:channel)
                              ├─ 2xx            → MarkSent  (pending → sent)
                              ├─ 429/5xx/net    → backoff (exp + equal jitter, ≥ Retry-After), max 3 retries
                              └─ other 4xx / exhausted → MarkFailed (pending → failed)
  end: completed (or failed if nothing was sent); ctx cancelled ⇒ 'interrupted', campaign stays 'sending'
Resume(ctx, campaignID, audience)  re-sends pending rows with the SAME key; sent/failed are skipped
```

## Usage

```go
d, _ := dispatch.New(
    repository.NewDispatchRepository(db), repository.NewCampaignRepository(db),
    provider.NewClient("http://localhost:9090", nil), dispatch.NewComposerRenderer(nil),
    dispatch.DefaultConfig())

_ = campaigns.Approve(ctx, id)                            // human approval
run, err := d.Start(serverCtx, id, dispatch.IDAudience(users, ids))
stats, _ := d.Stats(ctx, id)                              // live while running, DB afterwards
```

The public HTTP endpoints (start/resume, stats snapshot, live SSE stream) are served by
`internal/api` (ADR 009), which resolves the audience from the campaign's stored rule.
`StatsHandler` / `StreamHandler` remain for embedding the engine elsewhere.

## Stats

| field       | meaning                                                               |
|-------------|-----------------------------------------------------------------------|
| `total`     | deliveries this run makes (opted-in user × channel, not sent before)  |
| `processed` | reached a final state: `sent + failed`                                |
| `sent`      | accepted by the provider                                              |
| `failed`    | permanent error, retries exhausted, or content over the channel limit |
| `retried`   | retry **attempts** (a message retried twice counts 2)                  |
| `pending`   | `total - processed`                                                   |
| `opted_out` | user × channel pairs excluded for missing permission                  |
| `skipped`   | already dispatched earlier, or listed twice by the audience           |
| `speed`     | average msg/s of the run (`processed / elapsed_seconds`); per channel ≤ 100 |

Per channel under `channels.sms` / `channels.email`. Without an active run in the process the numbers come
from `dispatch_logs` (`opted_out` / `skipped` / `speed` are not persisted and read 0).

## Tests

| test                                          | proves                                                                                      |
|-----------------------------------------------|---------------------------------------------------------------------------------------------|
| `TestDispatchThroughFlakyProvider`            | real mock over HTTP (10% 429, 5% 500, 10-30 ms): provider never sees >100 req/s per channel, retries after 429 come ≥ 1 s later, ≤ 4 attempts, no opted-out recipient contacted, each key accepted at most once, DB and metrics match the provider |
| `TestLostResponsesAreNotDeliveredTwice`       | provider accepted but the response was lost → retry gets the original id, 1 message each   |
| `TestInterruptedRunResumesWithoutDuplicates`  | process stopped mid-run → `Resume` finishes, every key accepted exactly once               |
| `TestLimiter*`                                | sliding-window cap holds for any burst, virtual and real clock, many goroutines            |
| `TestBackoffDelay`, `TestRetriesOn429…`, `TestGivesUpAfterThreeRetries` | exponential + jitter, Retry-After floor, 1 + 3 attempts |
| `TestStaleOptInIsRejectedByClaim`, `TestOptOutAndDuplicateRecipients`    | permission re-checked in SQL; duplicates in the audience collapse |

`go test -short ./...` skips the ~5 s end-to-end test.
