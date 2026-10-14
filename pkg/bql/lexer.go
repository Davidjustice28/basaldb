// Package bql implements BasalQL (BQL), the path-based query language of
// BasalDB: zero-join hierarchical lookups like
//
//	GET Accounts[id=0] -> Teams -> Users[Name,Email][age=(gt=21)][limit=5]
//
// plus mutation and branching statements executed against engine snapshots.
package bql

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokIdent
	tokNumber
	tokString
	tokArrow  // ->
	tokLBrack // [
	tokRBrack // ]
	tokLParen // (
	tokRParen // )
	tokComma  // ,
	tokHash   // #
	tokStar   // *
	tokPipe   // |
	tokSemi   // ;
	tokEqual  // =
)

type token struct {
	kind tokenKind
	text string
	num  uint64
	pos  int
}

func (t token) String() string {
	switch t.kind {
	case tokEOF:
		return "end of input"
	case tokIdent:
		return t.text
	case tokNumber:
		if t.text != "" {
			return t.text
		}
		return strconv.FormatUint(t.num, 10)
	case tokString:
		return strconv.Quote(t.text)
	case tokArrow:
		return "->"
	case tokLBrack:
		return "["
	case tokRBrack:
		return "]"
	case tokLParen:
		return "("
	case tokRParen:
		return ")"
	case tokComma:
		return ","
	case tokHash:
		return "#"
	case tokStar:
		return "*"
	case tokPipe:
		return "|"
	case tokSemi:
		return ";"
	case tokEqual:
		return "="
	}
	return "?"
}

func lex(input string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(input) {
		c := input[i]
		switch {
		case unicode.IsSpace(rune(c)):
			i++
		case c == '-' && i+1 < len(input) && input[i+1] == '>':
			toks = append(toks, token{kind: tokArrow, text: "->", pos: i})
			i += 2
		case c == '[':
			toks = append(toks, token{kind: tokLBrack, text: "[", pos: i})
			i++
		case c == ']':
			toks = append(toks, token{kind: tokRBrack, text: "]", pos: i})
			i++
		case c == '(':
			toks = append(toks, token{kind: tokLParen, text: "(", pos: i})
			i++
		case c == ')':
			toks = append(toks, token{kind: tokRParen, text: ")", pos: i})
			i++
		case c == ',':
			toks = append(toks, token{kind: tokComma, text: ",", pos: i})
			i++
		case c == '#':
			toks = append(toks, token{kind: tokHash, text: "#", pos: i})
			i++
		case c == '*':
			toks = append(toks, token{kind: tokStar, text: "*", pos: i})
			i++
		case c == '|':
			toks = append(toks, token{kind: tokPipe, text: "|", pos: i})
			i++
		case c == ';':
			toks = append(toks, token{kind: tokSemi, text: ";", pos: i})
			i++
		case c == '=':
			toks = append(toks, token{kind: tokEqual, text: "=", pos: i})
			i++
		case c == '"' || c == '\'':
			s, next, err := lexString(input, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tokString, text: s, pos: i})
			i = next
		case c >= '0' && c <= '9',
			c == '-' && i+1 < len(input) && input[i+1] >= '0' && input[i+1] <= '9',
			c == '.' && i+1 < len(input) && input[i+1] >= '0' && input[i+1] <= '9':
			// num holds the magnitude of an integral literal; a leading
			// '-' stays in text so consumers that support signed values
			// (filter comparisons) can re-parse, while selectors can
			// reject it explicitly. Fractional and exponent forms
			// (0.9, -1.5, .5, 1e3, 2.5E-2) keep their full text and leave
			// num at 0: they are values, never slot or index selectors.
			start := i
			j := i
			if c == '-' {
				j++
			}
			for j < len(input) && input[j] >= '0' && input[j] <= '9' {
				j++
			}
			isFloat := false
			if j+1 < len(input) && input[j] == '.' && input[j+1] >= '0' && input[j+1] <= '9' {
				isFloat = true
				j++
				for j < len(input) && input[j] >= '0' && input[j] <= '9' {
					j++
				}
			}
			if j < len(input) && (input[j] == 'e' || input[j] == 'E') {
				k := j + 1
				if k < len(input) && (input[k] == '+' || input[k] == '-') {
					k++
				}
				if k < len(input) && input[k] >= '0' && input[k] <= '9' {
					isFloat = true
					j = k
					for j < len(input) && input[j] >= '0' && input[j] <= '9' {
						j++
					}
				}
			}
			text := input[start:j]
			var n uint64
			if !isFloat {
				mag := text
				if c == '-' {
					mag = text[1:]
				}
				var err error
				n, err = strconv.ParseUint(mag, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("bql: bad number %q at %d", text, i)
				}
			}
			toks = append(toks, token{kind: tokNumber, num: n, text: text, pos: start})
			i = j
		case isIdentStart(rune(c)):
			j := i
			for j < len(input) && isIdentPart(rune(input[j])) {
				j++
			}
			toks = append(toks, token{kind: tokIdent, text: input[i:j], pos: i})
			i = j
		default:
			return nil, fmt.Errorf("bql: unexpected character %q at %d; quote the value: \"...\"", string(c), i)
		}
	}
	toks = append(toks, token{kind: tokEOF, pos: len(input)})
	return toks, nil
}

func lexString(input string, start int) (string, int, error) {
	quote := input[start]
	var b strings.Builder
	i := start + 1
	for i < len(input) {
		c := input[i]
		if c == quote {
			return b.String(), i + 1, nil
		}
		if c == '\\' && i+1 < len(input) {
			i++
			switch input[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '\\', '"', '\'':
				b.WriteByte(input[i])
			default:
				return "", 0, fmt.Errorf("bql: bad escape \\%c at %d", input[i], i)
			}
			i++
			continue
		}
		b.WriteByte(c)
		i++
	}
	return "", 0, fmt.Errorf("bql: unterminated string starting at %d", start)
}

func isIdentStart(r rune) bool {
	return unicode.IsLetter(r) || r == '_'
}

// isIdentPart accepts the characters that may appear inside a bare word.
// '.', '/', ':' and '@' are included so values like browser.click,
// session:1, a@b.io or a/path lex unquoted; '-' is deliberately excluded
// so "->" (path step) and negative numbers still tokenize.
func isIdentPart(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' ||
		r == '.' || r == '/' || r == ':' || r == '@'
}

// keyword compares an identifier case-insensitively.
func keyword(tok token, kw string) bool {
	return tok.kind == tokIdent && strings.EqualFold(tok.text, kw)
}
