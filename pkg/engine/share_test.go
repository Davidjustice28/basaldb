package engine

import (
	"os"
	"testing"
)

func TestBranchSharesUnchangedPages(t *testing.T) {
	db := openTestDB(t)
	path := db.Path()
	tx, err := db.Begin(DefaultBranch, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	const n = 8000
	var last uint64
	for i := 0; i < n; i++ {
		rec, err := tx.Create("Accounts", 0, []string{"n"})
		if err != nil {
			t.Fatal(err)
		}
		last = rec.ID
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := residentIDEntries(db); got != 0 {
		t.Fatalf("resident id entries after open = %d, want 0", got)
	}
	snap, err := db.Snapshot(DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Count(0) != n {
		t.Fatalf("count = %d", snap.Count(0))
	}
	if _, ok := snap.Record(0, 1); !ok {
		t.Fatal("id lookup missed")
	}
	before := fileSize(t, path)
	if err := db.CreateBranch("scratch", DefaultBranch, testMeta()); err != nil {
		t.Fatal(err)
	}
	tx, err = db.Begin("scratch", testMeta())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Update("Accounts", last, []string{"changed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	after := fileSize(t, path)
	// A full page copy would about double the file. Growth should be one
	// page plus the scratch branch's id table.
	if after-before >= before/2 {
		t.Fatalf("file grew from %d to %d", before, after)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mainSnap, err := db.Snapshot(DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	scratch, err := db.Snapshot("scratch")
	if err != nil {
		t.Fatal(err)
	}
	if mainSnap.Count(0) != n || scratch.Count(0) != n {
		t.Fatalf("counts main=%d scratch=%d", mainSnap.Count(0), scratch.Count(0))
	}
	rec, ok := scratch.Record(0, last)
	if !ok || rec.Attrs[0] != "changed" {
		t.Fatalf("scratch record = %+v ok=%v", rec, ok)
	}
	rec, ok = mainSnap.Record(0, last)
	if !ok || rec.Attrs[0] != "n" {
		t.Fatalf("main record = %+v ok=%v", rec, ok)
	}
	if _, ok := mainSnap.Record(0, 1); !ok {
		t.Fatal("shared page id lookup missed on main")
	}
	if _, ok := scratch.Record(0, 1); !ok {
		t.Fatal("shared page id lookup missed on scratch")
	}
}

func residentIDEntries(db *DB) int {
	st := db.state.Load()
	total := 0
	for _, snap := range st.branches {
		for _, ls := range snap.layers {
			for cur := ls; cur != nil; cur = cur.base {
				total += len(cur.idIndex)
			}
		}
	}
	return total
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}
