# internal/segment — Safe Segment Engine & Audience Counter

```
brief ─► LLMClient (Mock | Anthropic, forced tool call) ─► raw JSON (untrusted)
      ─► Parser.Parse  (strict decode + whitelist validation + city normalisation)
      ─► Builder.Build (parameterised WHERE: SQLite "?" / Postgres "$n")
      ─► AudienceCounter.Count (one aggregate query) ─► Preview{rule, audience}
```

## Usage

```go
llm, _ := segment.NewAnthropicClientFromEnv()          // or segment.NewMockLLMClient()
eng := segment.NewEngine(llm, db, segment.DefaultRegistry, segment.SQLite{})

p, err := eng.Preview(ctx, brief)          // brief → rule → counts
p, err  = eng.PreviewJSON(ctx, ruleJSON)   // UI-edited rule, no LLM call
// p.Rule marshals to canonical JSON; p.Audience = {total, sms_opt_in, email_opt_in, reachable}
```

Map errors with `errors.Is` (`ErrUnknownField`, `ErrUnsupportedOperator`, `ErrInvalidValue`,
`ErrUnknownKey`, `ErrLimitExceeded`, `ErrMalformedJSON` → 422; `ErrLLM` → 502;
`ErrEmptyBrief`/`ErrBriefTooLong` → 400). `*ValidationError.Path` says where (`$.rules[1].field`).

## Rule tree

```json
{"type":"group","logic":"AND","rules":[
  {"type":"condition","field":"city","operator":"EQUALS","value":"İstanbul"},
  {"type":"condition","field":"cart_last_active_days","operator":"LESS_THAN_OR_EQUAL","value":30},
  {"type":"condition","field":"has_purchased_since_cart","operator":"EQUALS","value":false}]}
```

| Field | Type | Operators |
|---|---|---|
| city | string (81 provinces, normalised) | EQUALS, NOT_EQUALS, IN, NOT_IN |
| age | int [13,120] | EQUALS … LESS_THAN_OR_EQUAL, BETWEEN |
| total_spend | number [0,1e9] | same as age |
| cart_last_active_days | int [0,3650], NULL = no cart | same as age |
| has_purchased_since_cart | bool | EQUALS, NOT_EQUALS |

Logic: `AND`, `OR`, `NOT` (exactly one child). Limits: depth 4, 20 conditions, 50 list items, 16 KB.

## Storage: the `v_users_segment` view (ADR 008)

`DefaultRegistry` reads the view `v_users_segment` (defined in `internal/repository/schema.sql`,
recreated by every `repository.Migrate`), never the `users` table. The view exposes `id`, `city`,
`age`, `total_spend`, the opt-in flags and two attributes derived from stored timestamps at query time:

| column | derivation |
|---|---|
| `cart_last_active_days` | whole days since `cart_last_active_at`, **rounded up** (integer seconds, clamped at 0); NULL without cart activity. `<= 30` ⇔ activity within the last 30 × 24 h |
| `has_purchased_since_cart` | 1 if `last_purchase_at >= cart_last_active_at`, else 0 (also 0 without a cart) |

No PII (name, e-mail, phone) is visible through the view. `repository.TestSegmentViewMatchesRegistryAndHidesPII`
fails if the view and the registry drift apart.

- **Dispatch:** `Engine.MatchingIDs(ctx, rule)` returns the matching user ids (opt-in not applied;
  the dispatcher checks it per channel). `Engine.Validate(raw)` re-validates a stored or edited rule.
- **City normalisation on write:** call `segment.NormalizeCity` in the seeder/ingest path
  so the column only holds canonical names. Rule values are normalised on read too.
- **Driver:** only tests import one (`sqlite_driver_test.go`); the package itself uses
  `database/sql` only.

## Tests

```
go test ./internal/segment -race          # unit + fixture + real 3,000-user seed through the view
go test ./internal/segment -run=^$ -fuzz=FuzzParseBuild -fuzztime=30s
```
