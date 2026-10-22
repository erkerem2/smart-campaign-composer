package segment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

// Parser turns untrusted JSON (from the LLM or an API client) into a
// validated *Node. It is safe for concurrent use.
type Parser struct{ reg *Registry }

// NewParser binds a parser to a field whitelist.
func NewParser(reg *Registry) *Parser { return &Parser{reg: reg} }

// Parse strictly decodes and validates a rule tree.
//
// Rejected, among others: unknown keys (e.g. "sql", "where"), trailing data
// after the document, null/missing values, wrong JSON types, fields or
// operators not in the registry, out-of-range numbers, non-integral numbers
// for integer fields, unknown cities, oversized lists, too-deep or too-large
// trees. The root must be a group node.
func (p *Parser) Parse(raw []byte) (*Node, error) {
	lim := p.reg.Limits
	if len(raw) == 0 {
		return nil, verr("$", ErrMalformedJSON, "empty document")
	}
	if len(raw) > lim.MaxRawBytes {
		return nil, verr("$", ErrLimitExceeded, "document is %d bytes (max %d)", len(raw), lim.MaxRawBytes)
	}
	if !utf8.Valid(raw) {
		return nil, verr("$", ErrMalformedJSON, "invalid UTF-8")
	}
	st := &parseState{p: p}
	root, err := st.node(raw, "$", 1)
	if err != nil {
		return nil, err
	}
	if root.Type != NodeGroup {
		return nil, verr("$", ErrInvalidNodeType, "root must be a group node")
	}
	return root, nil
}

type parseState struct {
	p          *Parser
	conditions int
}

// decodeStrict decodes exactly one JSON value into v, refusing unknown
// struct fields and any trailing tokens.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after JSON value")
	}
	return nil
}

func classifyDecodeErr(path string, err error) error {
	if strings.Contains(err.Error(), "unknown field") {
		return verr(path, ErrUnknownKey, "%s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return verr(path, ErrMalformedJSON, "%v", err)
}

func (st *parseState) node(raw []byte, path string, depth int) (*Node, error) {
	lim := st.p.reg.Limits
	var w wireNode
	if err := decodeStrict(raw, &w); err != nil {
		return nil, classifyDecodeErr(path, err)
	}
	switch w.Type {
	case NodeGroup:
		if w.Field != "" || w.Operator != "" || w.Value != nil {
			return nil, verr(path, ErrUnknownKey, "group node must not carry field/operator/value")
		}
		if depth > lim.MaxDepth {
			return nil, verr(path, ErrLimitExceeded, "nesting depth %d exceeds %d", depth, lim.MaxDepth)
		}
		switch w.Logic {
		case LogicAnd, LogicOr:
			if len(w.Rules) == 0 {
				return nil, verr(path, ErrInvalidLogic, "%s group needs at least one rule", w.Logic)
			}
		case LogicNot:
			if len(w.Rules) != 1 {
				return nil, verr(path, ErrInvalidLogic, "NOT group needs exactly one rule, got %d", len(w.Rules))
			}
		default:
			return nil, verr(path+".logic", ErrInvalidLogic, "%q (allowed: AND, OR, NOT)", w.Logic)
		}
		n := &Node{Type: NodeGroup, Logic: w.Logic, Rules: make([]*Node, 0, len(w.Rules))}
		for i, child := range w.Rules {
			c, err := st.node(child, fmt.Sprintf("%s.rules[%d]", path, i), depth+1)
			if err != nil {
				return nil, err
			}
			n.Rules = append(n.Rules, c)
		}
		return n, nil

	case NodeCondition:
		if w.Logic != "" || w.Rules != nil {
			return nil, verr(path, ErrUnknownKey, "condition node must not carry logic/rules")
		}
		st.conditions++
		if st.conditions > lim.MaxConditions {
			return nil, verr(path, ErrLimitExceeded, "more than %d conditions", lim.MaxConditions)
		}
		return st.condition(&w, path)

	default:
		return nil, verr(path+".type", ErrInvalidNodeType, "%q (allowed: group, condition)", truncate(string(w.Type)))
	}
}

func (st *parseState) condition(w *wireNode, path string) (*Node, error) {
	spec, ok := st.p.reg.Field(w.Field)
	if !ok {
		return nil, verr(path+".field", ErrUnknownField, "%q", truncate(w.Field))
	}
	if !spec.allows(w.Operator) {
		return nil, verr(path+".operator", ErrUnsupportedOperator, "%q on %s", truncate(string(w.Operator)), spec.Name)
	}
	vpath := path + ".value"
	if w.Value == nil || bytes.Equal(bytes.TrimSpace(w.Value), []byte("null")) {
		return nil, verr(vpath, ErrInvalidValue, "value is required")
	}
	n := &Node{Type: NodeCondition, Field: spec.Name, Operator: w.Operator}

	if !w.Operator.isList() {
		v, err := scalar(spec, w.Value, vpath)
		if err != nil {
			return nil, err
		}
		n.Value = v
		return n, nil
	}

	var items []json.RawMessage
	if err := decodeStrict(w.Value, &items); err != nil {
		return nil, verr(vpath, ErrInvalidValue, "%s expects a JSON array", w.Operator)
	}
	switch {
	case w.Operator == OpBetween && len(items) != 2:
		return nil, verr(vpath, ErrInvalidValue, "BETWEEN expects exactly [min, max]")
	case len(items) == 0:
		return nil, verr(vpath, ErrInvalidValue, "%s list is empty", w.Operator)
	case len(items) > st.p.reg.Limits.MaxListSize:
		return nil, verr(vpath, ErrLimitExceeded, "%s list has %d items (max %d)", w.Operator, len(items), st.p.reg.Limits.MaxListSize)
	}
	seen := map[any]bool{}
	for i, it := range items {
		v, err := scalar(spec, it, fmt.Sprintf("%s[%d]", vpath, i))
		if err != nil {
			return nil, err
		}
		if w.Operator != OpBetween {
			if seen[v] { // "Istanbul" and "İstanbul" collapse to one bind arg
				continue
			}
			seen[v] = true
		}
		n.Values = append(n.Values, v)
	}
	if w.Operator == OpBetween && toFloat(n.Values[0]) > toFloat(n.Values[1]) {
		return nil, verr(vpath, ErrInvalidValue, "BETWEEN min is greater than max")
	}
	return n, nil
}

// scalar decodes one JSON scalar and checks it against the field spec.
// JSON types must match exactly: "30" is not accepted for an integer field,
// and 1 is not accepted for a boolean.
func scalar(spec *FieldSpec, raw json.RawMessage, path string) (any, error) {
	var v any
	if err := decodeStrict(raw, &v); err != nil {
		return nil, verr(path, ErrInvalidValue, "not a JSON scalar")
	}
	switch spec.Type {
	case TypeString:
		s, ok := v.(string)
		if !ok {
			return nil, verr(path, ErrInvalidValue, "%s expects a string", spec.Name)
		}
		if s == "" || len(s) > maxStringLen {
			return nil, verr(path, ErrInvalidValue, "%s length must be 1..%d", spec.Name, maxStringLen)
		}
		if spec.Normalize != nil {
			ns, err := spec.Normalize(s)
			if err != nil {
				return nil, verr(path, ErrInvalidValue, "%s: %v", spec.Name, err)
			}
			s = ns
		}
		return s, nil

	case TypeBool:
		b, ok := v.(bool)
		if !ok {
			return nil, verr(path, ErrInvalidValue, "%s expects true/false", spec.Name)
		}
		return b, nil

	case TypeInt, TypeFloat:
		num, ok := v.(json.Number)
		if !ok {
			return nil, verr(path, ErrInvalidValue, "%s expects a number", spec.Name)
		}
		f, err := num.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, verr(path, ErrInvalidValue, "%s: unparsable number", spec.Name)
		}
		if f < spec.Min || f > spec.Max {
			return nil, verr(path, ErrInvalidValue, "%s must be within [%g, %g], got %g", spec.Name, spec.Min, spec.Max, f)
		}
		if spec.Type == TypeInt {
			if f != math.Trunc(f) {
				return nil, verr(path, ErrInvalidValue, "%s expects an integer, got %g", spec.Name, f)
			}
			return int64(f), nil
		}
		return f, nil
	}
	return nil, verr(path, ErrInvalidValue, "unsupported field type %q", spec.Type)
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

// truncate keeps attacker-controlled strings in error messages short.
func truncate(s string) string {
	const max = 40
	if len(s) <= max {
		return s
	}
	for i := max; i > 0; i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i] + "…"
		}
	}
	return "…"
}
