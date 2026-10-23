package segment

import (
	"fmt"
	"regexp"
	"slices"
)

// FieldType is the value type a field accepts.
type FieldType string

const (
	TypeString FieldType = "string"
	TypeInt    FieldType = "integer"
	TypeFloat  FieldType = "number"
	TypeBool   FieldType = "boolean"
)

// FieldSpec whitelists one queryable field.
type FieldSpec struct {
	Name        string // name the LLM / API uses
	Column      string // SQL column; must be a plain identifier (enforced)
	Type        FieldType
	Operators   []Operator
	Min, Max    float64 // inclusive numeric bounds (ignored for string/bool)
	Description string  // shown to the LLM in the prompt and JSON Schema

	// Normalize canonicalises string values (e.g. NormalizeCity) and is
	// expected to reject anything outside the allowed domain.
	Normalize func(string) (string, error)
}

func (f *FieldSpec) allows(op Operator) bool { return slices.Contains(f.Operators, op) }

// Limits bound the size of any accepted rule tree.
type Limits struct {
	MaxDepth      int // group nesting depth (root group = 1)
	MaxConditions int // total condition nodes
	MaxListSize   int // elements in IN / NOT_IN
	MaxRawBytes   int // size of the raw JSON document
}

// DefaultLimits are deliberately tight: real campaign briefs need a handful
// of conditions; anything bigger is far more likely to be abuse or a
// hallucination than a legitimate segment.
var DefaultLimits = Limits{MaxDepth: 4, MaxConditions: 20, MaxListSize: 50, MaxRawBytes: 16 << 10}

const maxStringLen = 64

var identRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Registry is the whitelist of fields, plus the table they live in.
type Registry struct {
	Table      string // table or VIEW name; plain identifier (enforced)
	SMSColumn  string // boolean opt-in columns used by the counter
	EmailCol   string
	Limits     Limits
	fields     map[string]*FieldSpec
	fieldOrder []string
}

// NewRegistry validates the configuration up front so a typo (or a
// deliberately smuggled expression) in a column name fails at startup, not
// at query time.
func NewRegistry(table, smsCol, emailCol string, limits Limits, specs ...FieldSpec) (*Registry, error) {
	for _, id := range []string{table, smsCol, emailCol} {
		if !identRe.MatchString(id) {
			return nil, fmt.Errorf("segment: %q is not a safe SQL identifier", id)
		}
	}
	r := &Registry{Table: table, SMSColumn: smsCol, EmailCol: emailCol, Limits: limits, fields: map[string]*FieldSpec{}}
	for i := range specs {
		s := specs[i]
		if !identRe.MatchString(s.Name) || !identRe.MatchString(s.Column) {
			return nil, fmt.Errorf("segment: field %q / column %q is not a safe identifier", s.Name, s.Column)
		}
		if _, dup := r.fields[s.Name]; dup {
			return nil, fmt.Errorf("segment: duplicate field %q", s.Name)
		}
		if len(s.Operators) == 0 {
			return nil, fmt.Errorf("segment: field %q has no operators", s.Name)
		}
		for _, op := range s.Operators {
			if _, ok := sqlOps[op]; !ok {
				return nil, fmt.Errorf("segment: field %q uses unknown operator %q", s.Name, op)
			}
		}
		r.fields[s.Name] = &s
		r.fieldOrder = append(r.fieldOrder, s.Name)
	}
	return r, nil
}

// MustRegistry is NewRegistry that panics; for package-level defaults.
func MustRegistry(table, smsCol, emailCol string, limits Limits, specs ...FieldSpec) *Registry {
	r, err := NewRegistry(table, smsCol, emailCol, limits, specs...)
	if err != nil {
		panic(err)
	}
	return r
}

// Field looks up a whitelisted field.
func (r *Registry) Field(name string) (*FieldSpec, bool) {
	f, ok := r.fields[name]
	return f, ok
}

// Fields returns the field specs in declaration order.
func (r *Registry) Fields() []*FieldSpec {
	out := make([]*FieldSpec, 0, len(r.fieldOrder))
	for _, n := range r.fieldOrder {
		out = append(out, r.fields[n])
	}
	return out
}

var (
	cmpOps  = []Operator{OpEquals, OpNotEquals, OpGreaterThan, OpGreaterThanOrEqual, OpLessThan, OpLessThanOrEqual, OpBetween}
	setOps  = []Operator{OpEquals, OpNotEquals, OpIn, OpNotIn}
	boolOps = []Operator{OpEquals, OpNotEquals}
)

// DefaultRegistry is the whitelist for the v_users_segment VIEW
// (internal/repository/schema.sql). The view derives the time-relative
// attributes (cart_last_active_days, has_purchased_since_cart) from the
// timestamps stored in users at query time, and exposes no PII. Column names
// here are deliberately restricted to plain identifiers so no SQL expression
// can ever be configured in by accident; the derivation lives in the view.
var DefaultRegistry = MustRegistry("v_users_segment", "opt_in_sms", "opt_in_email", DefaultLimits,
	FieldSpec{
		Name: "city", Column: "city", Type: TypeString, Operators: setOps,
		Normalize:   NormalizeCity,
		Description: "Kullanıcının yaşadığı il (Türkiye'nin 81 ilinden biri, örn. \"İstanbul\").",
	},
	FieldSpec{
		Name: "age", Column: "age", Type: TypeInt, Operators: cmpOps, Min: 13, Max: 120,
		Description: "Kullanıcının yaşı (tam sayı).",
	},
	FieldSpec{
		Name: "total_spend", Column: "total_spend", Type: TypeFloat, Operators: cmpOps, Min: 0, Max: 1e9,
		Description: "Kullanıcının toplam harcaması (TL).",
	},
	FieldSpec{
		Name: "cart_last_active_days", Column: "cart_last_active_days", Type: TypeInt, Operators: cmpOps, Min: 0, Max: 3650,
		Description: "Sepete son ürün eklemesinin üzerinden geçen gün sayısı (yukarı yuvarlanır: 0-24 saat önce = 1). Hiç sepet aktivitesi olmayanlarda boştur. \"Son 30 günde sepete ekleyen\" → LESS_THAN_OR_EQUAL 30.",
	},
	FieldSpec{
		Name: "has_purchased_since_cart", Column: "has_purchased_since_cart", Type: TypeBool, Operators: boolOps,
		Description: "Son sepet aktivitesinden sonra (veya aynı anda) alışveriş yaptı mı. Hiç sepeti olmayanlarda false'tur; bu yüzden sepet şartını cart_last_active_days ile ayrıca ver. \"Sepeti terk eden / alışveriş yapmayan\" → EQUALS false.",
	},
)
