package segment

import (
	"errors"
	"fmt"
)

// Sentinel errors. Every validation failure wraps exactly one of these so
// callers (HTTP handlers, metrics) can classify with errors.Is.
var (
	ErrMalformedJSON       = errors.New("malformed rule json")
	ErrUnknownKey          = errors.New("unknown key in rule node")
	ErrInvalidNodeType     = errors.New("invalid node type")
	ErrInvalidLogic        = errors.New("invalid group logic")
	ErrUnknownField        = errors.New("field is not allowed")
	ErrUnsupportedOperator = errors.New("operator is not allowed for field")
	ErrInvalidValue        = errors.New("invalid value for field")
	ErrLimitExceeded       = errors.New("rule exceeds safety limits")

	ErrEmptyBrief   = errors.New("campaign brief is empty")
	ErrBriefTooLong = errors.New("campaign brief is too long")
	ErrLLM          = errors.New("llm call failed")
)

// ValidationError pinpoints where in the tree validation failed.
// Path uses a JSONPath-like notation, e.g. "$.rules[1].value".
type ValidationError struct {
	Path   string
	Kind   error // one of the sentinel errors above
	Detail string
}

func (e *ValidationError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("%s: %v", e.Path, e.Kind)
	}
	return fmt.Sprintf("%s: %v: %s", e.Path, e.Kind, e.Detail)
}

func (e *ValidationError) Unwrap() error { return e.Kind }

func verr(path string, kind error, format string, args ...any) error {
	return &ValidationError{Path: path, Kind: kind, Detail: fmt.Sprintf(format, args...)}
}
