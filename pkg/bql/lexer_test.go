package bql

import (
	"strings"
	"testing"
)

// Bare decimals, negative decimals, leading-dot decimals and exponents lex
// as number tokens with their full text preserved (issue #31).
func TestLexNumbers(t *testing.T) {
	cases := []struct {
		in   string
		text string
		num  uint64 // magnitude for integral literals, 0 for fractions
	}{
		{`42`, "42", 42},
		{`0`, "0", 0},
		{`-5`, "-5", 5},
		{`0.9`, "0.9", 0},
		{`-1.5`, "-1.5", 0},
		{`.5`, ".5", 0},
		{`1e3`, "1e3", 0},
		{`2.5E-2`, "2.5E-2", 0},
	}
	for _, tc := range cases {
		toks, err := lex(tc.in)
		if err != nil {
			t.Errorf("lex(%q): %v", tc.in, err)
			continue
		}
		if len(toks) != 2 || toks[0].kind != tokNumber || toks[1].kind != tokEOF {
			t.Errorf("lex(%q) = %+v, want one number token then EOF", tc.in, toks)
			continue
		}
		if toks[0].text != tc.text || toks[0].num != tc.num {
			t.Errorf("lex(%q) token = text %q num %d, want text %q num %d",
				tc.in, toks[0].text, toks[0].num, tc.text, tc.num)
		}
	}
}

// "1." is the number 1 followed by a stray dot, and "e3" is an exponent
// only after a digit: half-written forms must not silently lex as numbers.
func TestLexNumbersRejectHalfForms(t *testing.T) {
	if _, err := lex(`1.`); err == nil {
		t.Error(`lex("1.") succeeded, want error for the stray dot`)
	}
	if toks, err := lex(`e3`); err != nil || toks[0].kind != tokIdent {
		t.Errorf(`lex("e3") = %v, %v; want a bare identifier`, toks, err)
	}
}

// '.', '/', ':' and '@' may appear inside bare words so values like
// browser.click, session:1, a@b.io and a/path need no quotes (issue #31).
// '-' stays excluded so "->" and negative numbers still tokenize.
func TestLexBareWords(t *testing.T) {
	for _, in := range []string{`browser.click`, `session:1`, `a@b.io`, `some/path`, `v1.2.3`} {
		toks, err := lex(in)
		if err != nil {
			t.Errorf("lex(%q): %v", in, err)
			continue
		}
		if len(toks) != 2 || toks[0].kind != tokIdent || toks[0].text != in {
			t.Errorf("lex(%q) = %+v, want one ident token %q", in, toks, in)
		}
	}
	// Arrow and negative numbers are unaffected.
	toks, err := lex(`Users -> Teams`)
	if err != nil || len(toks) != 4 || toks[1].kind != tokArrow {
		t.Errorf(`lex("Users -> Teams") = %v, %v; want ident arrow ident EOF`, toks, err)
	}
	if toks, err = lex(`age=(gt=-1.5)`); err != nil || toks[5].kind != tokNumber || toks[5].text != "-1.5" {
		t.Errorf("lex(age=(gt=-1.5)) = %v, %v; want -1.5 as a number", toks, err)
	}
}

// End-to-end: unquoted decimals and dotted words survive Parse (issue #31
// repro: CREATE Facts [text="x",confidence=0.9] / GET Facts[confidence=(gte=0.8)]).
func TestParseUnquotedDecimalsAndDottedValues(t *testing.T) {
	stmt, err := Parse(`CREATE Facts [text=browser.click,confidence=0.9]`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	create := stmt.(*CreateStmt)
	if got := create.Rows[0]; len(got) != 2 ||
		got[0] != (Assignment{Field: "text", Value: "browser.click"}) ||
		got[1] != (Assignment{Field: "confidence", Value: "0.9"}) {
		t.Errorf("assignments = %+v", got)
	}

	stmt, err = Parse(`GET Facts[confidence=(gte=0.8),text=browser.click]`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	filters := stmt.(*GetStmt).Path[0].Filters
	if len(filters) != 2 ||
		filters[0] != (Filter{Field: "confidence", Op: OpGte, Value: "0.8"}) ||
		filters[1] != (Filter{Field: "text", Op: OpEq, Value: "browser.click"}) {
		t.Errorf("filters = %+v", filters)
	}
}

// A character that is still not allowed gets a quoting hint (issue #31).
func TestLexUnexpectedCharacterSuggestsQuoting(t *testing.T) {
	_, err := lex(`CREATE Users [name=a!b]`)
	if err == nil {
		t.Fatal("lex succeeded, want error")
	}
	if !strings.Contains(err.Error(), `quote the value: "..."`) {
		t.Errorf("error %q does not suggest quoting the value", err)
	}
}
