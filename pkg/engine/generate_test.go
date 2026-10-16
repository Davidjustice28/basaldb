package engine

import "testing"

func TestGenerateFlatOpensWithoutResidentIDs(t *testing.T) {
	path := t.TempDir() + "/flat.bsl"
	n, err := GenerateFlat(path, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1000 {
		t.Fatalf("records = %d", n)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := residentIDEntries(db); got != 0 {
		t.Fatalf("resident ids = %d", got)
	}
	snap, err := db.Snapshot(DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Count(0) != n {
		t.Fatalf("count = %d, want %d", snap.Count(0), n)
	}
	rec, ok := snap.Record(0, uint64(n))
	if !ok || rec.Attrs[0] != "user-value" {
		t.Fatalf("last record = %+v ok=%v", rec, ok)
	}
	if _, ok := snap.Record(0, uint64(n)+1); ok {
		t.Fatal("missing id reported present")
	}
}
