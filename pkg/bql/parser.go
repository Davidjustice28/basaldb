package bql

import (
	"fmt"
	"strings"
)

// SelectorKind picks records inside a layer when used as a reference.
type SelectorKind int

const (
	SelAll  SelectorKind = iota // *
	SelID                       // [1001]    -> record ID
	SelSlot                     // [#1001]   -> X-axis slot
)

// Selector is the bracket part of a path reference.
type Selector struct {
	Kind SelectorKind
	N    uint64
}

func (s Selector) String() string {
	switch s.Kind {
	case SelAll:
		return "[*]"
	case SelSlot:
		return fmt.Sprintf("[#%d]", s.N)
	default:
		return fmt.Sprintf("[%d]", s.N)
	}
}

// Ref is a path reference like Users[1001], Teams[#3] or Accounts.
type Ref struct {
	Layer string
	Sel   Selector
}

func (r Ref) String() string { return r.Layer + r.Sel.String() }

// Statement is one parsed BQL statement.
type Statement interface {
	statementNode()
}

// GetStmt reads a path traversal of adjacent layers:
//
//	GET Accounts[0] -> Teams -> Users
type GetStmt struct {
	Path []Ref
}

// CreateStmt creates a record, optionally under an existing parent record:
//
//	CREATE Accounts ("Acme Corp", "2026-01-01")
//	CREATE Users PARENT Teams[101] ("Sam", "sam@dev.io")
type CreateStmt struct {
	Layer  string
	Parent *Ref // nil creates a root record
	Attrs  []string
}

// UpdateStmt replaces the scalar attributes of every selected record in
// one commit:
//
//	UPDATE Users[1001] ("Alex Rivera", "alex@dev.io")
type UpdateStmt struct {
	Ref   Ref
	Attrs []string
}

// DeleteStmt cascade-deletes the selected record and every descendant
// below it in one commit:
//
//	DELETE Accounts[1]
type DeleteStmt struct {
	Ref Ref
}

// ShowStmt lists database objects: LAYERS, BRANCHES, COMMITS or RECORDS.
type ShowStmt struct {
	Kind  string // "LAYERS" | "BRANCHES" | "COMMITS" | "RECORDS"
	Layer string // for RECORDS
}

// BranchStmt manages branches: LIST, USE, CREATE [FROM].
type BranchStmt struct {
	Action string // "LIST" | "USE" | "CREATE"
	Name   string
	From   string
}

func (*GetStmt) statementNode()    {}
func (*CreateStmt) statementNode() {}
func (*UpdateStmt) statementNode() {}
func (*DeleteStmt) statementNode() {}
func (*ShowStmt) statementNode()   {}
func (*BranchStmt) statementNode() {}

type parser struct {
	toks []token
	pos  int
}

// Parse parses exactly one BQL statement.
func Parse(q string) (Statement, error) {
	toks, err := lex(q)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	stmt, err := p.parseStmt()
	if err != nil {
		return nil, err
	}
	if p.peek().kind == tokSemi {
		p.next()
	}
	if p.peek().kind != tokEOF {
		return nil, fmt.Errorf("bql: unexpected %s after statement", p.peek())
	}
	return stmt, nil
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != tokEOF {
		p.pos++
	}
	return t
}

func (p *parser) expect(kind tokenKind, what string) (token, error) {
	t := p.peek()
	if t.kind != kind {
		return token{}, fmt.Errorf("bql: expected %s, got %s", what, t)
	}
	p.next()
	return t, nil
}

func (p *parser) parseStmt() (Statement, error) {
	t := p.peek()
	if t.kind != tokIdent {
		return nil, fmt.Errorf("bql: expected a statement, got %s", t)
	}
	switch {
	case keyword(t, "GET"):
		p.next()
		return p.parseGet()
	case keyword(t, "CREATE"):
		p.next()
		return p.parseCreate()
	case keyword(t, "UPDATE"):
		p.next()
		return p.parseUpdate()
	case keyword(t, "DELETE"):
		p.next()
		return p.parseDelete()
	case keyword(t, "SHOW"):
		p.next()
		return p.parseShow()
	case keyword(t, "BRANCH"):
		p.next()
		return p.parseBranch()
	default:
		return nil, fmt.Errorf("bql: unknown statement %q", t.text)
	}
}

// parseSelector parses [N] (id), [#N] (slot) or [*] (all).
func (p *parser) parseSelector() (Selector, error) {
	if _, err := p.expect(tokLBrack, "'['"); err != nil {
		return Selector{}, err
	}
	t := p.peek()
	var sel Selector
	switch t.kind {
	case tokStar:
		p.next()
		sel = Selector{Kind: SelAll}
	case tokHash:
		p.next()
		n, err := p.expect(tokNumber, "slot number after '#'")
		if err != nil {
			return Selector{}, err
		}
		sel = Selector{Kind: SelSlot, N: n.num}
	case tokNumber:
		p.next()
		sel = Selector{Kind: SelID, N: t.num}
	default:
		return Selector{}, fmt.Errorf("bql: expected selector ('*', '#n' or 'n'), got %s", t)
	}
	if _, err := p.expect(tokRBrack, "']'"); err != nil {
		return Selector{}, err
	}
	return sel, nil
}

// parseStep parses a path element: Layer with an optional selector
// (default: all records of the layer).
func (p *parser) parseStep() (Ref, error) {
	name, err := p.expect(tokIdent, "layer name")
	if err != nil {
		return Ref{}, err
	}
	sel := Selector{Kind: SelAll}
	if p.peek().kind == tokLBrack {
		sel, err = p.parseSelector()
		if err != nil {
			return Ref{}, err
		}
	}
	return Ref{Layer: name.text, Sel: sel}, nil
}

// parseAttrList parses a parenthesized list of positional attribute
// strings: ("Acme Corp", "2026-01-01").
func (p *parser) parseAttrList() ([]string, error) {
	if _, err := p.expect(tokLParen, "'(' to start the attribute list"); err != nil {
		return nil, err
	}
	var attrs []string
	if p.peek().kind == tokRParen {
		p.next()
		return attrs, nil
	}
	for {
		s, err := p.expect(tokString, "quoted attribute")
		if err != nil {
			return nil, err
		}
		attrs = append(attrs, s.text)
		if p.peek().kind == tokComma {
			p.next()
			continue
		}
		if _, err := p.expect(tokRParen, "')' to close the attribute list"); err != nil {
			return nil, err
		}
		return attrs, nil
	}
}

// parseGet handles a path traversal of adjacent layers:
//
//	GET Layer[sel] -> Step -> Step
func (p *parser) parseGet() (Statement, error) {
	first, err := p.parseStep()
	if err != nil {
		return nil, err
	}
	path := []Ref{first}
	for p.peek().kind == tokArrow {
		p.next()
		step, err := p.parseStep()
		if err != nil {
			return nil, err
		}
		path = append(path, step)
	}
	return &GetStmt{Path: path}, nil
}

// parseCreate handles:
//
//	CREATE Layer ("attrs", ...)
//	CREATE Layer PARENT Layer[sel] ("attrs", ...)
func (p *parser) parseCreate() (Statement, error) {
	name, err := p.expect(tokIdent, "layer name")
	if err != nil {
		return nil, err
	}
	stmt := &CreateStmt{Layer: name.text}
	if p.peek().kind == tokIdent && keyword(p.peek(), "PARENT") {
		p.next()
		parent, err := p.parseStep()
		if err != nil {
			return nil, err
		}
		stmt.Parent = &parent
	}
	attrs, err := p.parseAttrList()
	if err != nil {
		return nil, err
	}
	stmt.Attrs = attrs
	return stmt, nil
}

// parseUpdate handles update(Layer[sel], ["attrs"...]).
func (p *parser) parseUpdate() (Statement, error) {
	ref, err := p.parseStep()
	if err != nil {
		return nil, err
	}
	attrs, err := p.parseAttrList()
	if err != nil {
		return nil, err
	}
	return &UpdateStmt{Ref: ref, Attrs: attrs}, nil
}

// parseDelete handles delete(Layer[sel]).
func (p *parser) parseDelete() (Statement, error) {
	ref, err := p.parseStep()
	if err != nil {
		return nil, err
	}
	return &DeleteStmt{Ref: ref}, nil
}

func (p *parser) parseShow() (Statement, error) {
	what, err := p.expect(tokIdent, "LAYERS, BRANCHES, COMMITS or RECORDS")
	if err != nil {
		return nil, err
	}
	stmt := &ShowStmt{Kind: strings.ToUpper(what.text)}
	switch stmt.Kind {
	case "LAYERS", "BRANCHES", "COMMITS":
		return stmt, nil
	case "RECORDS":
		if p.peek().kind == tokIdent {
			stmt.Layer = p.next().text
		}
		return stmt, nil
	default:
		return nil, fmt.Errorf("bql: SHOW %s not supported (want LAYERS, BRANCHES, COMMITS or RECORDS)", what)
	}
}

func (p *parser) parseBranch() (Statement, error) {
	action, err := p.expect(tokIdent, "LIST, USE or CREATE")
	if err != nil {
		return nil, err
	}
	stmt := &BranchStmt{Action: strings.ToUpper(action.text)}
	switch stmt.Action {
	case "LIST":
		return stmt, nil
	case "USE":
		name, err := p.expect(tokIdent, "branch name")
		if err != nil {
			return nil, err
		}
		stmt.Name = name.text
		return stmt, nil
	case "CREATE":
		name, err := p.expect(tokIdent, "branch name")
		if err != nil {
			return nil, err
		}
		stmt.Name = name.text
		if p.peek().kind == tokIdent && keyword(p.peek(), "FROM") {
			p.next()
			from, err := p.expect(tokIdent, "source branch name")
			if err != nil {
				return nil, err
			}
			stmt.From = from.text
		}
		return stmt, nil
	default:
		return nil, fmt.Errorf("bql: BRANCH %s not supported (want LIST, USE or CREATE)", action)
	}
}
