package bql

import (
	"fmt"
	"strings"
	"testing"

	"basaldb/pkg/engine"
)

func TestParseGetPath(t *testing.T) {
	stmt, err := Parse(`GET Accounts[id=0] -> Teams -> Users`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	get, ok := stmt.(*GetStmt)
	if !ok {
		t.Fatalf("stmt = %T, want *GetStmt", stmt)
	}
	if len(get.Path) != 3 {
		t.Fatalf("path length = %d, want 3", len(get.Path))
	}
	if get.Path[0].Layer != "Accounts" || len(get.Path[0].Filters) != 1 ||
		get.Path[0].Filters[0] != (Filter{Field: "id", Op: OpEq, Value: "0"}) {
		t.Errorf("first step = %+v", get.Path[0])
	}
	if get.Path[1].Layer != "Teams" || len(get.Path[1].Filters) != 0 {
		t.Errorf("second step = %+v", get.Path[1])
	}
}

func TestParseSelectors(t *testing.T) {
	stmt, err := Parse(`GET Users[*][slot=1001]`)
	if err != nil {
		t.Fatal(err)
	}
	get := stmt.(*GetStmt)
	if get.Path[0].Layer != "Users" || len(get.Path[0].Filters) != 1 ||
		get.Path[0].Filters[0] != (Filter{Field: "slot", Op: OpEq, Value: "1001"}) {
		t.Errorf("step = %+v", get.Path[0])
	}
	// DELETE accepts [*] (every record in the layer) and filter brackets.
	if stmt, err := Parse(`DELETE Accounts[*]`); err != nil {
		t.Fatalf("[*] should parse: %v", err)
	} else if del := stmt.(*DeleteStmt); len(del.Target.Filters) != 0 {
		t.Errorf("filters = %+v, want none", del.Target.Filters)
	}
}

func TestParseMutationsAndBranches(t *testing.T) {
	cases := []struct {
		q    string
		want Statement
	}{
		{`CREATE Users PARENT Teams[id=101] [name="Sam",email="sam@dev.io"]`,
			&CreateStmt{Layer: "Users", Parent: &RecordRef{Layer: "Teams", Filters: []Filter{{Field: "id", Op: OpEq, Value: "101"}}}, Rows: [][]Assignment{{{Field: "name", Value: "Sam"}, {Field: "email", Value: "sam@dev.io"}}}}},
		// Multi-row CREATE: one assignment bracket per new record, one commit.
		{`CREATE Users [name="Sam"] [name="Ada"] [name="Linus"]`,
			&CreateStmt{Layer: "Users", Rows: [][]Assignment{{{Field: "name", Value: "Sam"}}, {{Field: "name", Value: "Ada"}}, {{Field: "name", Value: "Linus"}}}}},
		{`UPDATE Users[id=1001][name="Alex R",email="alex@dev.io"]`,
			&UpdateStmt{Target: RecordRef{Layer: "Users", Filters: []Filter{{Field: "id", Op: OpEq, Value: "1001"}}}, Assignments: []Assignment{{Field: "name", Value: "Alex R"}, {Field: "email", Value: "alex@dev.io"}}}},
		{`UPDATE Users[Name=David,Age=(gt=21)][name=D]`,
			&UpdateStmt{Target: RecordRef{Layer: "Users", Filters: []Filter{{Field: "Name", Op: OpEq, Value: "David"}, {Field: "Age", Op: OpGt, Value: "21"}}}, Assignments: []Assignment{{Field: "name", Value: "D"}}}},
		// UPDATE with repeated filter brackets (AND) + trailing sort/limit modifier.
		{`UPDATE Users[region=emea][active=yes][limit=10,sort=asc][name="X"]`,
			&UpdateStmt{Target: RecordRef{Layer: "Users", Filters: []Filter{{Field: "region", Op: OpEq, Value: "emea"}, {Field: "active", Op: OpEq, Value: "yes"}}}, Assignments: []Assignment{{Field: "name", Value: "X"}}, Modifiers: MutationModifiers{Limit: 10, Sort: "asc"}}},
		// Positional assignments and attribute-less creates parse too.
		{`CREATE Accounts [0="Acme",2="2026"]`,
			&CreateStmt{Layer: "Accounts", Rows: [][]Assignment{{{Field: "0", Value: "Acme"}, {Field: "2", Value: "2026"}}}}},
		{`DELETE Accounts[id=1]`, &DeleteStmt{Target: RecordRef{Layer: "Accounts", Filters: []Filter{{Field: "id", Op: OpEq, Value: "1"}}}}},
		{`DELETE Accounts[region=emea]`, &DeleteStmt{Target: RecordRef{Layer: "Accounts", Filters: []Filter{{Field: "region", Op: OpEq, Value: "emea"}}}}},
		{`DELETE Users[*]`, &DeleteStmt{Target: RecordRef{Layer: "Users"}}},
		// DELETE with sort/limit modifier bracket.
		{`DELETE Accounts[stale=yes][sort=desc][limit=1]`,
			&DeleteStmt{Target: RecordRef{Layer: "Accounts", Filters: []Filter{{Field: "stale", Op: OpEq, Value: "yes"}}}, Modifiers: MutationModifiers{Limit: 1, Sort: "desc"}}},
		{`SHOW LAYERS`, &ShowStmt{Kind: "LAYERS"}},
		{`show records Users`, &ShowStmt{Kind: "RECORDS", Layer: "Users"}},
		{`BRANCH LIST`, &BranchStmt{Action: "LIST"}},
		{`BRANCH USE dev`, &BranchStmt{Action: "USE", Name: "dev"}},
		{`BRANCH CREATE feature FROM main`, &BranchStmt{Action: "CREATE", Name: "feature", From: "main"}},
	}
	for _, tc := range cases {
		got, err := Parse(tc.q)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.q, err)
			continue
		}
		if !equalStmt(got, tc.want) {
			t.Errorf("Parse(%q) = %+v, want %+v", tc.q, got, tc.want)
		}
	}
}

func equalStmt(a, b Statement) bool {
	switch x := a.(type) {
	case *CreateStmt:
		y, ok := b.(*CreateStmt)
		return ok && x.Layer == y.Layer && sameRows(x.Rows, y.Rows) &&
			((x.Parent == nil) == (y.Parent == nil)) &&
			(x.Parent == nil || sameRef(*x.Parent, *y.Parent))
	case *UpdateStmt:
		y, ok := b.(*UpdateStmt)
		return ok && sameRef(x.Target, y.Target) && sameAssigns(x.Assignments, y.Assignments) && x.Modifiers == y.Modifiers
	case *DeleteStmt:
		y, ok := b.(*DeleteStmt)
		return ok && sameRef(x.Target, y.Target) && x.Modifiers == y.Modifiers
	case *ShowStmt:
		y, ok := b.(*ShowStmt)
		return ok && x.Kind == y.Kind && x.Layer == y.Layer
	case *BranchStmt:
		y, ok := b.(*BranchStmt)
		return ok && *x == *y
	}
	return false
}

func sameAssigns(a, b []Assignment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameRows(a, b [][]Assignment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameAssigns(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sameRef(a, b RecordRef) bool {
	if a.Layer != b.Layer || len(a.Filters) != len(b.Filters) {
		return false
	}
	for i := range a.Filters {
		if a.Filters[i] != b.Filters[i] {
			return false
		}
	}
	return true
}

func TestParseErrors(t *testing.T) {
	for _, q := range []string{
		``,
		`FROBNICATE Users`,
		`GET Accounts[`,
		`GET Accounts[#x]`,
		`GET "Users"`,
		`SHOW TABLES`,
		`BRANCH DELETE dev`,
		`CREATE Users PARENT Teams[id=1] ("x") EXTRA`, // old paren lists are gone
		`CREATE Users ("unterminated)`,
		`UPDATE Users[1] ("x")`,
		`UPDATE Users[id=1]`,                // no attribute bracket
		`UPDATE Users[id=1][]`,              // empty attribute bracket
		`UPDATE Users[id=1][name=A,name=B]`, // duplicate assignment
		`CREATE Users [name]`,               // assignment without a value
		`CREATE Users [name=(gt=1)]`,        // comparison operator in an assignment
		`DELETE Accounts[1]`,
		`DELETE Accounts[#1]`,
		`GET Accounts[0] ->`,
		`GET Users; DELETE Users[id=1]`,
	} {
		if _, err := Parse(q); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", q)
		}
	}
}

// ---- end-to-end ----

func setupDB(t *testing.T) (*engine.DB, *ExecContext) {
	t.Helper()
	db, err := engine.Init(t.TempDir()+"/bql.bsl", []engine.Layer{
		{Height: 0, Name: "Accounts"},
		{Height: 1, Name: "Teams"},
		{Height: 2, Name: "Users"},
	}, engine.TxMeta{Author: "Test", Summary: "init", Reason: "bql tests"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := &ExecContext{
		DB:     db,
		Branch: engine.DefaultBranch,
		Meta:   engine.TxMeta{Author: "Alex Rivera <alex@dev.io>", Summary: "seed", Reason: "bql tests"},
	}
	mustExec(t, ctx, `CREATE Accounts [0="Acme Corp",1="2026-01-01"]`)
	mustExec(t, ctx, `CREATE Accounts [0="Stark Ind",1="2026-02-15"]`)
	mustExec(t, ctx, `CREATE Teams PARENT Accounts[id=1] [0="Eng Team"]`)
	mustExec(t, ctx, `CREATE Teams PARENT Accounts[id=1] [0="Sales Team"]`)
	mustExec(t, ctx, `CREATE Teams PARENT Accounts[id=2] [0="R&D Team"]`)
	mustExec(t, ctx, `CREATE Users PARENT Teams[id=3] [0=Alex,1="alex@dev.io"]`)
	mustExec(t, ctx, `CREATE Users PARENT Teams[id=3] [0=Elena,1="elena@dev.io"]`)
	mustExec(t, ctx, `CREATE Users PARENT Teams[id=5] [0=Tony,1="tony@stark.io"]`)
	return db, ctx
}

func mustExec(t *testing.T, ctx *ExecContext, q string) *Result {
	t.Helper()
	res, err := Exec(ctx, q)
	if err != nil {
		t.Fatalf("Exec(%q): %v", q, err)
	}
	return res
}

func TestExecCreateAndGetPath(t *testing.T) {
	_, ctx := setupDB(t)

	res := mustExec(t, ctx, `GET Accounts[id=1] -> Teams -> Users`)
	// Acme Corp (id 1) -> Eng + Sales teams -> Alex + Elena
	if len(res.Rows) != 2 {
		t.Fatalf("GET path rows = %d, want 2 (%+v)", len(res.Rows), res.Rows)
	}
	names := map[string]bool{}
	for _, r := range res.Rows {
		names[r.Attrs[0]] = true
		if r.Layer != "Users" {
			t.Errorf("row layer = %q, want Users", r.Layer)
		}
	}
	if !names["Alex"] || !names["Elena"] {
		t.Errorf("traversal names = %v", names)
	}

	// Slot selector and children zero-join walk.
	res = mustExec(t, ctx, `GET Accounts[*]`)
	if len(res.Rows) != 2 {
		t.Fatalf("GET all accounts = %d rows, want 2", len(res.Rows))
	}
	res = mustExec(t, ctx, `SHOW RECORDS Users`)
	if len(res.Rows) != 3 {
		t.Fatalf("SHOW RECORDS Users = %d rows, want 3", len(res.Rows))
	}
}

func TestExecGetPathLayerMustBeAdjacent(t *testing.T) {
	_, ctx := setupDB(t)
	if _, err := Exec(ctx, `GET Accounts[id=1] -> Users`); err == nil {
		t.Fatal("non-adjacent path traversal succeeded, want error")
	}
}

func TestExecUpdateAndCommitMetadata(t *testing.T) {
	db, ctx := setupDB(t)
	res := mustExec(t, ctx, `UPDATE Users[id=6][0="Alex Rivera",1="alex@dev.io"]`)
	if res.Commit == nil {
		t.Fatal("write result carries no commit block")
	}
	if res.Commit.Author != "Alex Rivera <alex@dev.io>" || res.Commit.Summary == "" || res.Commit.Reason == "" {
		t.Errorf("commit block missing mandatory metadata: %+v", res.Commit)
	}
	c, ok := db.Commit(res.Commit.ID)
	if !ok || c.Summary != "seed" {
		t.Errorf("commit %d not in audit trail: %+v", res.Commit.ID, c)
	}
	snap, _ := db.Snapshot(engine.DefaultBranch)
	r, _ := snap.Record(2, 6)
	if r.Attrs[0] != "Alex Rivera" {
		t.Errorf("update not applied: %+v", r)
	}
}

func TestExecDeleteCascade(t *testing.T) {
	_, ctx := setupDB(t)
	res := mustExec(t, ctx, `DELETE Accounts[id=1]`)
	if !strings.Contains(res.Message, "5") { // account + 2 teams + 2 users
		t.Errorf("delete message = %q, want 5 records", res.Message)
	}
	res = mustExec(t, ctx, `SHOW RECORDS`)
	if len(res.Rows) != 3 { // Stark Ind + R&D Team + Tony survive
		t.Fatalf("records after cascade = %d rows, want 3", len(res.Rows))
	}
}

func TestExecCreateMultiRowAutoIDs(t *testing.T) {
	db, ctx := setupDB(t)

	// Three rows, one bracket each, all in one commit; ids auto-assign in
	// row order and never collide with the pre-seeded records.
	res := mustExec(t, ctx, `CREATE Users [0="Sam",1="sam@dev.io"] [0="Ada",1="ada@dev.io"] [0="Linus",1="linus@dev.io"]`)
	if len(res.Rows) != 3 {
		t.Fatalf("multi-row CREATE returned %d rows, want 3 (%+v)", len(res.Rows), res.Rows)
	}
	seen := map[uint64]bool{}
	want := []string{"Sam", "Ada", "Linus"}
	for i, r := range res.Rows {
		if seen[r.ID] {
			t.Errorf("duplicate auto-assigned id %d", r.ID)
		}
		seen[r.ID] = true
		if r.Attrs[0] != want[i] {
			t.Errorf("row %d attrs = %v, want %q first", i, r.Attrs, want[i])
		}
	}
	snap, _ := db.Snapshot(engine.DefaultBranch)
	if got := len(snap.Records(2)); got != 6 { // 3 seeded + 3 new
		t.Errorf("Users count = %d, want 6", got)
	}
}

func TestExecUpdateSortLimitAndRepeatedFilters(t *testing.T) {
	db, ctx := setupDB(t)

	// Touch only the two newest users (sort=desc + limit=2), setting their
	// email slot via a positional assignment. Repeated filter brackets AND
	// together, so the second bracket is a redundant match on every record.
	mustExec(t, ctx, `UPDATE Users[0=(ne="nobody")][0=(ne="nobody")][limit=2,sort=desc][1="touched@dev.io"]`)
	snap, _ := db.Snapshot(engine.DefaultBranch)
	touched := 0
	for _, r := range snap.Records(2) {
		if r.Attrs[1] == "touched@dev.io" {
			touched++
			if r.ID < 7 {
				t.Errorf("sort=desc+limit=2 touched id %d, want only the two newest (>=7)", r.ID)
			}
		}
	}
	if touched != 2 {
		t.Errorf("touched %d records, want 2", touched)
	}
}

func TestExecDeleteSortLimit(t *testing.T) {
	db, ctx := setupDB(t)

	// Delete only the newest top-level account (Stark Ind, id=2); its
	// subtree goes with it. Acme Corp and its family survive.
	res := mustExec(t, ctx, `DELETE Accounts[sort=desc][limit=1]`)
	if !strings.Contains(res.Message, "3") { // Stark Ind + R&D Team + Tony
		t.Errorf("delete message = %q, want 3 records", res.Message)
	}
	snap, _ := db.Snapshot(engine.DefaultBranch)
	if got := len(snap.Records(0)); got != 1 {
		t.Errorf("Accounts left = %d, want 1 (Acme Corp)", got)
	}
}

func TestExecRecordWritesDontRequireAuditMetadata(t *testing.T) {
	_, ctx := setupDB(t)
	ctx.Meta = engine.TxMeta{}

	// Record CRUD is metadata-free; the commit block simply carries none.
	res := mustExec(t, ctx, `CREATE Accounts [0="X"]`)
	if res.Commit == nil {
		t.Fatal("metadata-free write produced no commit block")
	}
	if res.Commit.Author != "" || res.Commit.Summary != "" || res.Commit.Reason != "" {
		t.Errorf("commit metadata = %+v, want empty", res.Commit)
	}
	mustExec(t, ctx, `UPDATE Accounts[id=9][0=Y]`)
	mustExec(t, ctx, `DELETE Accounts[id=9]`)

	// Structural operations still require the audit metadata.
	if _, err := Exec(ctx, `BRANCH CREATE bare`); err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("BRANCH CREATE without audit metadata = %v, want required error", err)
	}
}

func TestExecBranching(t *testing.T) {
	db, ctx := setupDB(t)
	mustExec(t, ctx, `BRANCH CREATE feature`)

	featCtx := *ctx
	featCtx.Branch = "feature"
	mustExec(t, &featCtx, `CREATE Accounts [0="Feature Co"]`)

	// main is unaffected: divergence without merge.
	res := mustExec(t, ctx, `SHOW RECORDS Accounts`)
	if len(res.Rows) != 2 {
		t.Errorf("main accounts = %d, want 2", len(res.Rows))
	}
	res = mustExec(t, &featCtx, `SHOW RECORDS Accounts`)
	if len(res.Rows) != 3 {
		t.Errorf("feature accounts = %d, want 3", len(res.Rows))
	}

	res = mustExec(t, ctx, `BRANCH LIST`)
	if len(res.Rows) != 2 {
		t.Fatalf("BRANCH LIST = %d rows, want 2", len(res.Rows))
	}
	res = mustExec(t, ctx, `BRANCH USE feature`)
	if res.UseBranch != "feature" {
		t.Errorf("BRANCH USE = %+v", res)
	}
	if _, err := Exec(ctx, `BRANCH USE nope`); err == nil {
		t.Error("BRANCH USE of unknown branch succeeded")
	}

	// Commits are per-branch and carry the provided metadata.
	res = mustExec(t, &featCtx, `SHOW COMMITS`)
	if len(res.Rows) < 2 {
		t.Errorf("feature commits = %d, want >= 2", len(res.Rows))
	}
	for _, r := range res.Rows {
		if len(r.Cells) >= 5 && (r.Cells[2] == "" || r.Cells[3] == "" || r.Cells[4] == "") {
			t.Errorf("commit row missing metadata: %v", r.Cells)
		}
	}
	_ = db
}

// ---- projection / filters / modifiers ----

func TestParseGetProjectionFiltersModifiers(t *testing.T) {
	stmt, err := Parse(`GET Accounts[Name,Age][Name=David,Age=(gt=21)][limit=1,sort=desc]`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	step := stmt.(*GetStmt).Path[0]
	if step.Layer != "Accounts" {
		t.Errorf("layer = %q", step.Layer)
	}
	if len(step.Project) != 2 || !step.Project[0].ByName || step.Project[0].Name != "Name" ||
		!step.Project[1].ByName || step.Project[1].Name != "Age" {
		t.Errorf("projection = %+v", step.Project)
	}
	if len(step.Filters) != 2 ||
		step.Filters[0] != (Filter{Field: "Name", Op: OpEq, Value: "David"}) ||
		step.Filters[1] != (Filter{Field: "Age", Op: OpGt, Value: "21"}) {
		t.Errorf("filters = %+v", step.Filters)
	}
	if step.Modifiers != (GetModifiers{Limit: 1, Sort: "desc"}) {
		t.Errorf("modifiers = %+v", step.Modifiers)
	}
}

func TestParseGetBracketForms(t *testing.T) {
	cases := []struct {
		q string
		// expectations on the parsed step
		proj    int
		filters int
		mods    GetModifiers
	}{
		{`GET Accounts[*]`, 0, 0, GetModifiers{}},
		{`GET Accounts`, 0, 0, GetModifiers{}},
		// A bare number is an attribute index, not a record id.
		{`GET Accounts[3]`, 1, 0, GetModifiers{}},
		// Filters in the first bracket are accepted (records = all).
		{`GET Accounts[Name=David]`, 0, 1, GetModifiers{}},
		{`GET Accounts[*][slot=0]`, 0, 1, GetModifiers{}},
		{`GET Accounts[0,1]`, 2, 0, GetModifiers{}},
		{`GET Accounts[Name][limit=5]`, 1, 0, GetModifiers{Limit: 5}},
		{`GET Accounts[*][sort=asc]`, 0, 0, GetModifiers{Sort: "asc"}},
		{`GET Accounts[*][Age=(lte=30),Age=(gte=18)]`, 0, 2, GetModifiers{}},
		// Repeated filter brackets AND together.
		{`GET Accounts[*][id=3][age=(gte=21)]`, 0, 2, GetModifiers{}},
	}
	for _, tc := range cases {
		stmt, err := Parse(tc.q)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.q, err)
			continue
		}
		step := stmt.(*GetStmt).Path[0]
		if len(step.Project) != tc.proj {
			t.Errorf("Parse(%q).Project = %+v, want %d items", tc.q, step.Project, tc.proj)
		}
		if len(step.Filters) != tc.filters {
			t.Errorf("Parse(%q).Filters = %+v, want %d", tc.q, step.Filters, tc.filters)
		}
		if step.Modifiers != tc.mods {
			t.Errorf("Parse(%q).Modifiers = %+v, want %+v", tc.q, step.Modifiers, tc.mods)
		}
	}
}

func TestParseGetBracketErrors(t *testing.T) {
	for _, q := range []string{
		`GET Accounts[1,]`,                            // trailing comma
		`GET Accounts[Name=]`,                         // missing value
		`GET Accounts[Name=(wat=1)]`,                  // unknown operator
		`GET Accounts[Name=(gt=1]`,                    // unterminated operator form
		`GET Accounts[Name=David,Age]`,                // mixing filters and attributes
		`GET Accounts[*,Name]`,                        // star in a projection
		`GET Accounts[#1]`,                            // '#' selectors are not part of GET
		`GET Accounts[#1,Name]`,                       // slot in a projection
		`GET Accounts[-1]`,                            // negative selector
		`GET Accounts[limit=(gt=1)]`,                  // operator on a modifier
		`GET Accounts[limit=many]`,                    // non-numeric limit
		`GET Accounts[sort=sideways]`,                 // bad sort direction
		`GET Accounts[*][limit=1][sort=asc][limit=2]`, // duplicate modifier bracket
		`GET Accounts[*][Name=A][Name,B]`,             // duplicate projection bracket
	} {
		if _, err := Parse(q); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", q)
		}
	}
}

// filterDB builds a container whose layers declare attribute names.
func filterDB(t *testing.T) (*engine.DB, *ExecContext) {
	t.Helper()
	db, err := engine.Init(t.TempDir()+"/filter.bsl", []engine.Layer{
		{Height: 0, Name: "Accounts", Attrs: []string{"name", "region"}},
		{Height: 1, Name: "Users", Attrs: []string{"name", "age"}},
	}, engine.TxMeta{Author: "Test", Summary: "init", Reason: "filter tests"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := &ExecContext{
		DB:     db,
		Branch: engine.DefaultBranch,
		Meta:   engine.TxMeta{Author: "Test <t@t.io>", Summary: "seed", Reason: "filter tests"},
	}
	mustExec(t, ctx, `CREATE Accounts [name=Acme,region=emea]`)
	mustExec(t, ctx, `CREATE Accounts [name=Globex,region=amer]`)
	mustExec(t, ctx, `CREATE Users PARENT Accounts[id=1] [name=David,age=34]`)
	mustExec(t, ctx, `CREATE Users PARENT Accounts[id=1] [name=Amy,age=19]`)
	mustExec(t, ctx, `CREATE Users PARENT Accounts[id=2] [name=Ravi,age=41]`)
	return db, ctx
}

func TestExecGetFiltersOperators(t *testing.T) {
	_, ctx := filterDB(t)

	// Equals on a declared attribute name (matching is case-insensitive).
	res := mustExec(t, ctx, `GET Accounts[*][name=Acme]`)
	if len(res.Rows) != 1 || res.Rows[0].Attrs[0] != "Acme" {
		t.Errorf("eq filter rows = %+v", res.Rows)
	}
	res = mustExec(t, ctx, `GET Accounts[NAME][Name=Acme]`)
	if len(res.Rows) != 1 || res.Rows[0].Attrs[0] != "Acme" {
		t.Errorf("case-insensitive rows = %+v", res.Rows)
	}
	// Not-equals, ordering on numbers stored as strings.
	res = mustExec(t, ctx, `GET Users[*][age=(gt=21)]`)
	if len(res.Rows) != 2 {
		t.Fatalf("gt filter rows = %+v", res.Rows)
	}
	res = mustExec(t, ctx, `GET Users[*][age=(lte=19)]`)
	if len(res.Rows) != 1 || res.Rows[0].Attrs[0] != "Amy" {
		t.Errorf("lte filter rows = %+v", res.Rows)
	}
	res = mustExec(t, ctx, `GET Users[*][age=(gte=19),age=(lte=34)]`)
	if len(res.Rows) != 2 {
		t.Errorf("range filter rows = %+v", res.Rows)
	}
	// Builtin fields.
	res = mustExec(t, ctx, `GET Accounts[*][id=2]`)
	if len(res.Rows) != 1 || res.Rows[0].Attrs[0] != "Globex" {
		t.Errorf("id filter rows = %+v", res.Rows)
	}
	res = mustExec(t, ctx, `GET Users[*][parent=1]`)
	if len(res.Rows) != 2 {
		t.Errorf("parent filter rows = %+v", res.Rows)
	}
	res = mustExec(t, ctx, `GET Users[*][id=4][slot=1]`)
	if len(res.Rows) != 1 || res.Rows[0].Attrs[0] != "Amy" {
		t.Errorf("selector+filter rows = %+v", res.Rows)
	}
	// Filters by attribute index.
	res = mustExec(t, ctx, `GET Accounts[*][0=Globex]`)
	if len(res.Rows) != 1 {
		t.Errorf("index filter rows = %+v", res.Rows)
	}
	// ne
	res = mustExec(t, ctx, `GET Accounts[*][region=(ne=emea)]`)
	if len(res.Rows) != 1 || res.Rows[0].Attrs[0] != "Globex" {
		t.Errorf("ne filter rows = %+v", res.Rows)
	}
	// A filter that matches nothing returns zero rows, not an error.
	res = mustExec(t, ctx, `GET Accounts[*][name=Nobody]`)
	if len(res.Rows) != 0 {
		t.Errorf("empty filter rows = %+v", res.Rows)
	}
}

func TestExecGetProjectionSortLimit(t *testing.T) {
	_, ctx := filterDB(t)

	// The example shape from the feature request.
	res := mustExec(t, ctx, `GET Users[name,age][age=(gt=18)][limit=2,sort=desc]`)
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %+v", res.Rows)
	}
	if res.Rows[0].Attrs[0] != "Ravi" || res.Rows[1].Attrs[0] != "Amy" {
		t.Errorf("sort=desc order = %v", res.Rows)
	}
	if len(res.Rows[0].Attrs) != 2 || res.Rows[0].Attrs[1] != "41" {
		t.Errorf("projection columns = %+v", res.Rows[0].Attrs)
	}

	// sort=asc + projection by index (ascending by record ID: David=3).
	res = mustExec(t, ctx, `GET Users[1,0][sort=asc]`)
	if len(res.Rows) != 3 || res.Rows[0].Attrs[0] != "34" || res.Rows[0].Attrs[1] != "David" {
		t.Errorf("index projection = %+v", res.Rows)
	}

	// limit=1 keeps the top row after sorting.
	res = mustExec(t, ctx, `GET Users[*][sort=desc,limit=1]`)
	if len(res.Rows) != 1 || res.Rows[0].Attrs[0] != "Ravi" {
		t.Errorf("limit rows = %+v", res.Rows)
	}

	// Filters mid-path prune the traversal.
	res = mustExec(t, ctx, `GET Accounts[*][name=Acme] -> Users[age=(lt=30)]`)
	if len(res.Rows) != 1 || res.Rows[0].Attrs[0] != "Amy" {
		t.Errorf("path filter rows = %+v", res.Rows)
	}

	// limit/sort on a non-final step is rejected.
	if _, err := Exec(ctx, `GET Users[limit=1] -> Accounts`); err == nil {
		t.Error("limit on a non-final step succeeded")
	}
}

func TestExecGetUnknownAttribute(t *testing.T) {
	_, ctx := filterDB(t)
	// Declared layer, unknown name: error lists the declared attributes.
	if _, err := Exec(ctx, `GET Users[Email]`); err == nil || !strings.Contains(err.Error(), "name, age") {
		t.Errorf("unknown attribute error = %v", err)
	}
	if _, err := Exec(ctx, `GET Users[*][Email=x]`); err == nil {
		t.Error("filter on unknown attribute succeeded")
	}
	// Layers without declared names: error explains how to declare them.
	db, err := engine.Init(t.TempDir()+"/plain.bsl", []engine.Layer{
		{Height: 0, Name: "Accounts"},
	}, engine.TxMeta{Author: "Test", Summary: "init", Reason: "tests"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	plain := &ExecContext{DB: db, Branch: engine.DefaultBranch,
		Meta: engine.TxMeta{Author: "Test", Summary: "seed", Reason: "tests"}}
	mustExec(t, plain, `CREATE Accounts [0="Acme"]`)
	_, err = Exec(plain, `GET Accounts[Name]`)
	if err == nil || !strings.Contains(err.Error(), "declares no attribute names") {
		t.Errorf("undeclared attribute error = %v", err)
	}
}

func TestExecDeleteByFilterMultipleRoots(t *testing.T) {
	_, ctx := filterDB(t)

	// A filter matching two records cascades both in one commit.
	// Ages: David 34, Amy 19, Ravi 41.
	res := mustExec(t, ctx, `DELETE Users[age=(lt=40)]`)
	if !strings.Contains(res.Message, "2 root(s)") {
		t.Errorf("delete message = %q", res.Message)
	}
	res = mustExec(t, ctx, `SHOW RECORDS`)
	if len(res.Rows) != 3 { // 2 accounts + Ravi survive
		t.Errorf("records after multi-root delete = %d, want 3", len(res.Rows))
	}

	// Without filters DELETE empties the layer.
	mustExec(t, ctx, `DELETE Users[*]`)
	res = mustExec(t, ctx, `SHOW RECORDS Users`)
	if len(res.Rows) != 0 {
		t.Errorf("users after DELETE Users[*] = %d, want 0", len(res.Rows))
	}

	// No matches is an error, never a silent no-op.
	if _, err := Exec(ctx, `DELETE Users[id=99]`); err == nil {
		t.Error("delete with no matches succeeded")
	}

	// UPDATE by attribute filter; the second bracket sets, and
	// attributes not listed keep their values (region survives).
	mustExec(t, ctx, `UPDATE Accounts[name=Globex][name="Globex Inc"]`)
	res = mustExec(t, ctx, `GET Accounts[name,region][name="Globex Inc"]`)
	if len(res.Rows) != 1 || len(res.Rows[0].Attrs) != 2 || res.Rows[0].Attrs[1] != "amer" {
		t.Errorf("update-by-filter rows = %+v", res.Rows)
	}

	// PARENT must resolve to exactly one record.
	if _, err := Exec(ctx, `CREATE Users PARENT Accounts[name=(ne=none)] [0="X"]`); err == nil {
		t.Error("PARENT matching many records succeeded")
	}
	mustExec(t, ctx, `CREATE Users PARENT Accounts[name="Globex Inc"] [name=Zoe,age=12]`)
	res = mustExec(t, ctx, `GET Users[name=Zoe]`)
	if len(res.Rows) != 1 || res.Rows[0].Parent != 2 {
		t.Errorf("create under filtered parent = %+v", res.Rows)
	}
}

// Point lookups resolve [id=N], [slot=N] and [parent=N] through the engine
// indexes instead of scanning the layer (issue #33). Results must match
// the scan path exactly: hits, misses, combined filters, mutations.
func TestIndexedPointLookups(t *testing.T) {
	_, ctx := setupDB(t)

	// id
	res := mustExec(t, ctx, `GET Users[id=6]`)
	if len(res.Rows) != 1 || res.Rows[0].Attrs[0] != "Alex" {
		t.Fatalf("GET Users[id=6] rows = %+v", res.Rows)
	}
	if res = mustExec(t, ctx, `GET Users[id=9999]`); len(res.Rows) != 0 {
		t.Errorf("missing id returned %d rows", len(res.Rows))
	}

	// slot
	slot := mustExec(t, ctx, `GET Users[id=8]`).Rows[0].Slot
	res = mustExec(t, ctx, fmt.Sprintf(`GET Users[slot=%d]`, slot))
	if len(res.Rows) != 1 || res.Rows[0].Attrs[0] != "Tony" {
		t.Fatalf("GET Users[slot=%d] rows = %+v", slot, res.Rows)
	}

	// parent: children of a record live one layer down.
	res = mustExec(t, ctx, `GET Users[parent=3]`)
	if len(res.Rows) != 2 {
		t.Fatalf("GET Users[parent=3] rows = %d, want 2", len(res.Rows))
	}

	// Combined indexed + attribute filters keep scan semantics.
	res = mustExec(t, ctx, `GET Users[parent=3][0=Alex]`)
	if len(res.Rows) != 1 || res.Rows[0].Attrs[0] != "Alex" {
		t.Fatalf("combined filter rows = %+v", res.Rows)
	}
	if res = mustExec(t, ctx, `GET Users[parent=3][0=Nobody]`); len(res.Rows) != 0 {
		t.Errorf("combined miss returned %d rows", len(res.Rows))
	}

	// Mutations resolve through the same fast path.
	mustExec(t, ctx, `UPDATE Users[id=8][1=tony@stark.io]`)
	res = mustExec(t, ctx, `GET Users[id=8]`)
	if len(res.Rows) != 1 || res.Rows[0].Attrs[1] != "tony@stark.io" {
		t.Fatalf("update via point lookup rows = %+v", res.Rows)
	}
	mustExec(t, ctx, `DELETE Users[id=7]`)
	if res = mustExec(t, ctx, `GET Users[id=7]`); len(res.Rows) != 0 {
		t.Errorf("deleted id returned %d rows", len(res.Rows))
	}
	if res = mustExec(t, ctx, `GET Users`); len(res.Rows) != 2 {
		t.Errorf("users after point delete = %d, want 2", len(res.Rows))
	}
}
