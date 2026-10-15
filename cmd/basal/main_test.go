package main

import (
	"bytes"
	"strings"
	"testing"

	"basaldb/pkg/bql"
	"basaldb/pkg/engine"
)

// A write without audit metadata prints just "commit N on BRANCH"; set
// metadata keeps the author | summary | reason positions without trailing
// empty pipes (#8).
func TestCommitLine(t *testing.T) {
	cases := []struct {
		commit bql.CommitInfo
		want   string
	}{
		{bql.CommitInfo{ID: 2, Branch: "main"}, "commit 2 on main"},
		{bql.CommitInfo{ID: 3, Branch: "main", Author: "Alex <a@x.io>"},
			"commit 3 on main | Alex <a@x.io>"},
		{bql.CommitInfo{ID: 4, Branch: "main", Summary: "why"},
			"commit 4 on main |  | why"},
		{bql.CommitInfo{ID: 5, Branch: "scratch", Author: "A", Summary: "S", Reason: "R"},
			"commit 5 on scratch | A | S | R"},
	}
	for _, tc := range cases {
		if got := commitLine(&tc.commit); got != tc.want {
			t.Errorf("commitLine(%+v) = %q, want %q", tc.commit, got, tc.want)
		}
	}
}

// CSV/TSV put one column per named field on stdout, with the message and
// commit line kept off it so the output stays machine-readable (#28).
func TestPrintDelimited(t *testing.T) {
	record := func() *bql.Result {
		return &bql.Result{
			Columns: []string{"layer", "id", "slot", "parent", "children", "attrs"},
			Fields:  []string{"name", "email"},
			Rows: []bql.Row{{
				Layer: "Users", ID: 9, Slot: 0, Parent: 5,
				Children: []uint64{}, Attrs: []string{"Kim", "kim@globex.io"},
			}},
			Message: `1 record(s) on branch "main"`,
		}
	}

	var buf bytes.Buffer
	if err := printDelimited(&buf, record(), "csv"); err != nil {
		t.Fatalf("printDelimited: %v", err)
	}
	want := "layer,id,slot,parent,children,name,email\nUsers,9,0,5,,Kim,kim@globex.io\n"
	if buf.String() != want {
		t.Errorf("csv output = %q, want %q", buf.String(), want)
	}

	buf.Reset()
	if err := printDelimited(&buf, record(), "tsv"); err != nil {
		t.Fatalf("printDelimited: %v", err)
	}
	want = "layer\tid\tslot\tparent\tchildren\tname\temail\nUsers\t9\t0\t5\t\tKim\tkim@globex.io\n"
	if buf.String() != want {
		t.Errorf("tsv output = %q, want %q", buf.String(), want)
	}

	// Catalog rows are their cell list; attribute lists with commas are
	// quoted by the CSV writer.
	buf.Reset()
	schema := &bql.Result{
		Columns: []string{"height", "layer", "attributes"},
		Rows: []bql.Row{
			{Cells: []string{"0", "Accounts", "name, plan"}},
			{Cells: []string{"2", "Users", ""}},
		},
	}
	if err := printDelimited(&buf, schema, "csv"); err != nil {
		t.Fatalf("printDelimited: %v", err)
	}
	want = "height,layer,attributes\n0,Accounts,\"name, plan\"\n2,Users,\n"
	if buf.String() != want {
		t.Errorf("schema csv output = %q, want %q", buf.String(), want)
	}

	// No field names: attributes stay one quoted column.
	buf.Reset()
	full := record()
	full.Fields = nil
	if err := printDelimited(&buf, full, "csv"); err != nil {
		t.Fatalf("printDelimited: %v", err)
	}
	if want := "layer,id,slot,parent,children,attrs\nUsers,9,0,5,,Kim kim@globex.io\n"; buf.String() != want {
		t.Errorf("unprojected csv output = %q, want %q", buf.String(), want)
	}
}

// Table output keeps the human notes on the same stream as the rows and
// the commit line rendered by commitLine.
func TestPrintResultTable(t *testing.T) {
	res := &bql.Result{
		Columns: []string{"layer", "id", "slot", "parent", "children", "attrs"},
		Fields:  []string{"name"},
		Rows:    []bql.Row{{Layer: "Accounts", ID: 1, Slot: 0, Attrs: []string{"Acme"}}},
		Message: "created Accounts#0 (id=1)",
		Commit:  &bql.CommitInfo{ID: 2, Branch: "main"},
	}
	var out, notes bytes.Buffer
	if err := printResult(&out, &notes, res, "table"); err != nil {
		t.Fatalf("printResult: %v", err)
	}
	for _, want := range []string{
		"layer | id | slot | parent | children | attrs",
		`Accounts[1#0] parent=0 children=[] "Acme"`,
		"created Accounts#0 (id=1)",
		"commit 2 on main",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("table output missing %q:\n%s", want, out.String())
		}
	}
	if notes.Len() != 0 {
		t.Errorf("table notes on stderr: %q", notes.String())
	}

	// Delimited output moves message and commit to the notes stream.
	out.Reset()
	notes.Reset()
	if err := printResult(&out, &notes, res, "csv"); err != nil {
		t.Fatalf("printResult: %v", err)
	}
	if strings.Contains(out.String(), "commit") || strings.Contains(out.String(), "created") {
		t.Errorf("csv stdout carries notes:\n%s", out.String())
	}
	if !strings.Contains(notes.String(), "commit 2 on main") {
		t.Errorf("csv notes = %q, want the commit line", notes.String())
	}
}

// The version banner carries the build stamp and the .bsl format (#27).
func TestVersionText(t *testing.T) {
	got := versionText()
	for _, want := range []string{"basal " + version, "commit " + commit, "built " + date, "bsl format: " + engine.CurrentVersion} {
		if !strings.Contains(got, want) {
			t.Errorf("version text missing %q: %q", want, got)
		}
	}
}
