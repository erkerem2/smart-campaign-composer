// Package segment implements the Safe Segment Engine: it turns a marketer's
// natural-language campaign brief into an audience rule and counts how many
// users (and how many reachable via SMS / e-mail) match it.
//
// Security model — the LLM is treated as an untrusted input source:
//
//  1. The LLM never produces SQL. It may only emit a JSON rule tree (AST) whose
//     shape is described by the JSON Schema returned from Registry.JSONSchema.
//  2. Parse decodes that JSON strictly (unknown keys, trailing data, wrong
//     types, null values are rejected) and validates every node against a
//     whitelist Registry: allowed fields, per-field allowed operators, value
//     types, numeric ranges, list sizes, tree depth and node count.
//  3. City values are normalised ("Istanbul", "ISTANBUL", "İstanbul" →
//     "İstanbul") and must resolve to one of Türkiye's 81 provinces.
//  4. Build compiles the validated tree into a parameterised WHERE clause.
//     Column names and SQL operators come exclusively from Go constants in
//     the Registry; user/LLM values only ever travel as bind arguments.
//  5. AudienceCounter runs one aggregate query returning total, SMS opt-in,
//     e-mail opt-in and reachable (either channel) counts.
//
// Flow:  brief ─► LLMClient (Mock | Anthropic) ─► raw JSON ─► Parse/Validate
//
//	─► Build (?, $n) ─► AudienceCounter ─► Preview
package segment
