# internal/api — HTTP API (REST + SSE)

One standard-library `http.ServeMux` behind `recoverer → requestLogger → cors`. Every dependency is an
interface (`SegmentService`, `ComposerService`, `CampaignStore`, `DispatchService`); `cmd/server` wires the
real engines, tests wire the same engines with a mock LLM and the mock provider.

```
brief ─► POST /preview-segment ─► rule + audience     (LLM → whitelist parser → v_users_segment)
brief ─► POST /compose/stream  ─► SSE delta… done     (LLM → validator; "done" carries the draft)
rule + draft (maybe edited) ─► POST /api/campaigns ─► approved   (validated AGAIN, one transaction)
POST /{id}/dispatch ─► 202 ─► GET /{id}/dispatch/stream ─► SSE stats… done
```

## Endpoints

| Method & path | Body | Success | Errors (`error.code`) |
|---|---|---|---|
| `GET /healthz` | – | `200` `{status: ok\|degraded, checks, system}` | `503` `unhealthy` (database) |
| `POST /api/campaigns/preview-segment` | `{"brief"}` or `{"rule": {...}}` | `200` `{brief, rule, audience: {total, sms_opt_in, email_opt_in, reachable}}` | `400 invalid_brief`, `422 invalid_rule` (+`path`), `502 llm_unavailable` |
| `POST /api/campaigns/compose/stream` | `{"brief"}` | `200 text/event-stream` | `400 invalid_brief` (before the stream) |
| `POST /api/campaigns` | `{"brief", "segment_rule", "messages": {sms_content, email_subject, email_body}}` | `201` + `Location`, `{campaign, messages, report, audience}` | `400 missing_rule\|missing_messages\|invalid_brief`, `422 invalid_rule\|invalid_content (+issues)\|empty_audience` |
| `GET /api/campaigns/{id}` | – | `200` `{campaign, messages, dispatch}` | `400 invalid_id`, `404 campaign_not_found` |
| `POST /api/campaigns/{id}/dispatch` | `{}` or empty, `Content-Type: application/json` required (CSRF) | `202` `{campaign_id, state, resumed, audience_size, stats_url, stream_url}` | `409 already_running\|invalid_state\|missing_rule\|invalid_rule`, `404`, `415 unsupported_media_type`, `403 origin_not_allowed` |
| `GET /api/campaigns/{id}/dispatch` | – | `200` stats snapshot | `404` |
| `GET /api/campaigns/{id}/dispatch/stream` | – | `200 text/event-stream` | `404` |

All errors: `{"error": {"code", "message", "path"?, "issues"?}}`. JSON bodies must be
`Content-Type: application/json` (else `415`), ≤ 256 KB (`413`), one object without unknown fields (`400 invalid_json`).

## Streams

**Compose** (POST, so read it with `fetch` + `response.body.getReader()`; `EventSource` cannot POST):

```
event: delta  data: {"type":"delta","attempt":1,"field":"sms_content","delta":"{{first_name}}, sep"}
event: retry  data: {"type":"retry","attempt":1,"issues":[...]}        ← clear the fields, attempt 2 follows
event: done   data: {"type":"done","attempt":1,"draft":{"message":{...},"report":{...},"attempts":1}}
event: error  data: {"type":"error","error":"...","issues":[...],"rejected":{...}}  ← editable last attempt
```

Deltas are display-only. Show `done.draft.message` for review and send it (edited or not) to `POST /api/campaigns`.

**Dispatch** (GET, `EventSource` works — call `close()` on `done`, otherwise it reconnects):

```
event: stats  data: {"state":"sending","live":true,"total":1147,"processed":702,"sent":702,"failed":0,
                     "retried":122,"pending":445,"speed":94.8,"elapsed_seconds":7.4,"channels":{"sms":{...},"email":{...}}}
event: done   data: {"state":"completed"}
```

`speed` = average msg/s of the run (per channel ≤ 100). A campaign without a live run streams one snapshot and `done`.

## Behaviour worth knowing

- **Approval re-validates everything** the browser sends: the rule through the segment parser (the canonical
  form is stored, e.g. `Istanbul` → `İstanbul`), the content through the composer validator (SMS ≤ 160,
  subject ≤ 80, variables, markup). A rule nobody opted in to is refused (`empty_audience`).
- **Dispatch resolves recipients at dispatch time** from the stored rule (re-validated), and loads users then,
  so opt-outs since approval are honoured. A campaign stuck in `sending` (process restart) is **resumed** by the
  same `POST`, without sending anything twice. Runs use the server's lifetime context, not the request's.
- **CORS:** allowed origins are echoed with `Vary: Origin`; preflights are answered with `204` (or `403` for
  unknown origins). Defaults cover Vite (`5173`, `4173`), `8081` and `3000` on `localhost`/`127.0.0.1`;
  override with `CORS_ALLOWED_ORIGINS=http://a,http://b` (or `*`). No cookies, so no credentials mode.
- **Mock mode:** without `ANTHROPIC_API_KEY` the server uses the mock LLMs, which answer every brief with the
  reference rule and content (`MOCK_STREAM_DELAY_MS` slows the token stream for a visible typing effect).

## Tests

```
go test ./internal/api -run TestEndToEnd -v     # full flow on 3,000 seed users, ~10 s (real 100 msg/s limit)
go test ./internal/api -short                   # everything except the end-to-end dispatch
```

`TestEndToEndCampaignFlow` asserts: health + preflight; preview counts equal a Go evaluation of the rule on
the seed; streamed deltas reassemble to the validated draft; invalid approvals (161-char SMS, 81-char subject,
non-whitelisted field, raw SQL, empty audience, unknown field, wrong content type) store nothing; approval
stores the canonical rule; a double dispatch click gets `409`; the stream shows live progress with a speed
and ends `completed` with `total = sms_opt_in + email_opt_in`; the provider received only audience members with
the channel's opt-in, accepted each idempotency key once, and its accepted count equals `sent`.
