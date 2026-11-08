package manif

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Byte values for the C-style escape sequences upstream persist.lua may emit.
const (
	escBell      = 0x07 // \a
	escBackspace = 0x08 // \b
	escFormFeed  = 0x0C // \f
	escLineFeed  = 0x0A // \n
	escCarriage  = 0x0D // \r
	escTab       = 0x09 // \t
	escVTab      = 0x0B // \v
)

const (
	// decimalBase is the radix for parsing decimal escape sequences.
	decimalBase = 10

	// maxByteValue is the largest value a single byte can hold; decimal
	// escapes above it are out of range.
	maxByteValue = 255

	// hexDigitOffset maps a hex letter (a-f / A-F) to its numeric value
	// after subtracting the letter's base: 'a'-'a'+hexDigitOffset == 10.
	hexDigitOffset = 10
)

// Parse reads a serialized manifest (assignments mode) and returns a tree
// of native Go values:
//
//   - tables          → map[string]any (pure-string or sparse-numeric keys)
//     or *table when a table mixes string and numeric keys
//   - pure numeric    → []any when keys are exactly 1..N and dense
//     arrays
//   - Lua strings     → string
//   - Lua numbers     → int64 when integral, else float64
//   - true / false    → bool
//
// A bare nil literal in value position is a hard error, not a dropped entry —
// upstream persist.lua never emits one, so Parse never has to represent it.
// So is a table key that is neither a string nor a number (Lua allows
// booleans and tables), two keys that a stringified map would merge (1.5 and
// "1.5"), and tables nested more than 100 deep.
//
// Parse intentionally supports only the subset emitted by upstream
// persist.lua: table constructors, string literals (short and long form),
// integer and decimal numbers (with optional leading minus), boolean
// literals, identifier or quoted/bracketed keys, and nested tables.
//
// Anything outside that subset — function literals, control-flow keywords
// other than the boolean literals, expressions, the concat operator, etc.
// — is a hard error: the parser never silently drops input. Every such
// error wraps [ErrParse].
func Parse(b []byte) (any, error) {
	v, _, err := parse(b)

	return v, err
}

// parse is Parse, and also reports whether a table has a positional entry
// and an explicit key naming the same index, as in { "a", [1] = "b" }. Parse
// keeps the value given last, as LuaJIT does; PUC Lua and gopher-lua store
// the positional entries after the explicit ones, so the differential test,
// whose reference runs in gopher-lua, cannot judge such a table.
func parse(b []byte) (any, bool, error) {
	p := &parser{src: b}
	p.skipWS()
	// Top-level form: a sequence of `key = value` assignments. We
	// accumulate into a map[string]any. End-of-input when nothing but
	// whitespace remains.
	out := map[string]any{}

	for p.pos < len(p.src) {
		key, err := p.parseTopLevelKey()
		if err != nil {
			return nil, false, err
		}

		p.skipWS()

		if !p.consume('=') {
			return nil, false, p.errorf("expected '=' after key %q", key)
		}

		p.skipWS()

		v, err := p.parseValue()
		if err != nil {
			return nil, false, err
		}

		out[key] = v

		p.skipWS()
	}

	return out, p.overlap, nil
}

// maxNesting bounds how deeply tables may nest. Persisted manifests nest a
// handful of levels; the bound keeps a hostile one from exhausting the stack,
// and stays below what LuaJIT accepts (LJ_MAX_XLEVEL, 200 syntax levels), so
// Parse never accepts a nesting upstream rejects.
const maxNesting = 100

type parser struct {
	src   []byte
	pos   int
	depth int
	// overlap is set when a table has a positional entry and an explicit
	// key naming the same index; see parse.
	overlap bool
}

func (p *parser) errorf(format string, args ...any) error {
	line, col := p.lineCol()

	return fmt.Errorf("manif.Parse: line %d col %d: %s: %w", line, col, fmt.Sprintf(format, args...), ErrParse)
}

func (p *parser) lineCol() (int, int) {
	line, col := 1, 1

	for i := 0; i < p.pos && i < len(p.src); i++ {
		if p.src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}

	return line, col
}

func (p *parser) skipWS() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]

		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			p.pos++
		case c == '-' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '-':
			// Persist never emits comments; but the format does support
			// them when humans hand-edit a manifest. Single-line only —
			// long comments are not in the persist surface and would
			// signal something unusual.
			if p.pos+3 < len(p.src) && p.src[p.pos+2] == '[' && (p.src[p.pos+3] == '[' || p.src[p.pos+3] == '=') {
				// We don't accept long comments — that's outside the
				// persist-emitted subset.
				return
			}

			p.pos += 2
			// A CR ends the line as LF does (LuaJIT lex_iseol).
			for p.pos < len(p.src) && p.src[p.pos] != '\n' && p.src[p.pos] != '\r' {
				p.pos++
			}
		default:
			return
		}
	}
}

func (p *parser) peek() byte {
	if p.pos >= len(p.src) {
		return 0
	}

	return p.src[p.pos]
}

func (p *parser) consume(c byte) bool {
	if p.pos < len(p.src) && p.src[p.pos] == c {
		p.pos++

		return true
	}

	return false
}

func (p *parser) parseTopLevelKey() (string, error) {
	if !isIdentStart(p.peek()) {
		return "", p.errorf("expected identifier at start of top-level assignment, got %q", string(p.peek()))
	}

	start := p.pos

	// A keyword is no name in Lua: "end = 1" does not load.
	name := p.parseIdent()
	if luaKeywords[name] {
		p.pos = start

		return "", p.errorf("keyword %q cannot be assigned", name)
	}

	return name, nil
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func (p *parser) parseIdent() string {
	start := p.pos

	for p.pos < len(p.src) && isIdentPart(p.src[p.pos]) {
		p.pos++
	}

	return string(p.src[start:p.pos])
}

// parseValue dispatches on the next non-whitespace byte.
func (p *parser) parseValue() (any, error) {
	p.skipWS()

	if p.pos >= len(p.src) {
		return nil, p.errorf("unexpected end of input — expected value")
	}

	c := p.src[p.pos]

	switch {
	case c == '{':
		return p.parseTable()
	case c == '"' || c == '\'':
		return p.parseShortString()
	case c == '[':
		// long bracket string
		s, ok, err := p.tryParseLongBracket()
		if err != nil {
			return nil, err
		}

		if ok {
			return s, nil
		}

		return nil, p.errorf("unexpected '[' (expected long-bracket string)")
	case c == '-' || (c >= '0' && c <= '9'):
		return p.parseNumber()
	case isIdentStart(c):
		start := p.pos

		ident := p.parseIdent()
		switch ident {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "nil":
			// persist never emits a bare nil literal; treat it as a parse
			// error rather than returning an ambiguous (nil, nil).
			return nil, p.errorf("unexpected nil literal in value position")
		}
		// Anything else — function, control-flow, or an expression
		// involving a name — is outside the persist subset.
		p.pos = start

		return nil, p.errorf("unsupported identifier %q in value position (only true/false/nil are accepted)", ident)
	default:
		return nil, p.errorf("unexpected byte %q in value position", string(c))
	}
}

func (p *parser) parseShortString() (string, error) {
	quote := p.src[p.pos]
	p.pos++ // consume quote

	var b strings.Builder

	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == quote {
			p.pos++

			return b.String(), nil
		}

		// A raw CR is a line break to Lua as much as LF, and a short string
		// cannot span one (LuaJIT read_string: "unfinished string").
		if c == '\n' || c == '\r' {
			return "", p.errorf("unterminated short string (newline before closing quote)")
		}

		if c != '\\' {
			b.WriteByte(c)

			p.pos++

			continue
		}
		// escape
		p.pos++
		if p.pos >= len(p.src) {
			return "", p.errorf("dangling backslash at end of input")
		}

		esc := p.src[p.pos]
		switch esc {
		case 'a':
			b.WriteByte(escBell)

			p.pos++
		case 'b':
			b.WriteByte(escBackspace)

			p.pos++
		case 'f':
			b.WriteByte(escFormFeed)

			p.pos++
		case 'n':
			b.WriteByte(escLineFeed)

			p.pos++
		case 'r':
			b.WriteByte(escCarriage)

			p.pos++
		case 't':
			b.WriteByte(escTab)

			p.pos++
		case 'v':
			b.WriteByte(escVTab)

			p.pos++
		case '\\', '"', '\'':
			b.WriteByte(esc)

			p.pos++
		case '\n':
			b.WriteByte('\n')

			p.pos++
		case 'x':
			// \xHH — Lua 5.2+ syntax. Persist emits decimal, so this
			// is only relevant if a human hand-edited the file.
			if p.pos+2 >= len(p.src) {
				return "", p.errorf(`\x escape requires two hex digits`)
			}

			h1, ok1 := hexDigit(p.src[p.pos+1])

			h2, ok2 := hexDigit(p.src[p.pos+2])

			if !ok1 || !ok2 {
				return "", p.errorf(`\x escape requires two hex digits`)
			}

			// h1,h2 are single hex digits (0-15) so h1*16+h2 is 0-255.
			b.WriteByte(byte(h1*16 + h2))

			p.pos += 3
		default:
			if esc >= '0' && esc <= '9' {
				// Up to three decimal digits.
				n := 0

				digits := 0

				for digits < 3 && p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
					n = n*decimalBase + int(p.src[p.pos]-'0')
					p.pos++
					digits++
				}

				if n > maxByteValue {
					return "", p.errorf("decimal escape out of range: %d", n)
				}

				b.WriteByte(byte(n))

				continue
			}

			return "", p.errorf("unknown escape sequence \\%s", string(esc))
		}
	}

	return "", p.errorf("unterminated short string")
}

func hexDigit(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + hexDigitOffset, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + hexDigitOffset, true
	}

	return 0, false
}

// tryParseLongBracket consumes `[=*[ ... ]=*]` if the current position looks
// like one. Returns (value, true, nil) on success, (_, false, nil) if the
// position does not start a long bracket, and (_, _, err) on malformed
// content.
func (p *parser) tryParseLongBracket() (string, bool, error) {
	if p.peek() != '[' {
		return "", false, nil
	}

	save := p.pos
	p.pos++

	equals := 0
	for p.pos < len(p.src) && p.src[p.pos] == '=' {
		equals++
		p.pos++
	}

	if p.pos >= len(p.src) || p.src[p.pos] != '[' {
		// Not a long bracket — restore and let the caller deal.
		p.pos = save

		return "", false, nil
	}

	p.pos++ // consume opening '['

	// Per Lua spec, the lexer drops exactly one leading newline of ANY form
	// (\r\n, \n\r, lone \n, lone \r) right after the opening bracket. Persist
	// always writes such a newline.
	switch {
	case p.pos+1 < len(p.src) && p.src[p.pos] == '\r' && p.src[p.pos+1] == '\n':
		p.pos += 2
	case p.pos+1 < len(p.src) && p.src[p.pos] == '\n' && p.src[p.pos+1] == '\r':
		p.pos += 2
	case p.pos < len(p.src) && (p.src[p.pos] == '\n' || p.src[p.pos] == '\r'):
		p.pos++
	}

	closeBracket := "]" + strings.Repeat("=", equals) + "]"

	end := strings.Index(string(p.src[p.pos:]), closeBracket)
	if end < 0 {
		return "", false, p.errorf("unterminated long-bracket string")
	}

	val := normalizeLongBracketEOL(string(p.src[p.pos : p.pos+end]))
	p.pos += end + len(closeBracket)

	return val, true, nil
}

// normalizeLongBracketEOL collapses every interior end-of-line sequence
// (\r\n, \n\r, lone \r, lone \n) to a single \n, mirroring the Lua lexer's
// newline handling inside a long-bracket string (glr-aoc). The two-character
// sequences are handled before lone \r so a CRLF is not turned into two \n.
func normalizeLongBracketEOL(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}

	r := strings.NewReplacer("\r\n", "\n", "\n\r", "\n", "\r", "\n")

	return r.Replace(s)
}

// parseNumber accepts an optional leading '-' followed by decimal digits
// and an optional fractional part. No exponent support — persist emits
// numbers via tostring, which renders integers and small floats without an
// exponent. If we encounter scientific notation we still parse it via
// strconv to stay tolerant of hand-edits.
func (p *parser) parseNumber() (any, error) {
	start := p.pos

	if p.src[p.pos] == '-' {
		p.pos++
	}

	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		p.pos++
	}

	isFloat := false
	if p.pos < len(p.src) && p.src[p.pos] == '.' {
		isFloat = true

		p.pos++
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
		}
	}

	if p.pos < len(p.src) && (p.src[p.pos] == 'e' || p.src[p.pos] == 'E') {
		isFloat = true

		p.pos++
		if p.pos < len(p.src) && (p.src[p.pos] == '+' || p.src[p.pos] == '-') {
			p.pos++
		}

		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
		}
	}

	lit := string(p.src[start:p.pos])
	if lit == "" || lit == "-" {
		return nil, p.errorf("malformed number %q", lit)
	}

	// The Lua lexer reads every identifier byte and dot that follows into
	// the number, so "0X" or "1a" is one malformed number, not a number and
	// a name (LuaJIT lex_number).
	if c := p.peek(); isIdentPart(c) || c == '.' {
		return nil, p.errorf("malformed number near %q", lit+string(c))
	}

	if !isFloat {
		n, err := strconv.ParseInt(lit, 10, 64)
		if err == nil {
			return n, nil
		}
		// Falls through to float on overflow.
	}

	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return nil, p.errorf("malformed number %q: %v", lit, err)
	}

	return f, nil
}

// parseTable handles both array-style and record-style entries.
//
// Output shape:
//   - If every entry has an integer key 1..N (dense), returns []any.
//   - Mixed string and numeric keys → *table.
//   - Otherwise map[string]any (pure-string or sparse-numeric, keys stringified).
func (p *parser) parseTable() (any, error) {
	if !p.consume('{') {
		return nil, p.errorf("expected '{' starting table")
	}

	if p.depth >= maxNesting {
		return nil, p.errorf("tables nested deeper than %d", maxNesting)
	}

	p.depth++
	defer func() { p.depth-- }()

	var entries []entry

	autoIdx := int64(1)

	for {
		p.skipWS()

		if p.consume('}') {
			break
		}

		var ent entry
		// Determine if this is a keyed entry by lookahead.
		switch {
		case p.peek() == '[':
			// bracketed key: [expr] = value
			p.pos++
			p.skipWS()

			key, err := p.parseValue()
			if err != nil {
				return nil, err
			}

			p.skipWS()

			if !p.consume(']') {
				return nil, p.errorf("expected ']' after bracketed key")
			}

			p.skipWS()

			if !p.consume('=') {
				return nil, p.errorf("expected '=' after bracketed key")
			}

			p.skipWS()

			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}

			ent = entry{key: key, value: v}
		case isIdentStart(p.peek()) && p.identKeyLookahead():
			name := p.parseIdent()
			p.skipWS()

			if !p.consume('=') {
				return nil, p.errorf("expected '=' after ident key %q", name)
			}

			p.skipWS()

			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}

			ent = entry{key: name, value: v}
		default:
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}

			ent = entry{key: autoIdx, value: v, positional: true}
			autoIdx++
		}

		entries = append(entries, ent)

		p.skipWS()

		if p.consume(',') || p.consume(';') {
			continue
		}

		p.skipWS()

		if p.consume('}') {
			break
		}

		return nil, p.errorf("expected ',' or '}' in table")
	}

	return p.entriesToValue(entries)
}

// identKeyLookahead returns true if the position starts with an identifier
// followed (after whitespace) by '='. Used to distinguish `name = value`
// from a value that happens to be a boolean / nil literal.
func (p *parser) identKeyLookahead() bool {
	save := p.pos

	for p.pos < len(p.src) && isIdentPart(p.src[p.pos]) {
		p.pos++
	}

	// A keyword is no key: { end = 1 } does not load, and true/false/nil
	// start a value.
	if luaKeywords[string(p.src[save:p.pos])] {
		p.pos = save

		return false
	}
	// Skip whitespace but not newlines beyond — actually allow any WS.
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.pos++

			continue
		}

		break
	}

	isAssign := p.pos < len(p.src) && p.src[p.pos] == '='
	// Distinguish '=' from '==' just in case (persist never emits '==').
	if isAssign && p.pos+1 < len(p.src) && p.src[p.pos+1] == '=' {
		isAssign = false
	}

	p.pos = save

	return isAssign
}

// entriesToValue collapses the entry slice into one of:
//
//   - []any              — pure dense numeric (keys 1..N, no string keys)
//   - map[string]any     — pure string-keyed (no numeric keys)
//   - *table             — mixed numeric + string keys; preserves the
//     distinction so the writer can pack numeric
//     entries inline and emit string entries
//     multi-line, matching persist.lua.
//
// A key of any other type — a boolean or a table, which Lua allows but no
// manifest uses — is an error: the Go shapes have no place for it.
func (p *parser) entriesToValue(entries []entry) (any, error) {
	// Lua keys a table by value, whatever the order or the spelling of the
	// keys: [1.0] is the key 1, { [2] = "b", [1] = "a" } is an array, and a
	// key given twice keeps the value given last.
	var keys []any

	vals := map[any]any{}
	positional := map[any]bool{}

	for _, e := range entries {
		k := e.key

		switch kk := k.(type) {
		case int64, string:
		case float64:
			if kk == math.Trunc(kk) && math.Abs(kk) < int64Bound {
				k = int64(kk)
			}
		default:
			return nil, p.errorf("unsupported table key of type %s", luaTypeName(kk))
		}

		if was, seen := positional[k]; !seen {
			keys = append(keys, k)
		} else if was != e.positional {
			p.overlap = true
		}

		vals[k] = e.value
		positional[k] = e.positional
	}

	hasNum, hasStr, hasFloat := false, false, false
	dense := true

	for _, k := range keys {
		switch kk := k.(type) {
		case int64:
			hasNum = true
			// The keys are distinct, so len(keys) of them in 1..len(keys)
			// are exactly 1..N.
			dense = dense && kk >= 1 && kk <= int64(len(keys))
		case string:
			hasStr = true
		case float64:
			hasFloat = true
		}
	}

	if hasNum && !hasStr && !hasFloat && dense {
		arr := make([]any, len(keys))
		for _, k := range keys {
			i, _ := k.(int64)
			arr[i-1] = vals[k]
		}

		return arr, nil
	}

	if hasNum && hasStr && !hasFloat {
		t := newTable()

		for _, k := range keys {
			switch kk := k.(type) {
			case string:
				t.strKeys = append(t.strKeys, kk)
				t.str[kk] = vals[k]
			case int64:
				t.numKeys = append(t.numKeys, kk)
				t.num[kk] = vals[k]
			}
		}

		sort.Strings(t.strKeys)
		slices.Sort(t.numKeys)

		return t, nil
	}

	m := map[string]any{}
	orig := map[string]any{}

	for _, k := range keys {
		var key string

		switch kk := k.(type) {
		case string:
			key = kk
		case int64:
			key = strconv.FormatInt(kk, 10)
		case float64:
			key = strconv.FormatFloat(kk, 'g', -1, 64)
		}

		// Two keys Lua tells apart, such as 1.5 and "1.5", cannot share one
		// stringified key: keeping either would silently drop the other.
		if prev, ok := orig[key]; ok {
			return nil, p.errorf("table keys %s and %s collide as %q", keyString(prev), keyString(k), key)
		}

		orig[key] = k
		m[key] = vals[k]
	}

	return m, nil
}

// int64Bound is 2^63: a float64 below it in magnitude converts to int64
// exactly when it is integral.
const int64Bound = 1 << 63

// keyString renders a table key for an error message: a string quoted, a
// number as is.
func keyString(k any) string {
	if s, ok := k.(string); ok {
		return strconv.Quote(s)
	}

	return fmt.Sprint(k)
}

// luaTypeName names the Lua type of a parsed value, for error messages.
func luaTypeName(v any) string {
	switch v.(type) {
	case bool:
		return "boolean"
	case string:
		return "string"
	case int64, float64:
		return "number"
	default:
		return "table"
	}
}

type entry struct {
	key        any
	value      any
	positional bool
}

// ErrParse is wrapped by every error Parse returns; test for it with
// errors.Is(err, ErrParse).
var ErrParse = errors.New("parse error")
