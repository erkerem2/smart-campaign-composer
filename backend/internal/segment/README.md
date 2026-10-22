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

## Wiring into the existing project

- **Schema:** the registry expects `users(city, age, total_spend, cart_last_active_days,
  has_purchased_since_cart, opt_in_sms, opt_in_email)`. If some of these are derived
  (e.g. from orders/cart_events), create a VIEW and pass its name as the registry table.
  Column names must be plain identifiers — `NewRegistry` rejects expressions on purpose.
- **City normalisation on write:** call `segment.NormalizeCity` in the seeder/ingest path
  so the column only holds canonical names. Rule values are normalised on read too.
- **Seed counts:** tests use their own deterministic 3,000-user fixture
  (`seed_helpers_test.go`). Swap `openSeededDB` for the real seeder and update the pinned
  `wantReference`; the oracle comparison keeps working unchanged.
- **Driver:** only tests import one (`sqlite_driver_test.go`); the package itself uses
  `database/sql` only.

## Tests

```
go test ./internal/segment -race
go test ./internal/segment -run=^$ -fuzz=FuzzParseBuild -fuzztime=30s
```
