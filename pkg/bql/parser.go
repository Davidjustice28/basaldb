package bql

import (
	"fmt"
	"strconv"
	"strings"
)

// RecordRef names a layer plus the filters that pick its records:
// Users[id=1001], Teams[slot=3], Accounts[name="Acme"], or a bare
// Accounts for every record. Mutations (CREATE PARENT, UPDATE, DELETE)
// target records through these filters.
type RecordRef struct {
	Layer   string
	Filters []Filter
}

func (r RecordRef) String() string { return r.Layer + "[" + filtersString(r.Filters) + "]" }

// filtersString renders a filter list for messages; no filters = "*".
func filtersString(filters []Filter) string {
	if len(filters) == 0 {
		return "*"
	}
	parts := make([]string, len(filters))
	for i, f := range filters {
		parts[i] = f.String()
	}
	return strings.Join(parts, ", ")
}

// Statement is one parsed BQL statement.
type Statement interface {
	statementNode()
} // GetStmt reads a path traversal of adjacent layers. Each step lists
// the attribute columns to return ([Name,Age] / [0,1] / [*]), may
// filter records ([id=1,Name=David,Age=(gt=21)]) and — on the final
// step — shape the result ([limit=N,sort=asc|desc]):
//
//	GET Accounts[*][Name=David,Age=(gt=21)][limit=1,sort=desc]
//	GET Accounts[id=0] -> Teams -> Users[Name,Email]
type GetStmt struct {
	Path []GetStep
}

// GetStep is one step of a GET path: an attribute projection plus
// optional filters and (on the final step) modifiers. Record selection
// is filter-based: every record at the step is a candidate, the filters
// prune.
type GetStep struct {
	Layer     string
	Project   []AttrRef
	Filters   []Filter
	Modifiers GetModifiers
}

// AttrRef references one attribute column by declared name or by
// positional index (Z axis).
type AttrRef struct {
	Name   string
	Index  int
	ByName bool
}

func (a AttrRef) String() string {
	if a.ByName {
		return a.Name
	}
	return strconv.Itoa(a.Index)
}

// FilterOp is a filter comparison operator.
type FilterOp int

const (
	OpEq  FilterOp = iota // =
	OpNe                  // (ne=)
	OpGt                  // (gt=)
	OpGte                 // (gte=)
	OpLt                  // (lt=)
	OpLte                 // (lte=)
)

func (o FilterOp) String() string {
	switch o {
	case OpNe:
		return "ne"
	case OpGt:
		return "gt"
	case OpGte:
		return "gte"
	case OpLt:
		return "lt"
	case OpLte:
		return "lte"
	}
	return "eq"
}

// Filter is one predicate of the filter bracket: Field=Value or
// Field=(op=Value). Field is an attribute name, an attribute index, or a
// builtin record field (id, slot, parent).
type Filter struct {
	Field string
	Op    FilterOp
	Value string
}

func (f Filter) String() string {
	if f.Op == OpEq {
		return f.Field + "=" + f.Value
	}
	return fmt.Sprintf("%s=(%s=%s)", f.Field, f.Op, f.Value)
}

// Assignment is one Field=value pair of a mutation's attribute
// bracket: an attribute to set, by declared name or positional index.
// id, slot and parent are record fields set by the engine
// (auto-assigned), never by assignment.
type Assignment struct {
	Field string // attribute name or index (as written)
	Value string
}

// MutationModifiers are the optional result controls of a mutation's
// final bracket: limit caps how many matched records are touched (0 =
// all), sort orders the matched records by id before the cap. Both are
// optional; with neither the bracket is omitted entirely.
//
//	UPDATE Users[region=emea][active=yes][limit=10,sort=asc]
//	DELETE Accounts[stale=yes][sort=desc][limit=1]
type MutationModifiers struct {
	Limit int
	Sort  string // "" | "asc" | "desc"
}

func (m MutationModifiers) isZero() bool { return m.Limit == 0 && m.Sort == "" }

// GetModifiers are the result controls of the final bracket: limit caps
// the row count (0 = unlimited), sort orders rows by record ID.
type GetModifiers struct {
	Limit int
	Sort  string // "" | "asc" | "desc"
}

// CreateStmt creates a record, optionally under an existing parent
// record, and sets its attributes from an assignment bracket. The
// parent must match exactly one record:
//
//	CREATE Accounts [name="Acme Corp",date="2026-01-01"]
//	CREATE Users PARENT Teams[id=101] [name="Sam",email="sam@dev.io"]
type CreateStmt struct {
	Layer  string
	Parent *RecordRef     // nil creates a root record
	Rows   [][]Assignment // one assignment bracket per new record
}

// UpdateStmt sets the listed attributes on every record matching the
// filters in one commit; attributes that are not listed keep their
// current values (the same idea as SQL's UPDATE ... SET). No filters
// means every record in the layer:
//
//	UPDATE Users[id=1001][name="Alex Rivera"]
//	UPDATE Users[age=(lt=18)][region=none]
type UpdateStmt struct {
	Target      RecordRef
	Assignments []Assignment
	Modifiers   MutationModifiers
}

// DeleteStmt cascade-deletes every record matching the filters (and all
// of each root's descendants) in one commit. No filters means every
// record in the layer:
//
//	DELETE Accounts[id=1]
//	DELETE Accounts[region=emea]
type DeleteStmt struct {
	Target    RecordRef
	Modifiers MutationModifiers
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

// parseRecordRef parses a layer name with an optional filter bracket
// (or [*]) selecting its records. Repeated filter brackets AND
// together, mirroring GET.
func (p *parser) parseRecordRef() (RecordRef, MutationModifiers, error) {
	name, err := p.expect(tokIdent, "layer name")
	if err != nil {
		return RecordRef{}, MutationModifiers{}, err
	}
	ref := RecordRef{Layer: name.text}
	var mods MutationModifiers
	seenMod := false
	for p.peek().kind == tokLBrack {
		items, err := p.parseBracketItems()
		if err != nil {
			return RecordRef{}, MutationModifiers{}, err
		}
		kind, err := classifyBracket(items)
		if err != nil {
			return RecordRef{}, MutationModifiers{}, err
		}
		switch kind {
		case brProject:
			// [*] on its own means "no filters"; consume it and continue.
			if len(items) == 1 && items[0].star {
				continue
			}
			return RecordRef{}, MutationModifiers{}, fmt.Errorf("bql: bare selectors are gone; write id=1 or slot=1 in a filter bracket")
		case brFilters:
			if seenMod {
				return RecordRef{}, MutationModifiers{}, fmt.Errorf("bql: filter brackets must precede the limit/sort bracket")
			}
			fs, err := filtersFromItems(items)
			if err != nil {
				return RecordRef{}, MutationModifiers{}, err
			}
			ref.Filters = append(ref.Filters, fs...)
		case brModifiers:
			// Several modifier brackets may follow ([sort=..][limit=..]); each
			// contributes its keys, and a repeated key is the real error.
			seenMod = true
			for _, it := range items {
				if err := applyMutationModifier(&mods, it); err != nil {
					return RecordRef{}, MutationModifiers{}, err
				}
			}
			// one bracket may hold both limit and sort; the guard is per-bracket
		}
	}
	return ref, mods, nil
}

func applyMutationModifier(mods *MutationModifiers, it getBracketItem) error {
	if it.op != OpEq {
		return fmt.Errorf("bql: %s does not take a comparison operator", it.field)
	}
	switch {
	case strings.EqualFold(it.field, "limit"):
		if mods.Limit != 0 {
			return fmt.Errorf("bql: duplicate limit modifier")
		}
		n, err := strconv.ParseUint(it.value, 10, 31)
		if err != nil {
			return fmt.Errorf("bql: limit must be a non-negative number, got %q", it.value)
		}
		mods.Limit = int(n)
	case strings.EqualFold(it.field, "sort"):
		if mods.Sort != "" {
			return fmt.Errorf("bql: duplicate sort modifier")
		}
		switch strings.ToLower(it.value) {
		case "asc":
			mods.Sort = "asc"
		case "desc":
			mods.Sort = "desc"
		default:
			return fmt.Errorf("bql: sort must be asc or desc, got %q", it.value)
		}
	default:
		return fmt.Errorf("bql: unknown mutation modifier %q (want limit or sort)", it.field)
	}
	return nil
}

// parseFiltersOnly parses a bracket that may only hold filters ([*] is
// accepted and means "no filters"). Unlike GET, mutations have no
// projection or modifier brackets.
func (p *parser) parseFiltersOnly() ([]Filter, error) {
	items, err := p.parseBracketItems()
	if err != nil {
		return nil, err
	}
	var filters []Filter
	for _, it := range items {
		switch {
		case it.star:
			if len(items) == 1 {
				return nil, nil
			}
			return nil, fmt.Errorf("bql: '*' cannot combine with filters")
		case it.slot:
			return nil, fmt.Errorf("bql: '#%d' selectors are not part of BQL; use slot=%d in the filter bracket", it.n, it.n)
		case !it.hasValue:
			if it.num {
				return nil, fmt.Errorf("bql: bare selectors are gone; write id=%d (or slot=%d) to target a record", it.n, it.n)
			}
			return nil, fmt.Errorf("bql: expected a filter like id=1 or Name=value, got %q", it.name)
		}
		filters = append(filters, Filter{Field: it.field, Op: it.op, Value: it.value})
	}
	return filters, nil
}

// parseAssignmentBracket parses a bracket of Field=Value pairs that set
// a record's attributes, by declared attribute name or positional
// index — the same fields filters accept, minus the operators:
//
//	[name="Sam", email="sam@dev.io"]    declared names
//	[0="Sam", 1="sam@dev.io"]           positional indexes
func (p *parser) parseAssignmentBracket() ([]Assignment, error) {
	items, err := p.parseBracketItems()
	if err != nil {
		return nil, err
	}
	return assignmentsFromItems(items)
}

// filtersFromItems converts bracket items into filters, enforcing that a
// filter bracket holds only Field=Value / Field=(op=Value) predicates.
func filtersFromItems(items []getBracketItem) ([]Filter, error) {
	var filters []Filter
	for _, it := range items {
		switch {
		case it.star:
			if len(items) == 1 {
				return nil, nil
			}
			return nil, fmt.Errorf("bql: '*' cannot combine with filters")
		case it.slot:
			return nil, fmt.Errorf("bql: '#%d' selectors are not part of BQL; use slot=%d in the filter bracket", it.n, it.n)
		case !it.hasValue:
			if it.num {
				return nil, fmt.Errorf("bql: bare selectors are gone; write id=%d (or slot=%d) to target a record", it.n, it.n)
			}
			return nil, fmt.Errorf("bql: expected a filter like id=1 or Name=value, got %q", it.name)
		}
		filters = append(filters, Filter{Field: it.field, Op: it.op, Value: it.value})
	}
	return filters, nil
}

// assignmentsFromItems converts bracket items into attribute assignments.
func assignmentsFromItems(items []getBracketItem) ([]Assignment, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("bql: empty attribute bracket; omit it entirely to write no attributes")
	}
	seen := map[string]bool{}
	assigns := make([]Assignment, 0, len(items))
	for _, it := range items {
		switch {
		case it.star:
			return nil, fmt.Errorf("bql: '*' is not an assignment; write attributes like name=\"Sam\"")
		case it.slot:
			return nil, fmt.Errorf("bql: '#%d' selectors are not part of BQL; assign by index like [%d=\"value\"]", it.n, it.n)
		case !it.hasValue:
			if it.num {
				return nil, fmt.Errorf("bql: attribute index %d needs a value, e.g. %d=\"Sam\"", it.n, it.n)
			}
			return nil, fmt.Errorf("bql: expected an assignment like name=\"Sam\", got %q", it.name)
		case it.op != OpEq:
			return nil, fmt.Errorf("bql: assignments do not take a comparison operator; write %s=%s", it.field, strconv.Quote(it.value))
		}
		switch strings.ToLower(it.field) {
		case "id", "slot", "parent":
			return nil, fmt.Errorf("bql: %s is a record field, not an attribute; it cannot be assigned", strings.ToLower(it.field))
		}
		key := strings.ToLower(it.field)
		if seen[key] {
			return nil, fmt.Errorf("bql: duplicate assignment for attribute %q", it.field)
		}
		seen[key] = true
		assigns = append(assigns, Assignment{Field: it.field, Value: it.value})
	}
	return assigns, nil
}

// parseGet handles a path traversal of adjacent layers, each step with
// optional projection/filter/modifier brackets:
//
//	GET Layer[sel] -> Step
//	GET Layer[Name,Age][Name=David][limit=1,sort=desc]
func (p *parser) parseGet() (Statement, error) {
	first, err := p.parseGetStep()
	if err != nil {
		return nil, err
	}
	path := []GetStep{first}
	for p.peek().kind == tokArrow {
		p.next()
		step, err := p.parseGetStep()
		if err != nil {
			return nil, err
		}
		path = append(path, step)
	}
	return &GetStmt{Path: path}, nil
}

// bracketKind classifies a bracket group by its items and, for mutations,
// by its position in the statement.
type bracketKind int

const (
	brProject bracketKind = iota
	brFilters
	brModifiers
)

func (k bracketKind) String() string {
	switch k {
	case brFilters:
		return "filter"
	case brModifiers:
		return "modifier"
	}
	return "projection"
}

// getBracketItem is one comma-separated item of a GET bracket group.
type getBracketItem struct {
	star bool   // *
	slot bool   // #N
	num  bool   // bare non-negative number
	n    uint64 // slot or number value
	name string // bare identifier (attribute name / builtin field)

	field    string // filter field when hasValue
	op       FilterOp
	value    string
	hasValue bool
} // parseGetStep parses a layer name plus its bracket groups. Group order
// is free; each kind may appear at most once (filter brackets repeat
// and AND together).
func (p *parser) parseGetStep() (GetStep, error) {
	name, err := p.expect(tokIdent, "layer name")
	if err != nil {
		return GetStep{}, err
	}
	step := GetStep{Layer: name.text}
	seen := map[bracketKind]bool{}
	for p.peek().kind == tokLBrack {
		items, err := p.parseBracketItems()
		if err != nil {
			return GetStep{}, err
		}
		kind, err := classifyBracket(items)
		if err != nil {
			return GetStep{}, err
		}
		if seen[kind] && kind != brFilters {
			return GetStep{}, fmt.Errorf("bql: duplicate %s bracket on %s", kind, step.Layer)
		}
		seen[kind] = true
		switch kind {
		case brProject:
			proj, err := projectionFromItems(items)
			if err != nil {
				return GetStep{}, err
			}
			step.Project = proj
		case brFilters: // repeated filter brackets simply AND together
			for _, it := range items {
				step.Filters = append(step.Filters, Filter{Field: it.field, Op: it.op, Value: it.value})
			}
		case brModifiers:
			for _, it := range items {
				if err := applyModifier(&step.Modifiers, it); err != nil {
					return GetStep{}, err
				}
			}
		}
	}
	return step, nil
}

func (p *parser) parseBracketItems() ([]getBracketItem, error) {
	if _, err := p.expect(tokLBrack, "'['"); err != nil {
		return nil, err
	}
	var items []getBracketItem
	for {
		it, err := p.parseBracketItem()
		if err != nil {
			return nil, err
		}
		items = append(items, it)
		if p.peek().kind == tokComma {
			p.next()
			continue
		}
		if _, err := p.expect(tokRBrack, "']'"); err != nil {
			return nil, err
		}
		return items, nil
	}
}

// parseBracketItem parses one selector, attribute reference or filter
// (Field=Value / Field=(op=Value)).
func (p *parser) parseBracketItem() (getBracketItem, error) {
	t := p.next()
	var it getBracketItem
	var field string
	switch t.kind {
	case tokStar:
		it.star = true
	case tokHash:
		it.slot = true
		n, err := p.expect(tokNumber, "slot number after '#'")
		if err != nil {
			return it, err
		}
		it.n = n.num
	case tokNumber:
		if strings.HasPrefix(t.text, "-") {
			return it, fmt.Errorf("bql: selector/attribute index must be >= 0, got %s", t)
		}
		it.num = true
		it.n = t.num
		field = t.text
	case tokIdent:
		it.name = t.text
		field = t.text
	default:
		return it, fmt.Errorf("bql: expected selector, attribute or filter, got %s", t)
	}

	if p.peek().kind == tokEqual {
		p.next()
		if p.peek().kind == tokLParen {
			// Field=(op=Value)
			p.next()
			opTok, err := p.expect(tokIdent, "comparison operator (eq, ne, gt, gte, lt, lte)")
			if err != nil {
				return it, err
			}
			op, err := parseFilterOp(opTok)
			if err != nil {
				return it, err
			}
			if _, err := p.expect(tokEqual, "'=' inside the filter"); err != nil {
				return it, err
			}
			value, err := p.parseFilterValue()
			if err != nil {
				return it, err
			}
			if _, err := p.expect(tokRParen, "')' closing the filter"); err != nil {
				return it, err
			}
			if field == "" {
				return it, fmt.Errorf("bql: '*' and '#N' cannot be used as filter fields")
			}
			it.field, it.op, it.value, it.hasValue = field, op, value, true
			return it, nil
		}
		// Field=Value (equals)
		value, err := p.parseFilterValue()
		if err != nil {
			return it, err
		}
		if field == "" {
			return it, fmt.Errorf("bql: '*' and '#N' cannot be used as filter fields")
		}
		it.field, it.op, it.value, it.hasValue = field, OpEq, value, true
	}
	return it, nil
}

func (p *parser) parseFilterValue() (string, error) {
	v := p.next()
	switch v.kind {
	case tokNumber, tokString, tokIdent:
		return v.text, nil
	}
	return "", fmt.Errorf("bql: expected a filter value (word, number or \"string\"), got %s", v)
}

func parseFilterOp(tok token) (FilterOp, error) {
	switch {
	case keyword(tok, "eq"):
		return OpEq, nil
	case keyword(tok, "ne"):
		return OpNe, nil
	case keyword(tok, "gt"):
		return OpGt, nil
	case keyword(tok, "gte"):
		return OpGte, nil
	case keyword(tok, "lt"):
		return OpLt, nil
	case keyword(tok, "lte"):
		return OpLte, nil
	}
	return OpEq, fmt.Errorf("bql: unknown comparison operator %q (want eq, ne, gt, gte, lt or lte)", tok.text)
}

// classifyBracket decides whether a bracket holds filters, modifiers or
// a selector/projection.
func classifyBracket(items []getBracketItem) (bracketKind, error) {
	hasValue := false
	for _, it := range items {
		if it.hasValue {
			hasValue = true
			break
		}
	}
	if !hasValue {
		return brProject, nil
	}
	allMods := true
	for _, it := range items {
		if !it.hasValue {
			return 0, fmt.Errorf("bql: cannot mix filters and attributes in one bracket")
		}
		if !isModifierKey(it.field) {
			allMods = false
		}
	}
	if allMods {
		return brModifiers, nil
	}
	return brFilters, nil
}

func isModifierKey(field string) bool {
	return strings.EqualFold(field, "limit") || strings.EqualFold(field, "sort")
}

// projectionFromItems turns a projection bracket into attribute
// references. A single [*] means every attribute (no projection);
// anything else lists columns by declared name or positional index.
func projectionFromItems(items []getBracketItem) ([]AttrRef, error) {
	if len(items) == 1 && items[0].star {
		return nil, nil
	}
	proj := make([]AttrRef, 0, len(items))
	for _, it := range items {
		if it.star {
			return nil, fmt.Errorf("bql: '*' cannot combine with attribute names in a projection")
		}
		if it.slot {
			return nil, fmt.Errorf("bql: '#%d' selectors are not part of GET; use slot=%d in the filter bracket", it.n, it.n)
		}
		if it.num {
			proj = append(proj, AttrRef{Index: int(it.n)})
		} else {
			proj = append(proj, AttrRef{Name: it.name, ByName: true})
		}
	}
	return proj, nil
}

func applyModifier(mods *GetModifiers, it getBracketItem) error {
	if it.op != OpEq {
		return fmt.Errorf("bql: %s does not take a comparison operator", it.field)
	}
	switch {
	case strings.EqualFold(it.field, "limit"):
		n, err := strconv.ParseUint(it.value, 10, 31)
		if err != nil {
			return fmt.Errorf("bql: limit must be a non-negative number, got %q", it.value)
		}
		mods.Limit = int(n)
	case strings.EqualFold(it.field, "sort"):
		switch strings.ToLower(it.value) {
		case "asc":
			mods.Sort = "asc"
		case "desc":
			mods.Sort = "desc"
		default:
			return fmt.Errorf("bql: sort must be asc or desc, got %q", it.value)
		}
	}
	return nil
}

// parseCreate handles:
//
//	CREATE Layer [field=value, ...]
//	CREATE Layer PARENT Layer[filters] [field=value, ...]
//
// parseCreateParentRef parses the PARENT target: a layer name with exactly
// one filter bracket. Unlike a standalone record ref it must not consume
// the trailing assignment bracket that follows, so it reads at most one
// bracket and leaves the rest for parseAssignmentBracket.
func (p *parser) parseCreateParentRef() (RecordRef, error) {
	name, err := p.expect(tokIdent, "layer name")
	if err != nil {
		return RecordRef{}, err
	}
	ref := RecordRef{Layer: name.text}
	if p.peek().kind == tokLBrack {
		items, err := p.parseBracketItems()
		if err != nil {
			return RecordRef{}, err
		}
		fs, err := filtersFromItems(items)
		if err != nil {
			return RecordRef{}, err
		}
		ref.Filters = fs
	}
	return ref, nil
}

func (p *parser) parseCreate() (Statement, error) {
	name, err := p.expect(tokIdent, "layer name")
	if err != nil {
		return nil, err
	}
	stmt := &CreateStmt{Layer: name.text}
	if p.peek().kind == tokIdent && keyword(p.peek(), "PARENT") {
		p.next()
		parent, err := p.parseCreateParentRef()
		if err != nil {
			return nil, err
		}
		stmt.Parent = &parent
	}
	if p.peek().kind == tokLParen {
		return nil, fmt.Errorf("bql: attribute lists in parentheses are gone; use a bracket of assignments, e.g. CREATE %s [name=\"value\"]", stmt.Layer)
	}
	for p.peek().kind == tokLBrack {
		assigns, err := p.parseAssignmentBracket()
		if err != nil {
			return nil, err
		}
		stmt.Rows = append(stmt.Rows, assigns)
	}
	if len(stmt.Rows) == 0 {
		return nil, fmt.Errorf("bql: CREATE needs at least one assignment bracket, e.g. CREATE %s [name=\"value\"]", stmt.Layer)
	}
	return stmt, nil
}

// parseUpdate handles UPDATE Layer[filters]...[modifiers][assignments]:
// filter brackets pick the records (AND together), an optional [limit=N,sort=asc|desc]
// bracket caps/orders which matched records are touched, and the final bracket
// sets attributes. The assignment bracket is always last.
func (p *parser) parseUpdate() (Statement, error) {
	name, err := p.expect(tokIdent, "layer name")
	if err != nil {
		return nil, err
	}
	ref := RecordRef{Layer: name.text}
	var mods MutationModifiers
	var assigns []Assignment
	sawFilter := false
	for p.peek().kind == tokLBrack {
		// Classify each bracket by position: modifiers ([limit=,sort=]) may
		// appear anywhere; a plain Field=Value bracket is a filter unless it
		// is the last bracket, in which case it is the attribute bracket.
		items, err := p.parseBracketItems()
		if err != nil {
			return nil, err
		}
		kind, err := classifyBracket(items)
		if err != nil {
			return nil, err
		}
		if kind == brProject {
			// [*] on its own means "no filters"; consume it and continue.
			if len(items) == 1 && items[0].star {
				continue
			}
			return nil, fmt.Errorf("bql: bare selectors are gone; write id=1 or slot=1 in a filter bracket")
		}
		if kind == brModifiers {
			// Several modifier brackets may follow ([sort=..][limit=..]); each
			// contributes its keys, and a repeated key is the real error.
			for _, it := range items {
				if err := applyMutationModifier(&mods, it); err != nil {
					return nil, err
				}
			}
			continue
		}
		// Plain Field=Value bracket. If another bracket follows, this is a
		// filter bracket; at end of statement it is the trailing attribute
		// bracket.
		if p.peek().kind == tokLBrack {
			fs, err := filtersFromItems(items)
			if err != nil {
				return nil, err
			}
			ref.Filters = append(ref.Filters, fs...)
			sawFilter = true
			continue
		}
		// Nothing but the assignment bracket remains: this is it.
		if !sawFilter {
			return nil, fmt.Errorf("bql: UPDATE needs at least one filter bracket before the attribute bracket, e.g. UPDATE %s[id=1][name=\"value\"]", ref.Layer)
		}
		for _, it := range items {
			if it.op != OpEq {
				return nil, fmt.Errorf("bql: assignments do not take a comparison operator; write %s=%s", it.field, strconv.Quote(it.value))
			}
		}
		a, err := assignmentsFromItems(items)
		if err != nil {
			return nil, err
		}
		assigns = a
		break
	}
	if assigns == nil {
		return nil, fmt.Errorf("bql: UPDATE needs an attribute bracket, e.g. UPDATE %s[id=1][name=\"value\"]", ref.Layer)
	}
	return &UpdateStmt{Target: ref, Assignments: assigns, Modifiers: mods}, nil
}

// parseDelete handles DELETE Layer[filters]...[limit=N,sort=asc|desc]:
// filter brackets pick the roots (AND together) and an optional trailing
// modifier bracket caps/orders which matched roots are cascade-deleted.
func (p *parser) parseDelete() (Statement, error) {
	ref, mods, err := p.parseRecordRef()
	if err != nil {
		return nil, err
	}
	return &DeleteStmt{Target: ref, Modifiers: mods}, nil
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
