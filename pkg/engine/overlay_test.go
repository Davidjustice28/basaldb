package engine

import (
	"fmt"
	"testing"
)

// Write transactions publish copy-on-write overlays (issue #34). A long
// run of single-record commits must stay readable at every chain depth,
// including past the flatten threshold, and survive deletes, checkpoint
// and reopen.
func TestOverlayChainSurvivesFlattenAndReopen(t *testing.T) {
	path := t.TempDir() + "/chain.bsl"
	db, err := Init(path, []Layer{{Height: 0, Name: "Accounts"}}, testMeta())
	if err != nil {
		t.Fatal(err)
	}

	const commits = maxOverlayDepth + 40
	var lastID uint64
	for i := 0; i < commits; i++ {
		tx, err := db.Begin(DefaultBranch, testMeta())
		if err != nil {
			t.Fatal(err)
		}
		r, err := tx.Create("Accounts", 0, []string{fmt.Sprintf("acct-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		lastID = r.ID
	}

	snap, err := db.Snapshot(DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	if n := snap.Count(0); n != commits {
		t.Fatalf("count after %d overlay commits = %d, want %d", commits, n, commits)
	}
	recs := snap.Records(0)
	if len(recs) != commits {
		t.Fatalf("records = %d, want %d", len(recs), commits)
	}
	for i, r := range recs {
		if r.Slot != uint64(i) {
			t.Fatalf("records out of slot order at %d: slot %d", i, r.Slot)
		}
		if r.Attrs[0] != fmt.Sprintf("acct-%d", i) {
			t.Fatalf("record %d = %q, want acct-%d", i, r.Attrs[0], i)
		}
	}
	if r, ok := snap.Record(0, lastID); !ok || r.Attrs[0] != fmt.Sprintf("acct-%d", commits-1) {
		t.Fatalf("point lookup of last id %d failed (ok=%v)", lastID, ok)
	}

	// A delete through the overlay chain shadows base entries via
	// tombstones and must show up in count, scan and point lookups.
	tx, err := db.Begin(DefaultBranch, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Delete("Accounts", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	snap, _ = db.Snapshot(DefaultBranch)
	if n := snap.Count(0); n != commits-1 {
		t.Fatalf("count after delete = %d, want %d", n, commits-1)
	}
	if _, ok := snap.Record(0, 1); ok {
		t.Error("deleted id still resolvable")
	}
	if n := len(snap.Records(0)); n != commits-1 {
		t.Errorf("records after delete = %d, want %d", n, commits-1)
	}

	// Checkpoint folds the chain into the page region; the reopened
	// container must agree with the in-memory state.
	if err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	snap2, err := db2.Snapshot(DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	if n := snap2.Count(0); n != commits-1 {
		t.Fatalf("count after reopen = %d, want %d", n, commits-1)
	}
	if _, ok := snap2.Record(0, lastID); !ok {
		t.Error("last id lost across checkpoint/reopen")
	}
	if _, ok := snap2.Record(0, 1); ok {
		t.Error("deleted id survived checkpoint/reopen")
	}
}

// An old snapshot must keep seeing its own state while later commits
// mutate copy-on-write overlays on top of it (MVCC through the chain).
func TestSnapshotIsolationAcrossOverlays(t *testing.T) {
	db := openTestDB(t)
	before, err := db.Snapshot(DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	beforeCount := before.Count(0)

	for i := 0; i < 10; i++ {
		tx, err := db.Begin(DefaultBranch, testMeta())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Create("Accounts", 0, []string{fmt.Sprintf("acct-%d", i)}); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	if n := before.Count(0); n != beforeCount {
		t.Errorf("old snapshot count = %d, want %d (overlay leaked into base)", n, beforeCount)
	}
	after, _ := db.Snapshot(DefaultBranch)
	if n := after.Count(0); n != beforeCount+10 {
		t.Errorf("new snapshot count = %d, want %d", n, beforeCount+10)
	}
}
