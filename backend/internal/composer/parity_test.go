package composer_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sort"
	"testing"

	"smart-campaign-composer/backend/internal/composer"
)

// textVector is one case of the shared counting / character rules. The same
// file is read by the frontend (frontend/tests/fixtures/text_vectors.json,
// lib/text.js), so the live counters and client-side checks agree with the
// server's validator on every case: trimming (Go's unicode.IsSpace set),
// code-point lengths, the {{first_name}} budget, and forbidden characters
// (tab, control, bidi, U+2028/2029, invisible format characters).
type textVector struct {
	Name       string   `json:"name"`
	Field      string   `json:"field"`
	Input      string   `json:"input"`
	Normalized string   `json:"normalized"`
	Runes      int      `json:"runes"`
	Estimated  int      `json:"estimated"`
	Codes      []string `json:"codes"`
}

const vectorFile = "testdata/text_vectors.json"

// mirroredCodes are the issue codes the client reproduces; markup checks of
// the e-mail body stay server-only.
var mirroredCodes = map[string]bool{"empty": true, "too_long": true, "estimated_too_long": true,
	"unknown_variable": true, "malformed_placeholder": true, "forbidden_character": true}

func TestSharedTextVectors(t *testing.T) {
	raw, err := os.ReadFile(vectorFile)
	if err != nil {
		t.Fatal(err)
	}
	var vectors []textVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) < 50 {
		t.Fatalf("only %d vectors", len(vectors))
	}
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			f := composer.Field(v.Field)
			m := composer.ReferenceMessage
			switch f {
			case composer.FieldSMS:
				m.SMSContent = v.Input
			case composer.FieldEmailSubject:
				m.EmailSubject = v.Input
			case composer.FieldEmailBody:
				m.EmailBody = v.Input
			default:
				t.Fatalf("unknown field %q", v.Field)
			}
			m = composer.Normalize(m)
			if got := m.Get(f); got != v.Normalized {
				t.Errorf("normalized %q, want %q", got, v.Normalized)
			}
			rep, err := composer.DefaultValidator.Validate(m)
			st := map[composer.Field]composer.FieldStats{composer.FieldSMS: rep.SMS,
				composer.FieldEmailSubject: rep.EmailSubject, composer.FieldEmailBody: rep.EmailBody}[f]
			if st.Runes != v.Runes || st.EstimatedRunes != v.Estimated {
				t.Errorf("runes/estimated = %d/%d, want %d/%d", st.Runes, st.EstimatedRunes, v.Runes, v.Estimated)
			}
			set := map[string]bool{}
			for _, is := range composer.IssuesOf(err) {
				if is.Field == f && mirroredCodes[is.Code] {
					set[is.Code] = true
				}
			}
			codes := []string{}
			for c := range set {
				codes = append(codes, c)
			}
			sort.Strings(codes)
			if !equalStrings(codes, v.Codes) {
				t.Errorf("codes %v, want %v", codes, v.Codes)
			}
		})
	}

	// The frontend reads its own copy (its Docker build context does not
	// include the backend). In a full checkout the two must be identical.
	front, err := os.ReadFile("../../../frontend/tests/fixtures/text_vectors.json")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		t.Log("frontend copy not present (backend-only build context); skipping the identity check")
	case err != nil:
		t.Fatal(err)
	case !bytes.Equal(bytes.ReplaceAll(front, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))):
		t.Fatal("frontend/tests/fixtures/text_vectors.json differs from the backend copy; copy " + vectorFile + " over it")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
