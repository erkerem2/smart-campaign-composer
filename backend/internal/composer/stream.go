package composer

import (
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// FieldDelta is newly decoded text for one field.
type FieldDelta struct {
	Field Field
	Text  string
}

// fieldExtractor incrementally decodes a stream of JSON fragments of the
// form {"sms_content":"...","email_subject":"...","email_body":"..."} and
// yields the decoded string contents per field as they arrive, so the UI can
// show each field "typing" instead of raw JSON.
//
// It is a display aid only: it is lenient, stops emitting (passive mode) on
// anything unexpected, and its output is never trusted. The authoritative
// content is the strict decode of the complete JSON after the stream ends.
//
// Fragment boundaries may fall anywhere — inside keys, inside escapes such
// as ç, between the two halves of a surrogate pair or inside a UTF-8
// sequence — and the extractor yields the same text either way.
type fieldExtractor struct {
	state   exState
	key     []byte // decoded key being read
	field   Field  // field of the current value ("" = ignored key)
	emitted map[Field]bool

	// string decoding
	esc     bool
	hex     []byte
	pending rune // high surrogate waiting for its low half

	out     []FieldDelta
	buf     []byte // undelivered text of the current field
	inValue bool
}

type exState int

const (
	exBeforeObject exState = iota
	exBeforeKey            // expect '"' or '}'
	exInKey
	exAfterKey // expect ':'
	exBeforeValue
	exInValue
	exAfterValue // expect ',' or '}'
	exDone
	exPassive
)

func newFieldExtractor() *fieldExtractor {
	return &fieldExtractor{emitted: map[Field]bool{}}
}

// Feed consumes one fragment and returns the text decoded from it, merged
// per field in order.
func (x *fieldExtractor) Feed(s string) []FieldDelta {
	x.out = x.out[:0]
	for i := 0; i < len(s) && x.state != exPassive && x.state != exDone; i++ {
		x.step(s[i])
	}
	x.flush(false)
	return append([]FieldDelta(nil), x.out...)
}

func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (x *fieldExtractor) step(c byte) {
	switch x.state {
	case exBeforeObject:
		if c == '{' {
			x.state = exBeforeKey
		} else if !isJSONSpace(c) {
			x.state = exPassive
		}
	case exBeforeKey:
		switch {
		case c == '"':
			x.key = x.key[:0]
			x.state = exInKey
		case c == '}':
			x.state = exDone
		case !isJSONSpace(c):
			x.state = exPassive
		}
	case exInKey:
		if x.stringByte(c) {
			x.key = append(x.key, x.buf...)
			x.buf = x.buf[:0]
			f := Field(x.key)
			x.field = ""
			if f.valid() {
				if x.emitted[f] {
					// Duplicate key: the strict decoder will reject it; do not
					// show two versions of a field.
					x.state = exPassive
					return
				}
				x.emitted[f] = true
				x.field = f
			}
			x.state = exAfterKey
		}
	case exAfterKey:
		if c == ':' {
			x.state = exBeforeValue
		} else if !isJSONSpace(c) {
			x.state = exPassive
		}
	case exBeforeValue:
		switch {
		case c == '"':
			x.state = exInValue
			x.inValue = true
		case !isJSONSpace(c):
			x.state = exPassive // non-string value: strict decode will reject
		}
	case exInValue:
		if x.stringByte(c) {
			x.flush(true)
			x.inValue = false
			x.state = exAfterValue
		}
	case exAfterValue:
		switch {
		case c == ',':
			x.state = exBeforeKey
		case c == '}':
			x.state = exDone
		case !isJSONSpace(c):
			x.state = exPassive
		}
	}
}

// stringByte decodes one byte of a JSON string body into the key or value
// buffer. It returns true on the closing quote.
func (x *fieldExtractor) stringByte(c byte) bool {
	if len(x.hex) > 0 || (x.esc && c == 'u') {
		if x.esc {
			x.esc = false
			x.hex = append(x.hex[:0], 'u')
			return false
		}
		x.hex = append(x.hex, c)
		if len(x.hex) == 5 {
			v, err := strconv.ParseUint(string(x.hex[1:]), 16, 32)
			x.hex = x.hex[:0]
			if err != nil {
				x.state = exPassive
				return false
			}
			x.unicodeEscape(rune(v))
		}
		return false
	}
	if x.esc {
		x.esc = false
		var r rune
		switch c {
		case '"', '\\', '/':
			r = rune(c)
		case 'b':
			r = '\b'
		case 'f':
			r = '\f'
		case 'n':
			r = '\n'
		case 'r':
			r = '\r'
		case 't':
			r = '\t'
		default:
			x.state = exPassive
			return false
		}
		x.writeRune(r)
		return false
	}
	switch c {
	case '\\':
		x.esc = true
		return false
	case '"':
		x.dropPendingSurrogate()
		return true
	}
	x.dropPendingSurrogate()
	x.writeByte(c)
	return false
}

func (x *fieldExtractor) unicodeEscape(r rune) {
	if x.pending != 0 {
		hi := x.pending
		x.pending = 0
		if utf16.IsSurrogate(r) && r >= 0xDC00 {
			x.writeRuneRaw(utf16.DecodeRune(hi, r))
			return
		}
		x.writeRuneRaw(utf8.RuneError)
	}
	switch {
	case r >= 0xD800 && r < 0xDC00:
		x.pending = r
	case utf16.IsSurrogate(r):
		x.writeRuneRaw(utf8.RuneError)
	default:
		x.writeRuneRaw(r)
	}
}

// dropPendingSurrogate emits U+FFFD for a lone high surrogate, matching
// encoding/json.
func (x *fieldExtractor) dropPendingSurrogate() {
	if x.pending != 0 {
		x.pending = 0
		x.writeRuneRaw(utf8.RuneError)
	}
}

func (x *fieldExtractor) writeRune(r rune) {
	x.dropPendingSurrogate()
	x.writeRuneRaw(r)
}

func (x *fieldExtractor) writeRuneRaw(r rune) {
	x.buf = utf8.AppendRune(x.buf, r)
}

func (x *fieldExtractor) writeByte(c byte) { x.buf = append(x.buf, c) }

// flush moves decoded value text to out. Keys are kept in key, not buf.
// Unless final, an incomplete trailing UTF-8 sequence is held back for the
// next fragment.
func (x *fieldExtractor) flush(final bool) {
	if x.state == exInKey {
		x.key = append(x.key, x.buf...)
		x.buf = x.buf[:0]
		return
	}
	if !x.inValue || len(x.buf) == 0 {
		x.buf = x.buf[:0]
		return
	}
	n := len(x.buf)
	if !final {
		n = completeUTF8Prefix(x.buf)
	}
	if n > 0 && x.field != "" {
		text := string(x.buf[:n])
		if k := len(x.out); k > 0 && x.out[k-1].Field == x.field {
			x.out[k-1].Text += text
		} else {
			x.out = append(x.out, FieldDelta{Field: x.field, Text: text})
		}
	}
	x.buf = append(x.buf[:0], x.buf[n:]...)
}

// completeUTF8Prefix returns the length of the longest prefix of b that does
// not end in an incomplete UTF-8 sequence.
func completeUTF8Prefix(b []byte) int {
	for back := 1; back <= utf8.UTFMax && back <= len(b); back++ {
		c := b[len(b)-back]
		if utf8.RuneStart(c) {
			if !utf8.FullRune(b[len(b)-back:]) {
				return len(b) - back
			}
			return len(b)
		}
	}
	return len(b)
}
