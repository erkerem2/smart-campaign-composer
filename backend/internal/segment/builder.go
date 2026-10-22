package segment

import (
	"fmt"
	"strconv"
	"strings"
)

// Dialect abstracts the few places SQL engines differ.
type Dialect interface {
	Name() string
	Placeholder(n int) string // n is 1-based
	BoolArg(b bool) any
}

// SQLite uses positional "?" placeholders and stores booleans as 0/1.
type SQLite struct{}

func (SQLite) Name() string           { return "sqlite" }
func (SQLite) Placeholder(int) string { return "?" }
func (SQLite) BoolArg(b bool) any {
	if b {
		return int64(1)
	}
	return int64(0)
}

// Postgres uses numbered "$n" placeholders and native booleans.
type Postgres struct{}

func (Postgres) Name() string             { return "postgres" }
func (Postgres) Placeholder(n int) string { return "$" + strconv.Itoa(n) }
func (Postgres) BoolArg(b bool) any       { return b }

// sqlOps is the only source of SQL operator text. Nothing from the input is
// ever concatenated into the query.
var sqlOps = map[Operator]string{
	OpEquals:             "=",
	OpNotEquals:          "<>",
	OpGreaterThan:        ">",
	OpGreaterThanOrEqual: ">=",
	OpLessThan:           "<",
	OpLessThanOrEqual:    "<=",
	OpIn:                 "IN",
	OpNotIn:              "NOT IN",
	OpBetween:            "BETWEEN",
}

// Where is a compiled, parameterised WHERE clause body.
type Where struct {
	SQL  string
	Args []any
}

// Builder compiles validated rule trees into parameterised SQL.
type Builder struct {
	reg     *Registry
	dialect Dialect
}

func NewBuilder(reg *Registry, d Dialect) *Builder { return &Builder{reg: reg, dialect: d} }

// Build compiles a tree produced by Parse. It re-checks fields and operators
// against the registry (defence in depth: a *Node could in theory be built
// by hand elsewhere in the codebase).
func (b *Builder) Build(root *Node) (Where, error) {
	c := &compiler{b: b}
	var sb strings.Builder
	if err := c.node(&sb, root); err != nil {
		return Where{}, err
	}
	return Where{SQL: sb.String(), Args: c.args}, nil
}

type compiler struct {
	b    *Builder
	args []any
}

func (c *compiler) bind(v any) string {
	if bv, ok := v.(bool); ok {
		v = c.b.dialect.BoolArg(bv)
	}
	c.args = append(c.args, v)
	return c.b.dialect.Placeholder(len(c.args))
}

func (c *compiler) node(sb *strings.Builder, n *Node) error {
	if n == nil {
		return fmt.Errorf("segment: nil node")
	}
	switch n.Type {
	case NodeGroup:
		if len(n.Rules) == 0 {
			return fmt.Errorf("segment: empty group")
		}
		switch n.Logic {
		case LogicNot:
			if len(n.Rules) != 1 {
				return fmt.Errorf("segment: NOT needs exactly one rule")
			}
			sb.WriteString("NOT ")
			return c.wrapped(sb, n.Rules[0])
		case LogicAnd, LogicOr:
			sb.WriteByte('(')
			for i, r := range n.Rules {
				if i > 0 {
					sb.WriteString(" " + string(n.Logic) + " ")
				}
				if err := c.node(sb, r); err != nil {
					return err
				}
			}
			sb.WriteByte(')')
			return nil
		}
		return fmt.Errorf("segment: bad logic %q", n.Logic)

	case NodeCondition:
		spec, ok := c.b.reg.Field(n.Field)
		if !ok || !spec.allows(n.Operator) {
			return fmt.Errorf("segment: %q/%q not whitelisted", n.Field, n.Operator)
		}
		op := sqlOps[n.Operator]
		col := spec.Column
		switch n.Operator {
		case OpIn, OpNotIn:
			if len(n.Values) == 0 {
				return fmt.Errorf("segment: empty IN list")
			}
			ph := make([]string, len(n.Values))
			for i, v := range n.Values {
				ph[i] = c.bind(v)
			}
			fmt.Fprintf(sb, "%s %s (%s)", col, op, strings.Join(ph, ", "))
		case OpBetween:
			if len(n.Values) != 2 {
				return fmt.Errorf("segment: BETWEEN needs two values")
			}
			lo, hi := c.bind(n.Values[0]), c.bind(n.Values[1])
			fmt.Fprintf(sb, "%s BETWEEN %s AND %s", col, lo, hi)
		default:
			fmt.Fprintf(sb, "%s %s %s", col, op, c.bind(n.Value))
		}
		return nil
	}
	return fmt.Errorf("segment: bad node type %q", n.Type)
}

func (c *compiler) wrapped(sb *strings.Builder, n *Node) error {
	if n.Type == NodeGroup && n.Logic != LogicNot {
		return c.node(sb, n) // already parenthesised
	}
	sb.WriteByte('(')
	if err := c.node(sb, n); err != nil {
		return err
	}
	sb.WriteByte(')')
	return nil
}
