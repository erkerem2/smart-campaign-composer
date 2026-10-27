# Smart Campaign Composer — Frontend

Vue 3 (Composition API, `<script setup>`) + Vite + Tailwind CSS v4 single-page app for the whole campaign flow:

| Step | What the marketer does | API |
|---|---|---|
| 1 Brief | Writes the request in natural language ("Örnek brief'i kullan" fills the CASE_SPEC brief) | – |
| 2 Kitle | Checks the rule Claude produced (plain-language card + raw JSON, editable) and the audience badges: total, SMS opt-in, e-mail opt-in, reachable | `POST /api/campaigns/preview-segment` |
| 3 Mesajlar | Watches SMS, e-mail subject and body stream in ("typing"); a retry clears the fields and shows the validator's issues | `POST /api/campaigns/compose/stream` (SSE over `fetch`) |
| 4 Onay | Reviews and edits: SMS 160 / subject 80 counters (red when over), SMS encoding and part count (`GSM-7 (160 limit) - 1 SMS` / `UCS-2 (70 limit) - 2 SMS`), `{{first_name}}` chips and a personalised preview, server issues next to each field | `POST /api/campaigns` |
| 5 Gönderim | Live dashboard: progress bar, sent / failed / retried / pending, instantaneous and average msg/s per channel against the 100/s limit, completion badge, resume for interrupted runs | `POST /api/campaigns/{id}/dispatch`, `GET …/dispatch/stream` (EventSource) |

The header polls `GET /healthz` every 10 s (paused while the tab is hidden) and shows green / amber / red, with per-check
details and the LLM mode (mock or Claude).

## Run

Requires Node.js ≥ 20.19 and the backend on `:8080` (`make run` from the repo root, plus `make mock-provider` for dispatch).

```bash
cd frontend
npm install
npm run dev        # http://localhost:5173 — /api and /healthz are proxied to http://localhost:8080
npm test           # Vitest: 218 unit + component tests, no backend needed
npm run build      # production bundle in dist/
npm run preview    # serve dist/ on :4173 with the same proxy
```

From the repo root: `make frontend-install`, `make frontend-dev`, `make test-frontend`, `make frontend-build`.

Set `VITE_API_TARGET` (see `.env.example`) to proxy somewhere else, or `VITE_API_BASE` to call an absolute API origin
from a build served without a proxy (the backend's CORS allow-list covers Vite's 5173/4173 by default).

## Layout

```
src/
  api/client.js              fetch wrapper → ApiError {status, code, issues}, Turkish user messages per error code
  lib/sse.js                 WHATWG event-stream parser for the POST stream (EventSource cannot POST)
  lib/text.js                mirror of the composer validator's counting: code points, {{var}} scan, estimate, preview
  lib/sms.js                 GSM-7 vs UCS-2 detection and SMS part counting (informational; the 160 limit stays in text.js)
  lib/rule.js                plain-language reading of the segment AST
  composables/
    useCampaignFlow.js       wizard state machine: one source of truth, later steps derived from earlier ones
    useComposeStream.js      delta / retry / done / error handling; `done` replaces the typed text
    useDispatchStream.js     EventSource + polling fallback, instantaneous per-channel rate
    useHealth.js             /healthz polling
  components/steps/          BriefStep, AudienceStep, ComposeStep, ReviewStep, DispatchStep
  components/ui/             HealthBadge, StepNav, FieldEditor, CharCounter, SmsPreview, EmailPreview, ThroughputChart, …
tests/                       Vitest (jsdom): parser, counters, SMS encoding, rule text, client, composables, components
```

Design tokens (colours, fonts, radii) live in `src/style.css` under `@theme` — Tailwind v4's CSS-first configuration.
SMS is always teal and e-mail always violet; green / amber / red are reserved for status and always come with an icon
and a label.
