package engine

import (
	"errors"
	"strings"
	"testing"
)

func testMeta() TxMeta {
	return TxMeta{Author: "Alex Rivera <alex@dev.io>", Summary: "test commit", Reason: "unit test coverage"}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Init(t.TempDir()+"/test.bsl", []Layer{
		{Height: 0, Name: "Accounts"},
		{Height: 1, Name: "Teams"},
		{Height: 2, Name: "Users"},
	}, TxMeta{Author: "Alex Rivera <alex@dev.io>", Summary: "Initial seed data creation", Reason: "unit test fixture"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedTree builds the sample matrix from the project README:
// Accounts #1 -> Teams 101,102 -> Users; Account #2 -> Team 103 -> User.
func seedTree(t *testing.T, db *DB) map[string]uint64 {
	t.Helper()
	tx, err := db.Begin(DefaultBranch, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]uint64{}
	must := func(r *Record, err error) *Record {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	acc1 := must(tx.Create("Accounts", 0, []string{"Acme Corp", "2026-01-01"}))
	acc2 := must(tx.Create("Accounts", 0, []string{"Stark Ind", "2026-02-15"}))
	team101 := must(tx.Create("Teams", acc1.ID, []string{"Eng Team"}))
	team102 := must(tx.Create("Teams", acc1.ID, []string{"Sales Team"}))
	team103 := must(tx.Create("Teams", acc2.ID, []string{"R&D Team"}))
	must(tx.Create("Users", team101.ID, []string{"Alex", "alex@dev.io"}))
	must(tx.Create("Users", team101.ID, []string{"Elena", "elena@dev.io"}))
	must(tx.Create("Users", team103.ID, []string{"Tony", "tony@stark.io"}))
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ids["acc1"] = acc1.ID
	ids["acc2"] = acc2.ID
	ids["team101"] = team101.ID
	ids["team102"] = team102.ID
	ids["team103"] = team103.ID
	return ids
}

func TestCascadeDeleteInvalidatesSubtree(t *testing.T) {
	db := openTestDB(t)
	ids := seedTree(t, db)

	tx, err := db.Begin(DefaultBranch, TxMeta{Author: "Alex", Summary: "drop acme", Reason: "tenant churn"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := tx.Delete("Accounts", ids["acc1"])
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n != 5 { // account + 2 teams + 2 users under team 101
		t.Fatalf("deleted %d records, want 5", n)
	}
	if _, ok := tx.Record("Accounts", ids["acc1"]); ok {
		t.Error("root account still visible in tx")
	}
	if _, ok := tx.Record("Teams", ids["team101"]); ok {
		t.Error("child team still visible in tx")
	}
	if _, ok := tx.Record("Teams", ids["team102"]); ok {
		t.Error("child team still visible in tx")
	}
	// The other tenant must be untouched.
	if r, ok := tx.Record("Teams", ids["team103"]); !ok || r.Attrs[0] != "R&D Team" {
		t.Errorf("unrelated team damaged: %+v ok=%v", r, ok)
	}
	snap, err := tx.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Count(0) != 1 || snap.Count(1) != 1 || snap.Count(2) != 1 {
		t.Errorf("counts after cascade = %d/%d/%d, want 1/1/1",
			snap.Count(0), snap.Count(1), snap.Count(2))
	}
}

func TestCascadeDeleteWritesImmutableCommitLog(t *testing.T) {
	db := openTestDB(t)
	ids := seedTree(t, db)

	tx, _ := db.Begin(DefaultBranch, TxMeta{Author: "Alex Rivera <alex@dev.io>", Summary: "cascade delete account", Reason: "tenant offboarding"})
	if _, err := tx.Delete("Accounts", ids["acc1"]); err != nil {
		t.Fatal(err)
	}
	snap, err := tx.Commit()
	if err != nil {
		t.Fatal(err)
	}

	c, ok := db.Commit(snap.Commit)
	if !ok {
		t.Fatalf("commit %d missing from audit trail", snap.Commit)
	}
	// Mandatory Author, Summary, Reason on the commit block.
	if c.Author == "" || c.Summary == "" || c.Reason == "" {
		t.Errorf("commit block missing mandatory metadata: %+v", c)
	}
	text := c.Text()
	for _, want := range []string{"Author: Alex Rivera <alex@dev.io>", "Summary: cascade delete account", "Reason: tenant offboarding", "DELETE Accounts"} {
		if !strings.Contains(text, want) {
			t.Errorf("commit block missing %q:\n%s", want, text)
		}
	}
}

// Record writes are metadata-free: Begin accepts any TxMeta and the
// commit carries exactly what it was given. Structural operations
// (container Init, CreateBranch) still require the full audit metadata.
func TestRecordWritesAcceptAnyMetadata(t *testing.T) {
	db := openTestDB(t)
	for _, meta := range []TxMeta{
		{},
		{Author: "", Summary: "s", Reason: "r"},
		{Author: "a", Summary: "", Reason: "r"},
		{Author: "a", Summary: "s", Reason: ""},
		{Author: "   ", Summary: "s", Reason: "r"},
	} {
		tx, err := db.Begin(DefaultBranch, meta)
		if err != nil {
			t.Fatalf("Begin(%+v): %v", meta, err)
		}
		if _, err := tx.Create("Accounts", 0, []string{"x"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		snap, err := tx.Commit()
		if err != nil {
			t.Fatalf("Commit(%+v): %v", meta, err)
		}
		c, ok := db.Commit(snap.Commit)
		if !ok {
			t.Fatalf("commit %d missing from the audit trail", snap.Commit)
		}
		if c.Author != meta.Author || c.Summary != meta.Summary || c.Reason != meta.Reason {
			t.Errorf("commit %+v, want %+v", c, meta)
		}
	}

	// Structural operations still require the full metadata.
	if _, err := Init(t.TempDir()+"/meta.bsl",
		[]Layer{{Height: 0, Name: "Accounts"}}, TxMeta{Author: "a"}); err == nil {
		t.Error("Init without full metadata succeeded")
	}
	if err := db.CreateBranch("no-meta", DefaultBranch, TxMeta{Author: "a"}); err == nil {
		t.Error("CreateBranch without full metadata succeeded")
	}
}

func TestCascadeDeleteUnknownRecord(t *testing.T) {
	db := openTestDB(t)
	tx, _ := db.Begin(DefaultBranch, testMeta())
	if _, err := tx.Delete("Accounts", 999999); err == nil {
		t.Fatal("Delete of unknown record succeeded")
	}
}

func TestCascadeDeleteDetachesFromParent(t *testing.T) {
	db := openTestDB(t)
	ids := seedTree(t, db)
	tx, _ := db.Begin(DefaultBranch, testMeta())
	if _, err := tx.Delete("Teams", ids["team102"]); err != nil {
		t.Fatal(err)
	}
	parent, _ := tx.Record("Accounts", ids["acc1"])
	for _, c := range parent.Children {
		if c == ids["team102"] {
			t.Error("deleted team still referenced by parent child array")
		}
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteRecordWithMissingLowerLayer(t *testing.T) {
	db := openTestDB(t)
	// Deleting a Users record (leaf layer) is fine; Users has no lower layer.
	tx, _ := db.Begin(DefaultBranch, testMeta())
	r, err := tx.Create("Users", 0, []string{"Solo", "solo@dev.io"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Delete("Users", r.ID); err != nil {
		t.Fatalf("leaf delete: %v", err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitConflictOnSameBranch(t *testing.T) {
	db := openTestDB(t)
	seedTree(t, db)

	tx1, _ := db.Begin(DefaultBranch, testMeta())
	tx2, _ := db.Begin(DefaultBranch, testMeta())
	if _, err := tx1.Create("Accounts", 0, []string{"A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Create("Accounts", 0, []string{"B"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx1.Commit(); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if _, err := tx2.Commit(); !errors.Is(err, ErrConflict) {
		t.Fatalf("second commit err = %v, want ErrConflict", err)
	}
	// The transaction stays aborted and cannot be reused.
	if _, err := tx2.Commit(); err == nil {
		t.Fatal("finished tx accepted a second commit")
	}
}
