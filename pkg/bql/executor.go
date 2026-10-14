package bql

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"basaldb/pkg/engine"
)

// ExecContext binds a query to a database, a branch and the optional
// audit metadata stamped onto commit blocks.
type ExecContext struct {
	DB     *engine.DB
	Branch string
	Meta   engine.TxMeta // Author, Summary, Reason (required for BRANCH CREATE)
}

// Row is one result row. Record rows carry the structured record fields;
// catalog rows (SHOW / BRANCH) use Cells.
type Row struct {
	Layer    string   `json:"layer,omitempty"`
	ID       uint64   `json:"id,omitempty"`
	Slot     uint64   `json:"slot,omitempty"`
	Parent   uint64   `json:"parent,omitempty"`
	Children []uint64 `json:"children,omitempty"`
	Attrs    []string `json:"attrs,omitempty"`
	Cells    []string `json:"cells,omitempty"`
}

// CommitInfo is the commit block echoed back after a write statement.
type CommitInfo struct {
	ID        uint64 `json:"id"`
	Branch    string `json:"branch"`
	Parent    uint64 `json:"parent"`
	Author    string `json:"author"`
	Summary   string `json:"summary"`
	Reason    string `json:"reason"`
	Timestamp int64  `json:"timestamp"`
}

// Result is the outcome of one statement.
type Result struct {
	Columns   []string    `json:"columns"`
	Rows      []Row       `json:"rows,omitempty"`
	Message   string      `json:"message,omitempty"`
	Commit    *CommitInfo `json:"commit,omitempty"`
	UseBranch string      `json:"use_branch,omitempty"`
}

var recordColumns = []string{"layer", "id", "slot", "parent", "children", "attrs"}

// Exec parses and executes one BQL statement.
func Exec(ctx *ExecContext, q string) (*Result, error) {
	stmt, err := Parse(q)
	if err != nil {
		return nil, err
	}
	return ExecStmt(ctx, stmt)
}

// ExecStmt executes an already-parsed statement.
func ExecStmt(ctx *ExecContext, stmt Statement) (*Result, error) {
	if ctx == nil || ctx.DB == nil {
		return nil, fmt.Errorf("bql: no database in execution context")
	}
	branch := ctx.Branch
	if branch == "" {
		branch = engine.DefaultBranch
	}
	switch s := stmt.(type) {
	case *GetStmt:
		return execGet(ctx, branch, s)
	case *CreateStmt:
		return execCreate(ctx, branch, s)
	case *UpdateStmt:
		return execUpdate(ctx, branch, s)
	case *DeleteStmt:
		return execDelete(ctx, branch, s)
	case *ShowStmt:
		return execShow(ctx, branch, s)
	case *BranchStmt:
		return execBranch(ctx, branch, s)
	default:
		return nil, fmt.Errorf("bql: unsupported statement %T", stmt)
	}
}

func recordRow(layerName string, r *engine.Record) Row {
	return Row{
		Layer:    layerName,
		ID:       r.ID,
		Slot:     r.Slot,
		Parent:   r.Parent,
		Children: r.Children,
		Attrs:    r.Attrs,
	}
}

func layerHeight(layers []engine.Layer, name string) (int, bool) {
	for _, l := range layers {
		if l.Name == name {
			return l.Height, true
		}
	}
	return 0, false
}

// matchRef resolves a RecordRef inside a write transaction to the
// records matching its filters.
func matchRef(tx *engine.WriteTx, ref RecordRef) ([]*engine.Record, error) {
	h, ok := layerHeight(tx.Layers(), ref.Layer)
	if !ok {
		return nil, fmt.Errorf("bql: unknown layer %q", ref.Layer)
	}
	all, err := tx.Records(ref.Layer)
	if err != nil {
		return nil, err
	}
	return filterRecords(tx.Layers(), h, all, ref.Filters)
}

// applyMutationMods orders the matched records by id (when sort is set)
// and caps how many are touched (when limit is set). With no modifiers
// it returns recs unchanged.
func applyMutationMods(recs []*engine.Record, mods MutationModifiers) []*engine.Record {
	if mods.Sort != "" {
		asc := mods.Sort == "asc"
		sort.SliceStable(recs, func(i, j int) bool {
			if asc {
				return recs[i].ID < recs[j].ID
			}
			return recs[i].ID > recs[j].ID
		})
	}
	if n := mods.Limit; n > 0 && len(recs) > n {
		recs = recs[:n]
	}
	return recs
}

// execGet runs a path traversal (zero join) over an immutable MVCC
// snapshot. Steps filter as they walk; the final step projects
// attribute columns and applies limit/sort to the result.
func execGet(ctx *ExecContext, branch string, s *GetStmt) (*Result, error) {
	if len(s.Path) == 0 {
		return nil, fmt.Errorf("bql: GET requires a starting layer")
	}
	for i, step := range s.Path[:len(s.Path)-1] {
		if step.Modifiers != (GetModifiers{}) {
			return nil, fmt.Errorf("bql: limit/sort only apply to the final step (used on step %d, %q)", i+1, step.Layer)
		}
	}
	snap, err := ctx.DB.Snapshot(branch)
	if err != nil {
		return nil, err
	}
	heights := map[string]int{}
	for _, l := range snap.Layers() {
		heights[l.Name] = l.Height
	}
	first := s.Path[0]
	h, ok := heights[first.Layer]
	if !ok {
		return nil, fmt.Errorf("bql: unknown layer %q", first.Layer)
	}
	cur, err := filterRecords(snap.Layers(), h, snap.Records(h), first.Filters)
	if err != nil {
		return nil, err
	}
	curHeight := h

	for _, step := range s.Path[1:] {
		stepH, ok := heights[step.Layer]
		if !ok {
			return nil, fmt.Errorf("bql: unknown layer %q", step.Layer)
		}
		if stepH != curHeight+1 {
			return nil, fmt.Errorf("bql: path step %q must be the child layer of height %d (got height %d)",
				step.Layer, curHeight, stepH)
		}
		var next []*engine.Record
		for _, r := range cur {
			next = append(next, snap.Children(curHeight, r.ID)...)
		}
		next, err = filterRecords(snap.Layers(), stepH, next, step.Filters)
		if err != nil {
			return nil, err
		}
		cur = next
		curHeight = stepH
	}

	final := s.Path[len(s.Path)-1]
	finalLayer := snap.Layers()[curHeight]
	rows := make([]Row, 0, len(cur))
	for _, r := range cur {
		row := recordRow(finalLayer.Name, r)
		if len(final.Project) > 0 {
			attrs, err := projectAttrs(finalLayer, r, final.Project)
			if err != nil {
				return nil, err
			}
			row.Attrs = attrs
		}
		rows = append(rows, row)
	}
	if final.Modifiers.Sort != "" {
		asc := final.Modifiers.Sort == "asc"
		sort.SliceStable(rows, func(i, j int) bool {
			if asc {
				return rows[i].ID < rows[j].ID
			}
			return rows[i].ID > rows[j].ID
		})
	}
	if n := final.Modifiers.Limit; n > 0 && len(rows) > n {
		rows = rows[:n]
	}
	return &Result{
		Columns: recordColumns,
		Rows:    rows,
		Message: fmt.Sprintf("%d record(s) on branch %q", len(rows), branch),
	}, nil
}

// filterRecords drops records that fail any filter of the step.
func filterRecords(layers []engine.Layer, height int, recs []*engine.Record, filters []Filter) ([]*engine.Record, error) {
	if len(filters) == 0 {
		return recs, nil
	}
	layer := layers[height]
	var out []*engine.Record
	for _, r := range recs {
		keep := true
		for _, f := range filters {
			ok, err := evalFilter(layer, r, f)
			if err != nil {
				return nil, err
			}
			if !ok {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, r)
		}
	}
	return out, nil
}

// evalFilter evaluates one filter against a record. Builtin fields (id,
// slot, parent) compare numerically; attributes compare as stored
// strings, with ordering operators using numeric comparison when both
// sides parse as numbers.
func evalFilter(layer engine.Layer, r *engine.Record, f Filter) (bool, error) {
	switch strings.ToLower(f.Field) {
	case "id":
		return compareNumFilter(float64(r.ID), f)
	case "slot":
		return compareNumFilter(float64(r.Slot), f)
	case "parent":
		return compareNumFilter(float64(r.Parent), f)
	}
	idx := -1
	if n, err := strconv.Atoi(f.Field); err == nil {
		idx = n
	} else {
		i, ok := layer.AttrIndex(f.Field)
		if !ok {
			return false, attrUnknownError(layer, f.Field)
		}
		idx = i
	}
	if idx < 0 || idx >= len(r.Attrs) {
		return false, nil // an out-of-range attribute never matches
	}
	return compareAttrFilter(r.Attrs[idx], f)
}

func attrUnknownError(layer engine.Layer, name string) error {
	if len(layer.Attrs) == 0 {
		return fmt.Errorf("bql: layer %q declares no attribute names; init it with -layers \"%d:%s(name,...)\" or reference attributes by index",
			layer.Name, layer.Height, layer.Name)
	}
	return fmt.Errorf("bql: layer %q has no attribute %q (declared: %s)", layer.Name, name, strings.Join(layer.Attrs, ", "))
}

func compareNumFilter(actual float64, f Filter) (bool, error) {
	want, err := strconv.ParseFloat(f.Value, 64)
	if err != nil {
		return false, fmt.Errorf("bql: %s filter needs a number, got %q", f.Field, f.Value)
	}
	switch f.Op {
	case OpEq:
		return actual == want, nil
	case OpNe:
		return actual != want, nil
	case OpGt:
		return actual > want, nil
	case OpGte:
		return actual >= want, nil
	case OpLt:
		return actual < want, nil
	default:
		return actual <= want, nil
	}
}

func compareAttrFilter(actual string, f Filter) (bool, error) {
	switch f.Op {
	case OpEq:
		return actual == f.Value, nil
	case OpNe:
		return actual != f.Value, nil
	}
	a, aErr := strconv.ParseFloat(actual, 64)
	b, bErr := strconv.ParseFloat(f.Value, 64)
	if aErr == nil && bErr == nil {
		switch f.Op {
		case OpGt:
			return a > b, nil
		case OpGte:
			return a >= b, nil
		case OpLt:
			return a < b, nil
		default:
			return a <= b, nil
		}
	}
	switch f.Op {
	case OpGt:
		return actual > f.Value, nil
	case OpGte:
		return actual >= f.Value, nil
	case OpLt:
		return actual < f.Value, nil
	default:
		return actual <= f.Value, nil
	}
}

// projectAttrs resolves a projection against the layer's declared
// attribute names and returns only the referenced columns, in the order
// written. An index past the record's attributes yields "".
func projectAttrs(layer engine.Layer, r *engine.Record, proj []AttrRef) ([]string, error) {
	out := make([]string, 0, len(proj))
	for _, a := range proj {
		idx := a.Index
		if a.ByName {
			i, ok := layer.AttrIndex(a.Name)
			if !ok {
				return nil, attrUnknownError(layer, a.Name)
			}
			idx = i
		}
		if idx < 0 || idx >= len(r.Attrs) {
			out = append(out, "")
			continue
		}
		out = append(out, r.Attrs[idx])
	}
	return out, nil
}

// execCreate creates one or more records (one assignment bracket per
// row), optionally under an existing parent, all in one commit. Without
// PARENT each record roots its own subtree. Ids are auto-assigned in
// row order and never collide.
func execCreate(ctx *ExecContext, branch string, s *CreateStmt) (*Result, error) {
	tx, err := ctx.DB.Begin(branch, ctx.Meta)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Result, error) {
		tx.Rollback()
		return nil, err
	}
	layers := tx.Layers()
	height, ok := layerHeight(layers, s.Layer)
	if !ok {
		return fail(fmt.Errorf("bql: unknown layer %q", s.Layer))
	}

	parentID := uint64(0)
	if s.Parent != nil {
		matches, err := matchRef(tx, *s.Parent)
		if err != nil {
			return fail(err)
		}
		if len(matches) != 1 {
			return fail(fmt.Errorf("bql: PARENT %s matched %d records (need exactly 1)", s.Parent, len(matches)))
		}
		parentID = matches[0].ID
	}

	var rows []Row
	for _, assigns := range s.Rows {
		attrs, err := applyAssignments(layers[height], nil, assigns)
		if err != nil {
			return fail(err)
		}
		rec, err := tx.Create(s.Layer, parentID, attrs)
		if err != nil {
			return fail(err)
		}
		rows = append(rows, recordRow(s.Layer, rec))
	}
	snap, err := tx.Commit()
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("created %d record(s) in %q", len(rows), s.Layer)
	if len(rows) == 1 {
		msg = fmt.Sprintf("created %s#%d (id=%d)", s.Layer, rows[0].Slot, rows[0].ID)
	}
	return writeResult(ctx, snap, msg, rows), nil
}

// execUpdate sets the listed attributes on every selected record in one
// commit; unlisted attributes keep their values. Zero matches is an
// error, never a silent no-op.
func execUpdate(ctx *ExecContext, branch string, s *UpdateStmt) (*Result, error) {
	tx, err := ctx.DB.Begin(branch, ctx.Meta)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Result, error) {
		tx.Rollback()
		return nil, err
	}
	matches, err := matchRef(tx, s.Target)
	if err != nil {
		return fail(err)
	}
	matches = applyMutationMods(matches, s.Modifiers)
	if len(matches) == 0 {
		return fail(fmt.Errorf("bql: no records in %q matched [%s]", s.Target.Layer, filtersString(s.Target.Filters)))
	}
	layers := tx.Layers()
	height, _ := layerHeight(layers, s.Target.Layer) // matchRef already validated the layer
	var rows []Row
	for _, r := range matches {
		attrs, err := applyAssignments(layers[height], r.Attrs, s.Assignments)
		if err != nil {
			return fail(err)
		}
		rec, err := tx.Update(s.Target.Layer, r.ID, attrs)
		if err != nil {
			return fail(err)
		}
		rows = append(rows, recordRow(s.Target.Layer, rec))
	}
	snap, err := tx.Commit()
	if err != nil {
		return nil, err
	}
	return writeResult(ctx, snap, fmt.Sprintf("updated %d record(s) in %q", len(matches), s.Target.Layer), rows), nil
}

// execDelete cascade-deletes every record matching the filters (and all
// of each root's descendants) in one commit. Without filters it empties
// the layer.
func execDelete(ctx *ExecContext, branch string, s *DeleteStmt) (*Result, error) {
	tx, err := ctx.DB.Begin(branch, ctx.Meta)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Result, error) {
		tx.Rollback()
		return nil, err
	}
	matches, err := matchRef(tx, s.Target)
	if err != nil {
		return fail(err)
	}
	matches = applyMutationMods(matches, s.Modifiers)
	if len(matches) == 0 {
		return fail(fmt.Errorf("bql: no records in %q matched [%s]", s.Target.Layer, filtersString(s.Target.Filters)))
	}
	total := 0
	for _, r := range matches {
		n, err := tx.Delete(s.Target.Layer, r.ID)
		if err != nil {
			return fail(err)
		}
		total += n
	}
	snap, err := tx.Commit()
	if err != nil {
		return nil, err
	}
	return writeResult(ctx, snap,
		fmt.Sprintf("cascade deleted %d record(s) under %d root(s) in %q",
			total, len(matches), s.Target.Layer), nil), nil
}

// maxAssignmentIndex caps positional assignments so a typo'd huge index
// cannot allocate an enormous attribute slice.
const maxAssignmentIndex = 1 << 20

// applyAssignments turns an assignment bracket into a full attribute
// list on top of base (nil for CREATE). Fields resolve by declared name
// (case-insensitive) or positional index; gaps fill with "", and
// positions past the end of base extend the record's attributes.
func applyAssignments(layer engine.Layer, base []string, assigns []Assignment) ([]string, error) {
	attrs := append([]string(nil), base...)
	for _, a := range assigns {
		idx, err := assignIndex(layer, a.Field)
		if err != nil {
			return nil, err
		}
		if idx < 0 || idx > maxAssignmentIndex {
			return nil, fmt.Errorf("bql: attribute index %d out of range (0..%d)", idx, maxAssignmentIndex)
		}
		for len(attrs) <= idx {
			attrs = append(attrs, "")
		}
		attrs[idx] = a.Value
	}
	return attrs, nil
}

// assignIndex resolves one assignment field to an attribute position:
// a numeric field is an index, anything else must be a declared
// attribute name. Builtin record fields (id, slot, parent) are not
// writable.
func assignIndex(layer engine.Layer, field string) (int, error) {
	switch strings.ToLower(field) {
	case "id", "slot", "parent":
		return 0, fmt.Errorf("bql: %s is a record field, not an attribute; it cannot be assigned", strings.ToLower(field))
	}
	if n, err := strconv.Atoi(field); err == nil {
		return n, nil
	}
	idx, ok := layer.AttrIndex(field)
	if !ok {
		return 0, attrUnknownError(layer, field)
	}
	return idx, nil
}

func writeResult(ctx *ExecContext, snap *engine.Snapshot, message string, rows []Row) *Result {
	res := &Result{Columns: recordColumns, Rows: rows, Message: message}
	if c, ok := ctx.DB.Commit(snap.Commit); ok {
		res.Commit = &CommitInfo{
			ID:        c.ID,
			Branch:    c.Branch,
			Parent:    c.Parent,
			Author:    c.Author,
			Summary:   c.Summary,
			Reason:    c.Reason,
			Timestamp: c.Timestamp,
		}
	}
	return res
}

func execShow(ctx *ExecContext, branch string, s *ShowStmt) (*Result, error) {
	switch s.Kind {
	case "LAYERS":
		snap, err := ctx.DB.Snapshot(branch)
		if err != nil {
			return nil, err
		}
		res := &Result{Columns: []string{"height", "name", "records"}}
		for _, l := range snap.Layers() {
			res.Rows = append(res.Rows, Row{Cells: []string{
				fmt.Sprintf("%d", l.Height), l.Name, fmt.Sprintf("%d", snap.Count(l.Height)),
			}})
		}
		return res, nil
	case "RECORDS":
		snap, err := ctx.DB.Snapshot(branch)
		if err != nil {
			return nil, err
		}
		res := &Result{Columns: recordColumns}
		for _, l := range snap.Layers() {
			if s.Layer != "" && l.Name != s.Layer {
				continue
			}
			for _, r := range snap.Records(l.Height) {
				res.Rows = append(res.Rows, recordRow(l.Name, r))
			}
		}
		res.Message = fmt.Sprintf("%d record(s) on branch %q", len(res.Rows), branch)
		return res, nil
	case "BRANCHES":
		res := &Result{Columns: []string{"name", "fork", "head"}}
		for _, b := range ctx.DB.Branches() {
			res.Rows = append(res.Rows, Row{Cells: []string{b.Name, b.Fork, fmt.Sprintf("%d", b.Head)}})
		}
		return res, nil
	case "COMMITS":
		res := &Result{Columns: []string{"id", "branch", "author", "summary", "reason", "timestamp"}}
		for _, e := range ctx.DB.Commits(branch) {
			if e.Commit != nil {
				c := e.Commit
				res.Rows = append(res.Rows, Row{Cells: []string{
					fmt.Sprintf("%d", c.ID), c.Branch, c.Author, c.Summary, c.Reason, fmt.Sprintf("%d", c.Timestamp),
				}})
			} else if e.Branch != nil {
				ev := e.Branch
				res.Rows = append(res.Rows, Row{Cells: []string{
					"branch", ev.Name, ev.Author, ev.Summary, ev.Reason, fmt.Sprintf("%d", ev.Timestamp),
				}})
			}
		}
		return res, nil
	}
	return nil, fmt.Errorf("bql: unsupported SHOW %s", s.Kind)
}

func execBranch(ctx *ExecContext, branch string, s *BranchStmt) (*Result, error) {
	switch s.Action {
	case "LIST":
		res := &Result{Columns: []string{"name", "fork", "head"}}
		for _, b := range ctx.DB.Branches() {
			res.Rows = append(res.Rows, Row{Cells: []string{b.Name, b.Fork, fmt.Sprintf("%d", b.Head)}})
		}
		return res, nil
	case "USE":
		if _, err := ctx.DB.Snapshot(s.Name); err != nil {
			return nil, err
		}
		return &Result{Message: fmt.Sprintf("switched to branch %q", s.Name), UseBranch: s.Name}, nil
	case "CREATE":
		from := s.From
		if from == "" {
			from = engine.DefaultBranch
		}
		if err := ctx.DB.CreateBranch(s.Name, from, ctx.Meta); err != nil {
			return nil, err
		}
		return &Result{Message: fmt.Sprintf("created branch %q from %q", s.Name, from)}, nil
	}
	return nil, fmt.Errorf("bql: unsupported BRANCH %s", s.Action)
}
