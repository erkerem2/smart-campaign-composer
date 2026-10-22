package segment

import "encoding/json"

// NodeType discriminates the two kinds of node in a rule tree.
type NodeType string

const (
	NodeGroup     NodeType = "group"
	NodeCondition NodeType = "condition"
)

// Logic combines the children of a group node.
type Logic string

const (
	LogicAnd Logic = "AND"
	LogicOr  Logic = "OR"
	LogicNot Logic = "NOT" // exactly one child
)

// Operator is a comparison in a condition node.
type Operator string

const (
	OpEquals             Operator = "EQUALS"
	OpNotEquals          Operator = "NOT_EQUALS"
	OpGreaterThan        Operator = "GREATER_THAN"
	OpGreaterThanOrEqual Operator = "GREATER_THAN_OR_EQUAL"
	OpLessThan           Operator = "LESS_THAN"
	OpLessThanOrEqual    Operator = "LESS_THAN_OR_EQUAL"
	OpIn                 Operator = "IN"
	OpNotIn              Operator = "NOT_IN"
	OpBetween            Operator = "BETWEEN" // inclusive, value = [min, max]
)

// AllOperators lists every operator the engine knows, in a stable order.
var AllOperators = []Operator{
	OpEquals, OpNotEquals,
	OpGreaterThan, OpGreaterThanOrEqual, OpLessThan, OpLessThanOrEqual,
	OpIn, OpNotIn, OpBetween,
}

func (o Operator) isList() bool { return o == OpIn || o == OpNotIn || o == OpBetween }

// Node is a validated rule-tree node. Values have already been type-checked
// and normalised: string (canonical city), int64, float64 or bool. For list
// operators (IN, NOT_IN, BETWEEN) the elements are in Values and Value is nil.
//
// A Node is only ever produced by Parse, so code holding a *Node may assume
// it is safe to compile.
type Node struct {
	Type NodeType

	// group
	Logic Logic
	Rules []*Node

	// condition
	Field    string
	Operator Operator
	Value    any
	Values   []any
}

// wireNode is the on-the-wire JSON shape (what the LLM emits).
type wireNode struct {
	Type     NodeType          `json:"type"`
	Logic    Logic             `json:"logic,omitempty"`
	Rules    []json.RawMessage `json:"rules,omitempty"`
	Field    string            `json:"field,omitempty"`
	Operator Operator          `json:"operator,omitempty"`
	Value    json.RawMessage   `json:"value,omitempty"`
}

// MarshalJSON emits the canonical (normalised) form of the rule, e.g. for
// returning the interpreted rule to the UI or persisting it with a campaign.
func (n *Node) MarshalJSON() ([]byte, error) {
	type group struct {
		Type  NodeType `json:"type"`
		Logic Logic    `json:"logic"`
		Rules []*Node  `json:"rules"`
	}
	type cond struct {
		Type     NodeType `json:"type"`
		Field    string   `json:"field"`
		Operator Operator `json:"operator"`
		Value    any      `json:"value"`
	}
	if n.Type == NodeGroup {
		return json.Marshal(group{n.Type, n.Logic, n.Rules})
	}
	v := n.Value
	if n.Operator.isList() {
		v = n.Values
	}
	return json.Marshal(cond{n.Type, n.Field, n.Operator, v})
}
