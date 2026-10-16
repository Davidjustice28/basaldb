package bql

import (
	"encoding/json"
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

// MarshalJSON renders record rows with every built-in field present, zero
// values included and empty lists as [] instead of null, so a consumer can
// tell "slot 0" from "field missing" and "no children" from "unset"
// (#7). Catalog rows (SHOW / BRANCH) keep their omitempty shape: they are
// cell lists, not records.
func (r Row) MarshalJSON() ([]byte, error) {
	type cellsRow struct {
		Cells []string `json:"cells"`
	}
	type recordRowJSON struct {
		Layer    string   `json:"layer"`
		ID       uint64   `json:"id"`
		Slot     uint64   `json:"slot"`
		Parent   uint64   `json:"parent"`
		Children []uint64 `json:"children"`
		Attrs    []string `json:"attrs"`
	}
	if len(r.Cells) > 0 {
		return json.Marshal(cellsRow{Cells: r.Cells})
	}
	children, attrs := r.Children, r.Attrs
	if children == nil {
		children = []uint64{}
	}
	if attrs == nil {
		attrs = []string{}
	}
	return json.Marshal(recordRowJSON{
		Layer: r.Layer, ID: r.ID, Slot: r.Slot, Parent: r.Parent,
		Children: children, Attrs: attrs,
	})
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
	Columns []string `json:"columns"`
	// Fields names the attribute values carried in each record row's
	// Attrs, in order (projected names for a column selection, else the
	// layer's declared names). Empty when the layer declares none or the
	// rows are catalog cells. Delimited output (#28) uses it for headers.
	Fields    []string    `json:"fields,omitempty"`
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
	case *CreateIndexStmt:
		return execCreateIndex(ctx, s)
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

// layerByName looks a layer up by name. Layers() returns the declared
// layers in ascending height order but compacted, so height is only safe
// as an index when heights happen to be contiguous (0,1,2,...); matching by
// name always is.
func layerByName(layers []engine.Layer, name string) (engine.Layer, bool) {
	for _, l := range layers {
		if l.Name == name {
			return l, true
		}
	}
	return engine.Layer{}, false
}

// indexedEq returns the first equality filter on a builtin record field
// (id, slot, parent) with a parseable value. Those fields resolve through
// the engine indexes in constant time (issue #33); every other filter
// falls back to a layer scan. A non-numeric value falls back too, so the
// usual "needs a number" error still surfaces from filterRecords.
func indexedEq(filters []Filter) (field string, value uint64, ok bool) {
	for _, f := range filters {
		if f.Op != OpEq {
			continue
		}
		switch strings.ToLower(f.Field) {
		case "id", "slot", "parent":
			n, err := strconv.ParseUint(f.Value, 10, 64)
			if err != nil {
				return "", 0, false
			}
			return strings.ToLower(f.Field), n, true
		}
	}
	return "", 0, false
}

// snapPoint resolves a step's filters through the engine indexes when the
// step pins a builtin field to one value: id=N via the id index, slot=N
// via the slot lookup, parent=N via the parent's child pointer array. The
// second return value reports whether the fast path applied; callers still
// run filterRecords over the candidates, so combined filters keep their
// exact semantics.
func snapPoint(snap *engine.Snapshot, filters []Filter, h int) ([]*engine.Record, bool) {
	field, n, ok := indexedEq(filters)
	if !ok {
		return snapAttr(snap, filters, h)
	}
	switch field {
	case "id":
		if r, found := snap.Record(h, n); found {
			return []*engine.Record{r}, true
		}
		return nil, true
	case "slot":
		if r, found := snap.RecordBySlot(h, n); found {
			return []*engine.Record{r}, true
		}
		return nil, true
	default: // parent: a record's children live one layer up
		if n == 0 || h == 0 {
			return nil, false
		}
		return snap.Children(h-1, n), true
	}
}

// snapAttr resolves the first equality filter that has a secondary hash
// index. Remaining filters still run through filterRecords. A false return
// means no indexed attribute matched, so the caller scans.
func snapAttr(snap *engine.Snapshot, filters []Filter, h int) ([]*engine.Record, bool) {
	for _, f := range filters {
		if f.Op != OpEq {
			continue
		}
		switch strings.ToLower(f.Field) {
		case "id", "slot", "parent":
			continue
		}
		ids, ok := snap.IDsByAttr(h, f.Field, f.Value)
		if !ok {
			continue
		}
		out := make([]*engine.Record, 0, len(ids))
		for _, id := range ids {
			if r, found := snap.Record(h, id); found {
				out = append(out, r)
			}
		}
		return out, true
	}
	return nil, false
}

// matchRef resolves a RecordRef inside a write transaction to the
// records matching its filters.
func matchRef(tx *engine.WriteTx, ref RecordRef) ([]*engine.Record, error) {
	layers := tx.Layers()
	h, ok := layerHeight(layers, ref.Layer)
	if !ok {
		return nil, fmt.Errorf("bql: unknown layer %q", ref.Layer)
	}
	all, err := txRecords(tx, layers, h, ref)
	if err != nil {
		return nil, err
	}
	return filterRecords(layers, h, all, ref.Filters)
}

// txRecords lists the candidate records for a reference, using the engine
// indexes for builtin-field equality filters (see snapPoint) and a full
// layer scan otherwise.
func txRecords(tx *engine.WriteTx, layers []engine.Layer, h int, ref RecordRef) ([]*engine.Record, error) {
	if field, n, ok := indexedEq(ref.Filters); ok {
		switch field {
		case "id":
			if r, found := tx.Record(ref.Layer, n); found {
				return []*engine.Record{r}, nil
			}
			return nil, nil
		case "slot":
			if r, found := tx.RecordBySlot(ref.Layer, n); found {
				return []*engine.Record{r}, nil
			}
			return nil, nil
		case "parent":
			if n != 0 && h > 0 {
				return tx.Children(layers[h-1].Name, n)
			}
		}
	}
	for _, f := range ref.Filters {
		if f.Op != OpEq {
			continue
		}
		switch strings.ToLower(f.Field) {
		case "id", "slot", "parent":
			continue
		}
		ids, ok := tx.IDsByAttr(ref.Layer, f.Field, f.Value)
		if !ok {
			continue
		}
		out := make([]*engine.Record, 0, len(ids))
		for _, id := range ids {
			if r, found := tx.Record(ref.Layer, id); found {
				out = append(out, r)
			}
		}
		return out, nil
	}
	return tx.Records(ref.Layer)
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
//
// A limit on a layer scan stops the walk once enough records match
// (issue #46). Sort is by id, and ids follow slot order within a layer,
// so sort=desc walks slots backward and can stop early too.
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
	if err := snap.ReadErr(); err != nil {
		return nil, err
	}
	heights := map[string]int{}
	for _, l := range snap.Layers() {
		heights[l.Name] = l.Height
	}

	var cur []*engine.Record
	curHeight := 0
	for i, step := range s.Path {
		h, ok := heights[step.Layer]
		if !ok {
			return nil, fmt.Errorf("bql: unknown layer %q", step.Layer)
		}
		if i > 0 && h != curHeight+1 {
			return nil, fmt.Errorf("bql: path step %q must be the child layer of height %d (got height %d)",
				step.Layer, curHeight, h)
		}
		final := i == len(s.Path)-1
		var matched []*engine.Record
		if i == 0 {
			matched, err = matchStep(snap, h, step, final)
		} else if final && step.Modifiers.Limit > 0 && step.Modifiers.Sort == "" {
			matched, err = takeChildren(snap, curHeight, cur, h, step.Filters, step.Modifiers.Limit)
		} else {
			var next []*engine.Record
			for _, r := range cur {
				next = append(next, snap.Children(curHeight, r.ID)...)
			}
			matched, err = filterRecords(snap.Layers(), h, next, step.Filters)
			if err == nil && final {
				matched = orderRecords(matched, step.Modifiers.Sort, step.Modifiers.Limit)
			}
		}
		if err != nil {
			return nil, err
		}
		cur = matched
		curHeight = h
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
	if err := snap.ReadErr(); err != nil {
		return nil, err
	}
	var fields []string
	if len(final.Project) > 0 {
		fields = projectFields(finalLayer, final.Project)
	} else {
		fields = attrFields(finalLayer.Attrs, rows)
	}
	return &Result{
		Columns: recordColumns,
		Fields:  fields,
		Rows:    rows,
		Message: fmt.Sprintf("%d record(s) on branch %q", len(rows), branch),
	}, nil
}

// matchStep resolves the first step of a GET. A limited scan of the
// whole layer stops after enough matches; an indexed point lookup stays
// on the small candidate set.
func matchStep(snap *engine.Snapshot, h int, step GetStep, final bool) ([]*engine.Record, error) {
	mods := step.Modifiers
	if cands, point := snapPoint(snap, step.Filters, h); point {
		matched, err := filterRecords(snap.Layers(), h, cands, step.Filters)
		if err != nil {
			return nil, err
		}
		if final {
			matched = orderRecords(matched, mods.Sort, mods.Limit)
		}
		return matched, nil
	}
	if final && mods.Limit > 0 {
		matched, err := scanLimited(snap, h, step.Filters, mods.Limit, mods.Sort == "desc")
		if err != nil {
			return nil, err
		}
		// scanLimited already returned the right subset in slot or
		// reverse-slot order. orderRecords makes sort=asc/desc stable
		// without pulling in records the scan did not visit.
		return orderRecords(matched, mods.Sort, mods.Limit), nil
	}
	matched, err := filterRecords(snap.Layers(), h, snap.Records(h), step.Filters)
	if err != nil {
		return nil, err
	}
	if final {
		matched = orderRecords(matched, mods.Sort, mods.Limit)
	}
	return matched, nil
}

// scanLimited walks a layer in slot order (or reverse, for sort=desc)
// and stops after limit matches.
func scanLimited(snap *engine.Snapshot, h int, filters []Filter, limit int, reverse bool) ([]*engine.Record, error) {
	layers := snap.Layers()
	var out []*engine.Record
	var ferr error
	snap.Walk(h, reverse, func(r *engine.Record) bool {
		for _, f := range filters {
			ok, err := evalFilter(layers[h], r, f)
			if err != nil {
				ferr = err
				return false
			}
			if !ok {
				return true
			}
		}
		out = append(out, r)
		return len(out) < limit
	})
	if ferr != nil {
		return nil, ferr
	}
	return out, nil
}

// takeChildren collects matching children of parents and stops once limit
// rows are in hand. With no sort, parent order then child slot order is
// the result order, so the rest of the layer does not need to be read.
func takeChildren(snap *engine.Snapshot, parentH int, parents []*engine.Record, childH int, filters []Filter, limit int) ([]*engine.Record, error) {
	var out []*engine.Record
	layers := snap.Layers()
	for _, p := range parents {
		matched, err := filterRecords(layers, childH, snap.Children(parentH, p.ID), filters)
		if err != nil {
			return nil, err
		}
		out = append(out, matched...)
		if len(out) >= limit {
			return out[:limit], nil
		}
	}
	return out, nil
}

// orderRecords sorts by id when asked and then applies limit.
func orderRecords(recs []*engine.Record, sortOrder string, limit int) []*engine.Record {
	if sortOrder != "" {
		asc := sortOrder == "asc"
		sort.SliceStable(recs, func(i, j int) bool {
			if asc {
				return recs[i].ID < recs[j].ID
			}
			return recs[i].ID > recs[j].ID
		})
	}
	if limit > 0 && len(recs) > limit {
		recs = recs[:limit]
	}
	return recs
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

// projectFields names the projected columns for Result.Fields: a
// by-name reference keeps the name as written, a positional one resolves
// to the declared attribute name when the layer has one.
func projectFields(layer engine.Layer, proj []AttrRef) []string {
	out := make([]string, 0, len(proj))
	for _, a := range proj {
		if a.ByName {
			out = append(out, a.Name)
			continue
		}
		if a.Index >= 0 && a.Index < len(layer.Attrs) {
			out = append(out, layer.Attrs[a.Index])
			continue
		}
		out = append(out, strconv.Itoa(a.Index))
	}
	return out
}

// attrFields names a result's Attrs values: the layer's declared names
// where there are any, positional indices (BQL's own 0=, [0,1] syntax)
// past the declaration or for layers that declare none, sized to the
// widest row. Returns nil when there is nothing to name.
func attrFields(declared []string, rows []Row) []string {
	n := len(declared)
	for _, r := range rows {
		if len(r.Attrs) > n {
			n = len(r.Attrs)
		}
	}
	if n == 0 {
		return nil
	}
	out := make([]string, n)
	for i := range out {
		if i < len(declared) {
			out[i] = declared[i]
		} else {
			out[i] = strconv.Itoa(i)
		}
	}
	return out
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

func execCreateIndex(ctx *ExecContext, s *CreateIndexStmt) (*Result, error) {
	if err := ctx.DB.CreateIndex(s.Layer, s.Attr); err != nil {
		return nil, err
	}
	return &Result{Message: fmt.Sprintf("indexed %s(%s)", s.Layer, s.Attr)}, nil
}

// execCreate creates one or more records (one assignment bracket per
// row), optionally under an existing parent, all in one commit. Only the
// root layer may be written without PARENT; a child-layer record always
// hangs off an existing parent so no cascade delete can miss it (#5).
// Ids are auto-assigned in row order and never collide.
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
	if _, ok := layerHeight(layers, s.Layer); !ok {
		return fail(fmt.Errorf("bql: unknown layer %q", s.Layer))
	}
	layer, _ := layerByName(layers, s.Layer)
	if s.Parent == nil && layer.Height != layers[0].Height {
		// A record outside the root layer always hangs off a parent, so a
		// parentless one could never be reached by a cascade delete (#5).
		return fail(fmt.Errorf("bql: layer %q is not the root layer; CREATE %s needs PARENT <Layer>[filter] (root layer is %q)",
			s.Layer, s.Layer, layers[0].Name))
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
		attrs, err := applyAssignments(layer, nil, assigns)
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
	layer, _ := layerByName(layers, s.Target.Layer) // matchRef already validated the layer
	var rows []Row
	for _, r := range matches {
		attrs, err := applyAssignments(layer, r.Attrs, s.Assignments)
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
	if len(rows) > 0 {
		if l, ok := layerByName(snap.Layers(), rows[0].Layer); ok {
			res.Fields = attrFields(l.Attrs, rows)
		}
	}
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
		var single engine.Layer
		seen := 0
		for _, l := range snap.Layers() {
			if s.Layer != "" && l.Name != s.Layer {
				continue
			}
			for _, r := range snap.Records(l.Height) {
				res.Rows = append(res.Rows, recordRow(l.Name, r))
			}
			single, seen = l, seen+1
		}
		if s.Layer != "" && seen == 0 {
			return nil, fmt.Errorf("bql: unknown layer %q", s.Layer)
		}
		// Attribute columns only work when every row comes from one layer;
		// a multi-layer listing keeps them in a single attrs column.
		if seen == 1 {
			res.Fields = attrFields(single.Attrs, res.Rows)
		}
		res.Message = fmt.Sprintf("%d record(s) on branch %q", len(res.Rows), branch)
		return res, nil
	case "SCHEMA":
		snap, err := ctx.DB.Snapshot(branch)
		if err != nil {
			return nil, err
		}
		res := &Result{Columns: []string{"height", "layer", "attributes"}}
		for _, l := range snap.Layers() {
			if s.Layer != "" && l.Name != s.Layer {
				continue
			}
			res.Rows = append(res.Rows, Row{Cells: []string{
				fmt.Sprintf("%d", l.Height), l.Name, l.SchemaAttrs(),
			}})
		}
		if s.Layer != "" && len(res.Rows) == 0 {
			return nil, fmt.Errorf("bql: unknown layer %q", s.Layer)
		}
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
