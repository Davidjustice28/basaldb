package bql

import (
	"strings"
	"testing"

	"basaldb/pkg/engine"
)

func TestParseGetPath(t *testing.T) {
	stmt, err := Parse(`GET Accounts[0] -> Teams -> Users`)
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
	if get.Path[0].Layer != "Accounts" || get.Path[0].Sel.Kind != SelID || get.Path[0].Sel.N != 0 {
		t.Errorf("first step = %+v", get.Path[0])
	}
	if get.Path[1].Layer != "Teams" || get.Path[1].Sel.Kind != SelAll {
		t.Errorf("second step = %+v", get.Path[1])
	}
}

func TestParseSelectors(t *testing.T) {
	stmt, err := Parse(`GET Users[#1001]`)
	if err != nil {
		t.Fatal(err)
	}
	get := stmt.(*GetStmt)
	if get.Path[0].Sel.Kind != SelSlot || get.Path[0].Sel.N != 1001 {
		t.Errorf("selector = %+v", get.Path[0].Sel)
	}
	// Wildcard references parse, but the executor rejects them for DELETE
	// (a cascade must root at a single record).
	if stmt, err := Parse(`DELETE Accounts[*]`); err != nil {
		t.Fatalf("wildcard ref should parse: %v", err)
	} else if del := stmt.(*DeleteStmt); del.Ref.Sel.Kind != SelAll {
		t.Errorf("selector = %+v, want SelAll", del.Ref.Sel)
	}
}

func TestParseMutationsAndBranches(t *testing.T) {
	cases := []struct {
		q    string
		want Statement
	}{
		{`CREATE Users PARENT Teams[101] ("Sam", "sam@dev.io")`,
			&CreateStmt{Layer: "Users", Parent: &Ref{Layer: "Teams", Sel: Selector{Kind: SelID, N: 101}}, Attrs: []string{"Sam", "sam@dev.io"}}},
		{`UPDATE Users[1001] ("Alex R", "alex@dev.io")`,
			&UpdateStmt{Ref: Ref{Layer: "Users", Sel: Selector{Kind: SelID, N: 1001}}, Attrs: []string{"Alex R", "alex@dev.io"}}},
		{`DELETE Accounts[1]`, &DeleteStmt{Ref: Ref{Layer: "Accounts", Sel: Selector{Kind: SelID, N: 1}}}},
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
		return ok && x.Layer == y.Layer && sameAttrs(x.Attrs, y.Attrs) &&
			((x.Parent == nil) == (y.Parent == nil)) &&
			(x.Parent == nil || *x.Parent == *y.Parent)
	case *UpdateStmt:
		y, ok := b.(*UpdateStmt)
		return ok && x.Ref == y.Ref && sameAttrs(x.Attrs, y.Attrs)
	case *DeleteStmt:
		y, ok := b.(*DeleteStmt)
		return ok && x.Ref == y.Ref
	case *ShowStmt:
		y, ok := b.(*ShowStmt)
		return ok && x.Kind == y.Kind && x.Layer == y.Layer
	case *BranchStmt:
		y, ok := b.(*BranchStmt)
		return ok && *x == *y
	}
	return false
}

func sameAttrs(a, b []string) bool {
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

func TestParseErrors(t *testing.T) {
	for _, q := range []string{
		``,
		`FROBNICATE Users`,
		`GET Accounts[`,
		`GET Accounts[x]`,
		`GET "Users"`,
		`SHOW TABLES`,
		`BRANCH DELETE dev`,
		`CREATE Users PARENT Teams[1] ("x") EXTRA`,
		`CREATE Users ("unterminated)`,
		`GET Accounts[0] ->`,
		`GET Users; DELETE Users[1]`,
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
	mustExec(t, ctx, `CREATE Accounts ("Acme Corp", "2026-01-01")`)
	mustExec(t, ctx, `CREATE Accounts ("Stark Ind", "2026-02-15")`)
	mustExec(t, ctx, `CREATE Teams PARENT Accounts[1] ("Eng Team")`)
	mustExec(t, ctx, `CREATE Teams PARENT Accounts[1] ("Sales Team")`)
	mustExec(t, ctx, `CREATE Teams PARENT Accounts[2] ("R&D Team")`)
	mustExec(t, ctx, `CREATE Users PARENT Teams[3] ("Alex", "alex@dev.io")`)
	mustExec(t, ctx, `CREATE Users PARENT Teams[3] ("Elena", "elena@dev.io")`)
	mustExec(t, ctx, `CREATE Users PARENT Teams[5] ("Tony", "tony@stark.io")`)
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

	res := mustExec(t, ctx, `GET Accounts[1] -> Teams -> Users`)
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
	if _, err := Exec(ctx, `GET Accounts[1] -> Users`); err == nil {
		t.Fatal("non-adjacent path traversal succeeded, want error")
	}
}

func TestExecUpdateAndCommitMetadata(t *testing.T) {
	db, ctx := setupDB(t)
	res := mustExec(t, ctx, `UPDATE Users[6] ("Alex Rivera", "alex@dev.io")`)
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
	res := mustExec(t, ctx, `DELETE Accounts[1]`)
	if !strings.Contains(res.Message, "5") { // account + 2 teams + 2 users
		t.Errorf("delete message = %q, want 5 records", res.Message)
	}
	res = mustExec(t, ctx, `SHOW RECORDS`)
	if len(res.Rows) != 3 { // Stark Ind + R&D Team + Tony survive
		t.Fatalf("records after cascade = %d rows, want 3", len(res.Rows))
	}
}

func TestExecWritesRequireAuditMetadata(t *testing.T) {
	_, ctx := setupDB(t)
	ctx.Meta = engine.TxMeta{Author: "", Summary: "s", Reason: "r"}
	if _, err := Exec(ctx, `CREATE Accounts ("X")`); err == nil {
		t.Fatal("write without Author succeeded")
	}
	// Reads still work without metadata.
	if _, err := Exec(ctx, `GET Accounts[*]`); err != nil {
		t.Fatalf("read failed: %v", err)
	}
}

func TestExecBranching(t *testing.T) {
	db, ctx := setupDB(t)
	mustExec(t, ctx, `BRANCH CREATE feature`)

	featCtx := *ctx
	featCtx.Branch = "feature"
	mustExec(t, &featCtx, `CREATE Accounts ("Feature Co")`)

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

	// Commits are per-branch and carry mandatory metadata.
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
