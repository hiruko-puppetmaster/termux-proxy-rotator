// Package opencodecfg reads and edits OpenCode JSONC configuration files while
// preserving comments, formatting, and unrelated settings.
//
// It implements the JSONC subset used by OpenCode: standard JSON plus line
// comments, block comments, and trailing commas.
package opencodecfg

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Kind identifies the type of a parsed JSON value.
type Kind int

const (
	ObjectKind Kind = iota
	ArrayKind
	StringKind
	NumberKind
	BoolKind
	NullKind
)

func (k Kind) String() string {
	switch k {
	case ObjectKind:
		return "object"
	case ArrayKind:
		return "array"
	case StringKind:
		return "string"
	case NumberKind:
		return "number"
	case BoolKind:
		return "boolean"
	case NullKind:
		return "null"
	default:
		return "unknown"
	}
}

// Member is one key/value pair inside an object. Start and End delimit the
// whole member: key, colon, and value. KeyStart and KeyEnd delimit the quoted
// key token.
type Member struct {
	Key      string
	KeyStart int
	KeyEnd   int
	Start    int
	End      int
	Value    *Node
}

// Node is a parsed JSON value with byte offsets into the source document.
type Node struct {
	Kind    Kind
	Start   int
	End     int
	Members []Member
	Items   []*Node
	Str     string
}

// Document is a parsed JSONC file.
type Document struct {
	Source []byte
	Root   *Node
}

// Member returns the member with the given key, or nil.
func (n *Node) Member(key string) *Member {
	if n == nil || n.Kind != ObjectKind {
		return nil
	}
	for i := range n.Members {
		if n.Members[i].Key == key {
			return &n.Members[i]
		}
	}
	return nil
}

// Parse parses a JSONC document. The returned ranges index into src.
func Parse(src []byte) (*Document, error) {
	p := &parser{src: src}
	p.skipBOM()
	p.skipSpaceAndComments()
	if p.eof() {
		return nil, p.errorf("empty document")
	}
	root, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	p.skipSpaceAndComments()
	if !p.eof() {
		return nil, p.errorf("unexpected data after the top-level value")
	}
	if root.Kind != ObjectKind {
		return nil, p.errorf("top-level value must be an object")
	}
	return &Document{Source: src, Root: root}, nil
}

type parser struct {
	src []byte
	i   int
}

func (p *parser) skipBOM() {
	if len(p.src) >= 3 && p.src[0] == 0xef && p.src[1] == 0xbb && p.src[2] == 0xbf {
		p.i = 3
	}
}

func (p *parser) eof() bool { return p.i >= len(p.src) }

func (p *parser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.src[p.i]
}

func (p *parser) skipSpaceAndComments() {
	for !p.eof() {
		switch p.src[p.i] {
		case ' ', '\t', '\r', '\n':
			p.i++
		case '/':
			if p.i+1 >= len(p.src) {
				return
			}
			switch p.src[p.i+1] {
			case '/':
				p.i += 2
				for !p.eof() && p.src[p.i] != '\n' {
					p.i++
				}
			case '*':
				p.i += 2
				for p.i+1 < len(p.src) && !(p.src[p.i] == '*' && p.src[p.i+1] == '/') {
					p.i++
				}
				if p.i+1 < len(p.src) {
					p.i += 2
				}
			default:
				return
			}
		default:
			return
		}
	}
}

func (p *parser) parseValue() (*Node, error) {
	p.skipSpaceAndComments()
	if p.eof() {
		return nil, p.errorf("expected a JSON value")
	}
	start := p.i
	switch p.peek() {
	case '{':
		return p.parseObject()
	case '[':
		return p.parseArray()
	case '"':
		s, err := p.parseString()
		if err != nil {
			return nil, err
		}
		return &Node{Kind: StringKind, Start: start, End: p.i, Str: s}, nil
	case 't':
		if err := p.expectLiteral("true"); err != nil {
			return nil, err
		}
		return &Node{Kind: BoolKind, Start: start, End: p.i, Str: "true"}, nil
	case 'f':
		if err := p.expectLiteral("false"); err != nil {
			return nil, err
		}
		return &Node{Kind: BoolKind, Start: start, End: p.i, Str: "false"}, nil
	case 'n':
		if err := p.expectLiteral("null"); err != nil {
			return nil, err
		}
		return &Node{Kind: NullKind, Start: start, End: p.i, Str: "null"}, nil
	default:
		if p.peek() == '-' || (p.peek() >= '0' && p.peek() <= '9') {
			return p.parseNumber()
		}
		return nil, p.errorf("unexpected character %q", p.peek())
	}
}

func (p *parser) parseObject() (*Node, error) {
	n := &Node{Kind: ObjectKind, Start: p.i}
	p.i++ // {
	seen := make(map[string]bool)
	p.skipSpaceAndComments()
	if p.peek() == '}' {
		p.i++
		n.End = p.i
		return n, nil
	}
	for {
		p.skipSpaceAndComments()
		if p.peek() != '"' {
			return nil, p.errorf("expected a quoted object key")
		}
		memberStart := p.i
		keyStart := p.i
		key, err := p.parseString()
		if err != nil {
			return nil, err
		}
		keyEnd := p.i
		if seen[key] {
			return nil, p.errorf("duplicate object key %q", key)
		}
		seen[key] = true

		p.skipSpaceAndComments()
		if p.peek() != ':' {
			return nil, p.errorf("expected ':' after object key %q", key)
		}
		p.i++
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		n.Members = append(n.Members, Member{
			Key:      key,
			KeyStart: keyStart,
			KeyEnd:   keyEnd,
			Start:    memberStart,
			End:      value.End,
			Value:    value,
		})

		p.skipSpaceAndComments()
		switch p.peek() {
		case ',':
			p.i++
			p.skipSpaceAndComments()
			if p.peek() == '}' { // trailing comma
				p.i++
				n.End = p.i
				return n, nil
			}
		case '}':
			p.i++
			n.End = p.i
			return n, nil
		default:
			if p.eof() {
				return nil, p.errorf("unterminated object")
			}
			return nil, p.errorf("expected ',' or '}' in object")
		}
	}
}

func (p *parser) parseArray() (*Node, error) {
	n := &Node{Kind: ArrayKind, Start: p.i}
	p.i++ // [
	p.skipSpaceAndComments()
	if p.peek() == ']' {
		p.i++
		n.End = p.i
		return n, nil
	}
	for {
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		n.Items = append(n.Items, value)

		p.skipSpaceAndComments()
		switch p.peek() {
		case ',':
			p.i++
			p.skipSpaceAndComments()
			if p.peek() == ']' { // trailing comma
				p.i++
				n.End = p.i
				return n, nil
			}
		case ']':
			p.i++
			n.End = p.i
			return n, nil
		default:
			if p.eof() {
				return nil, p.errorf("unterminated array")
			}
			return nil, p.errorf("expected ',' or ']' in array")
		}
	}
}

func (p *parser) parseString() (string, error) {
	if p.peek() != '"' {
		return "", p.errorf("expected a string")
	}
	p.i++
	var b strings.Builder
	for !p.eof() {
		c := p.src[p.i]
		switch c {
		case '"':
			p.i++
			return b.String(), nil
		case '\\':
			p.i++
			if p.eof() {
				return "", p.errorf("unterminated string escape")
			}
			esc := p.src[p.i]
			p.i++
			switch esc {
			case '"', '\\', '/':
				b.WriteByte(esc)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				r, err := p.parseUnicodeEscape()
				if err != nil {
					return "", err
				}
				b.WriteRune(r)
			default:
				return "", p.errorf("invalid string escape \\%c", esc)
			}
		default:
			if c < 0x20 {
				return "", p.errorf("unescaped control character in string")
			}
			if c < utf8.RuneSelf {
				b.WriteByte(c)
				p.i++
				continue
			}
			r, size := utf8.DecodeRune(p.src[p.i:])
			if r == utf8.RuneError && size == 1 {
				return "", p.errorf("invalid UTF-8 in string")
			}
			b.WriteRune(r)
			p.i += size
		}
	}
	return "", p.errorf("unterminated string")
}

func (p *parser) parseUnicodeEscape() (rune, error) {
	r1, err := p.parseHex4()
	if err != nil {
		return 0, err
	}
	if utf16.IsSurrogate(r1) {
		if p.i+1 >= len(p.src) || p.src[p.i] != '\\' || p.src[p.i+1] != 'u' {
			return utf8.RuneError, nil
		}
		p.i += 2
		r2, err := p.parseHex4()
		if err != nil {
			return 0, err
		}
		if combined := utf16.DecodeRune(r1, r2); combined != utf8.RuneError {
			return combined, nil
		}
		return utf8.RuneError, nil
	}
	return r1, nil
}

func (p *parser) parseHex4() (rune, error) {
	if p.i+4 > len(p.src) {
		return 0, p.errorf("truncated \\u escape")
	}
	v, err := strconv.ParseUint(string(p.src[p.i:p.i+4]), 16, 32)
	if err != nil {
		return 0, p.errorf("invalid \\u escape")
	}
	p.i += 4
	return rune(v), nil
}

func (p *parser) parseNumber() (*Node, error) {
	start := p.i
	if p.peek() == '-' {
		p.i++
	}
	digits := 0
	for p.peek() >= '0' && p.peek() <= '9' {
		p.i++
		digits++
	}
	if digits == 0 {
		return nil, p.errorf("invalid number")
	}
	if p.peek() == '.' {
		p.i++
		frac := 0
		for p.peek() >= '0' && p.peek() <= '9' {
			p.i++
			frac++
		}
		if frac == 0 {
			return nil, p.errorf("invalid number fraction")
		}
	}
	if p.peek() == 'e' || p.peek() == 'E' {
		p.i++
		if p.peek() == '+' || p.peek() == '-' {
			p.i++
		}
		exp := 0
		for p.peek() >= '0' && p.peek() <= '9' {
			p.i++
			exp++
		}
		if exp == 0 {
			return nil, p.errorf("invalid number exponent")
		}
	}
	return &Node{Kind: NumberKind, Start: start, End: p.i}, nil
}

func (p *parser) expectLiteral(lit string) error {
	if len(p.src)-p.i < len(lit) || string(p.src[p.i:p.i+len(lit)]) != lit {
		return p.errorf("expected %s", lit)
	}
	p.i += len(lit)
	return nil
}

func (p *parser) errorf(format string, args ...any) error {
	line, col := 1, 1
	for i := 0; i < p.i && i < len(p.src); i++ {
		if p.src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return fmt.Errorf("jsonc: line %d, column %d: %s", line, col, fmt.Sprintf(format, args...))
}
