package composer

import (
	"errors"
	"fmt"
	"strings"
)

// Request-level sentinel errors.
var (
	ErrEmptyBrief      = errors.New("campaign brief is empty")
	ErrBriefTooLong    = errors.New("campaign brief is too long")
	ErrLLM             = errors.New("llm call failed")
	ErrMalformedOutput = errors.New("malformed llm output")
	// ErrInvalidContent is the umbrella for every content validation failure.
	ErrInvalidContent = errors.New("content failed validation")
)

// Field-level sentinel errors. Every Issue wraps exactly one of these so
// callers can classify with errors.Is.
var (
	ErrEmptyField           = errors.New("field is empty")
	ErrTooLong              = errors.New("exceeds channel character limit")
	ErrEstimatedTooLong     = errors.New("estimated rendered length exceeds channel limit")
	ErrTooLarge             = errors.New("exceeds safety size cap")
	ErrUnknownVariable      = errors.New("unknown personalization variable")
	ErrMalformedPlaceholder = errors.New("malformed placeholder")
	ErrForbiddenCharacter   = errors.New("forbidden character")
	ErrUnsafeMarkup         = errors.New("unsafe or unsupported markup")
	ErrRenderedTooLong      = errors.New("rendered message exceeds channel limit")
)

// issueCodes are the stable, UI-facing identifiers of the sentinels.
var issueCodes = map[error]string{
	ErrEmptyField:           "empty",
	ErrTooLong:              "too_long",
	ErrEstimatedTooLong:     "estimated_too_long",
	ErrTooLarge:             "too_large",
	ErrUnknownVariable:      "unknown_variable",
	ErrMalformedPlaceholder: "malformed_placeholder",
	ErrForbiddenCharacter:   "forbidden_character",
	ErrUnsafeMarkup:         "unsafe_markup",
	ErrRenderedTooLong:      "rendered_too_long",
}

// Issue is one validation finding on one field.
type Issue struct {
	Field   Field  `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Kind    error  `json:"-"`
}

func newIssue(f Field, kind error, format string, args ...any) Issue {
	return Issue{Field: f, Code: issueCodes[kind], Message: fmt.Sprintf(format, args...), Kind: kind}
}

func (i Issue) Error() string { return fmt.Sprintf("%s: %v: %s", i.Field, i.Kind, i.Message) }
func (i Issue) Unwrap() error { return i.Kind }

// ValidationError collects every issue found in a message, so the UI (and
// the model, on retry) sees all problems at once instead of one per round.
type ValidationError struct {
	Issues []Issue
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Issues))
	for i, is := range e.Issues {
		parts[i] = is.Error()
	}
	return "content failed validation: " + strings.Join(parts, "; ")
}

// Unwrap exposes ErrInvalidContent and every issue (and through them their
// sentinel kinds) to errors.Is / errors.As.
func (e *ValidationError) Unwrap() []error {
	errs := make([]error, 0, len(e.Issues)+1)
	errs = append(errs, ErrInvalidContent)
	for _, is := range e.Issues {
		errs = append(errs, is)
	}
	return errs
}

// RejectedError is returned when every generation attempt failed decoding or
// validation. Last is the final attempt (zero if it could not be decoded) so
// a human can still edit and resubmit it.
type RejectedError struct {
	Attempts int
	Last     *Message
	Err      error
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("llm output rejected after %d attempt(s): %v", e.Attempts, e.Err)
}

func (e *RejectedError) Unwrap() error { return e.Err }

// IssuesOf extracts the validation issues from err (nil if there are none).
func IssuesOf(err error) []Issue {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Issues
	}
	return nil
}
