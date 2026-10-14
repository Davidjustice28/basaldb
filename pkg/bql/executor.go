package bql

import (
	"fmt"

	"basaldb/pkg/engine"
)

// ExecContext binds a query to a database, a branch and the mandatory audit
// metadata required by every write statement.
type ExecContext struct {
	DB     *engine.DB
	Branch string
	Meta   engine.TxMeta // Author, Summary, Reason (required for writes)
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

func matchSel(r *engine.Record, sel Selector) bool {
	switch sel.Kind {
	case SelAll:
		return true
	case SelID:
		return r.ID == sel.N
	case SelSlot:
		return r.Slot == sel.N
	}
	return false
}

// execGet runs a path traversal (zero join) over an immutable MVCC
// snapshot.
func execGet(ctx *ExecContext, branch string, s *GetStmt) (*Result, error) {
	if len(s.Path) == 0 {
		return nil, fmt.Errorf("bql: GET requires a starting layer")
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
	cur, err := selectRecords(snap, h, first)
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
			for _, child := range snap.Children(curHeight, r.ID) {
				if matchSel(child, step.Sel) {
					next = append(next, child)
				}
			}
		}
		cur = next
		curHeight = stepH
	}

	rows := make([]Row, 0, len(cur))
	for _, r := range cur {
		rows = append(rows, recordRow(snap.Layers()[curHeight].Name, r))
	}
	return &Result{
		Columns: recordColumns,
		Rows:    rows,
		Message: fmt.Sprintf("%d record(s) on branch %q", len(rows), branch),
	}, nil
}

func selectRecords(snap *engine.Snapshot, height int, ref Ref) ([]*engine.Record, error) {
	switch ref.Sel.Kind {
	case SelAll:
		return snap.Records(height), nil
	case SelID:
		if r, ok := snap.Record(height, ref.Sel.N); ok {
			return []*engine.Record{r}, nil
		}
		return nil, fmt.Errorf("bql: record %d not found in layer %q", ref.Sel.N, ref.Layer)
	case SelSlot:
		if r, ok := snap.RecordBySlot(height, ref.Sel.N); ok {
			return []*engine.Record{r}, nil
		}
		return nil, fmt.Errorf("bql: slot #%d not found in layer %q", ref.Sel.N, ref.Layer)
	}
	return nil, fmt.Errorf("bql: bad selector %v", ref.Sel)
}

// execCreate creates a record, optionally under an existing parent, in
// one commit. Without PARENT the record roots its own subtree.
func execCreate(ctx *ExecContext, branch string, s *CreateStmt) (*Result, error) {
	tx, err := ctx.DB.Begin(branch, ctx.Meta)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Result, error) {
		tx.Rollback()
		return nil, err
	}
	if _, ok := layerHeight(tx.Layers(), s.Layer); !ok {
		return fail(fmt.Errorf("bql: unknown layer %q", s.Layer))
	}

	parentID := uint64(0)
	if s.Parent != nil {
		if s.Parent.Sel.Kind == SelAll {
			return fail(fmt.Errorf("bql: PARENT %s must select a single record ([N] or [#N])", s.Parent))
		}
		var rec *engine.Record
		if s.Parent.Sel.Kind == SelID {
			if r, ok := tx.Record(s.Parent.Layer, s.Parent.Sel.N); ok {
				rec = r
			}
		} else if r, ok := tx.RecordBySlot(s.Parent.Layer, s.Parent.Sel.N); ok {
			rec = r
		}
		if rec == nil {
			return fail(fmt.Errorf("bql: parent %s not found", s.Parent))
		}
		parentID = rec.ID
	}

	rec, err := tx.Create(s.Layer, parentID, s.Attrs)
	if err != nil {
		return fail(err)
	}
	snap, err := tx.Commit()
	if err != nil {
		return nil, err
	}
	return writeResult(ctx, snap, fmt.Sprintf("created %s#%d (id=%d)", s.Layer, rec.Slot, rec.ID),
		[]Row{recordRow(s.Layer, rec)}), nil
}

// execUpdate applies the attribute replacement to every selected record
// in one commit. Zero matches is an error, never a silent no-op.
func execUpdate(ctx *ExecContext, branch string, s *UpdateStmt) (*Result, error) {
	tx, err := ctx.DB.Begin(branch, ctx.Meta)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Result, error) {
		tx.Rollback()
		return nil, err
	}
	if _, ok := layerHeight(tx.Layers(), s.Ref.Layer); !ok {
		return fail(fmt.Errorf("bql: unknown layer %q", s.Ref.Layer))
	}
	all, err := tx.Records(s.Ref.Layer)
	if err != nil {
		return fail(err)
	}
	var matches []*engine.Record
	for _, r := range all {
		if matchSel(r, s.Ref.Sel) {
			matches = append(matches, r)
		}
	}
	if len(matches) == 0 {
		return fail(fmt.Errorf("bql: no records in %q matched %s", s.Ref.Layer, s.Ref))
	}
	var rows []Row
	for _, r := range matches {
		rec, err := tx.Update(s.Ref.Layer, r.ID, s.Attrs)
		if err != nil {
			return fail(err)
		}
		rows = append(rows, recordRow(s.Ref.Layer, rec))
	}
	snap, err := tx.Commit()
	if err != nil {
		return nil, err
	}
	return writeResult(ctx, snap, fmt.Sprintf("updated %d record(s) in %q", len(matches), s.Ref.Layer), rows), nil
}

// execDelete cascade-deletes the selected record and every descendant in
// one commit. A wildcard cannot root a cascade: the engine needs a single
// record to prune from.
func execDelete(ctx *ExecContext, branch string, s *DeleteStmt) (*Result, error) {
	if s.Ref.Sel.Kind == SelAll {
		return nil, fmt.Errorf("bql: DELETE %s must select a single record ([N] or [#N])", s.Ref)
	}
	tx, err := ctx.DB.Begin(branch, ctx.Meta)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Result, error) {
		tx.Rollback()
		return nil, err
	}
	if _, ok := layerHeight(tx.Layers(), s.Ref.Layer); !ok {
		return fail(fmt.Errorf("bql: unknown layer %q", s.Ref.Layer))
	}
	var root *engine.Record
	if s.Ref.Sel.Kind == SelID {
		if r, ok := tx.Record(s.Ref.Layer, s.Ref.Sel.N); ok {
			root = r
		}
	} else if r, ok := tx.RecordBySlot(s.Ref.Layer, s.Ref.Sel.N); ok {
		root = r
	}
	if root == nil {
		return fail(fmt.Errorf("bql: record %s not found", s.Ref))
	}
	total, err := tx.Delete(s.Ref.Layer, root.ID)
	if err != nil {
		return fail(err)
	}
	snap, err := tx.Commit()
	if err != nil {
		return nil, err
	}
	return writeResult(ctx, snap,
		fmt.Sprintf("cascade deleted %d record(s) under %s", total, s.Ref), nil), nil
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
