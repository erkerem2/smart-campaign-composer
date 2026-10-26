package segment

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"smart-campaign-composer/backend/internal/textguard"
)

// ToolName is the tool the LLM must call to return a rule.
const ToolName = "emit_segment_rule"

// MaxBriefRunes caps the brief length sent to the LLM, in code points after
// textguard.CleanBrief.
const MaxBriefRunes = textguard.MaxBriefRunes

// Preview is what the UI shows the marketer before creating a campaign.
type Preview struct {
	Brief    string         `json:"brief"`
	Rule     *Node          `json:"rule"` // canonical, validated rule tree
	Audience AudienceCounts `json:"audience"`
}

// Engine wires LLM → Parser → Builder → Counter.
type Engine struct {
	LLM          LLMClient
	Parser       *Parser
	Counter      *AudienceCounter
	reg          *Registry
	systemPrompt string
	schema       map[string]any
	QueryTimeout time.Duration
}

func NewEngine(llm LLMClient, db *sql.DB, reg *Registry, d Dialect) *Engine {
	return &Engine{
		LLM:          llm,
		Parser:       NewParser(reg),
		Counter:      NewAudienceCounter(db, reg, d),
		reg:          reg,
		systemPrompt: reg.SystemPrompt(),
		schema:       reg.JSONSchema(),
		QueryTimeout: 5 * time.Second,
	}
}

// Translate asks the LLM for a rule and validates it. A rule that fails
// validation is never executed; the ValidationError is returned so it can be
// logged / shown ("model used unsupported field X").
func (e *Engine) Translate(ctx context.Context, brief string) (*Node, error) {
	// Invisible characters are removed before anything else: they could
	// carry instructions the marketer cannot see (Unicode tag characters
	// spell ASCII invisibly), and a brief made only of them is empty.
	brief = textguard.CleanBrief(brief)
	if brief == "" {
		return nil, ErrEmptyBrief
	}
	if textguard.BriefRunes(brief) > MaxBriefRunes {
		return nil, ErrBriefTooLong
	}
	raw, err := e.LLM.GenerateRule(ctx, RuleRequest{
		Brief: brief, SystemPrompt: e.systemPrompt, ToolName: ToolName, Schema: e.schema,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLLM, err)
	}
	return e.Parser.Parse(raw)
}

// Preview translates a brief and counts the matching audience.
func (e *Engine) Preview(ctx context.Context, brief string) (*Preview, error) {
	rule, err := e.Translate(ctx, brief)
	if err != nil {
		return nil, err
	}
	counts, err := e.CountRule(ctx, rule)
	if err != nil {
		return nil, err
	}
	return &Preview{Brief: textguard.CleanBrief(brief), Rule: rule, Audience: counts}, nil
}

// PreviewJSON counts an audience for a rule edited directly in the UI
// (no LLM round-trip). The JSON gets the same strict validation.
func (e *Engine) PreviewJSON(ctx context.Context, rawRule []byte) (*Preview, error) {
	rule, err := e.Parser.Parse(rawRule)
	if err != nil {
		return nil, err
	}
	counts, err := e.CountRule(ctx, rule)
	if err != nil {
		return nil, err
	}
	return &Preview{Rule: rule, Audience: counts}, nil
}

// Validate parses and validates a rule tree from JSON (a rule edited in the
// UI, or one read back from the database) with the same strict rules as LLM
// output. It never touches the database.
func (e *Engine) Validate(rawRule []byte) (*Node, error) { return e.Parser.Parse(rawRule) }

// MatchingIDs returns the ids of the users a validated rule selects.
func (e *Engine) MatchingIDs(ctx context.Context, rule *Node) ([]int64, error) {
	if e.QueryTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.QueryTimeout)
		defer cancel()
	}
	return e.Counter.MatchingIDs(ctx, rule)
}

// CountRule counts the audience of a validated rule.
func (e *Engine) CountRule(ctx context.Context, rule *Node) (AudienceCounts, error) {
	if e.QueryTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.QueryTimeout)
		defer cancel()
	}
	return e.Counter.Count(ctx, rule)
}
