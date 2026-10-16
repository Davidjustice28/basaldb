package engine

import (
	"fmt"
	"strings"
	"testing"
)

func TestAttrIndexUpdateDoesNotResurrectOldValue(t *testing.T) {
	root := newAttrIndex()
	root.add("old", 1)
	root.add("old", 2)

	ov := &attrIndex{base: root, put: map[string]map[uint64]struct{}{}}
	ov.drop("old", 1)
	ov.add("new", 1)

	if got := root.lookup("old"); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("parent index changed: %v", got)
	}
	if got := ov.lookup("old"); len(got) != 1 || got[0] != 2 {
		t.Fatalf("old value lookup = %v, want [2]", got)
	}
	if got := ov.lookup("new"); len(got) != 1 || got[0] != 1 {
		t.Fatalf("new value lookup = %v, want [1]", got)
	}
	flat := ov.flatten()
	if got := flat.lookup("old"); len(got) != 1 || got[0] != 2 {
		t.Fatalf("flattened old = %v", got)
	}
	if got := flat.lookup("new"); len(got) != 1 || got[0] != 1 {
		t.Fatalf("flattened new = %v", got)
	}
}

func TestParseByteSize(t *testing.T) {
	cases := map[string]int64{
		"256":    256,
		"256MB":  256 << 20,
		"256MiB": 256 << 20,
		"1K":     1 << 10,
		"2GB":    2 << 30,
	}
	for in, want := range cases {
		got, err := ParseByteSize(in)
		if err != nil {
			t.Errorf("ParseByteSize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseByteSize(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := ParseByteSize("nope"); err == nil {
		t.Error("ParseByteSize(nope) succeeded")
	}
}

func TestIndexedAttrRoundTrip(t *testing.T) {
	layers, err := ParseLayers("0:Pages(url!,status),1:Notes(body)")
	if err != nil {
		t.Fatal(err)
	}
	if !layers[0].IsIndexed("URL") || len(layers[0].Indexed) != 1 || layers[0].Indexed[0] != "url" {
		t.Fatalf("indexed = %+v", layers[0])
	}
	if layers[0].SchemaAttrs() != "url!, status" {
		t.Fatalf("schema attrs = %q", layers[0].SchemaAttrs())
	}
	h := validHeader()
	h.Layers = map[int]Layer{0: layers[0], 1: layers[1]}
	block, err := h.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(block), "0:Pages(url!,status)") {
		t.Fatalf("header missing bang:\n%s", block[:200])
	}
	got, err := ParseHeader(block)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Layers[0].IsIndexed("url") || got.Layers[0].Attrs[0] != "url" {
		t.Fatalf("reparsed = %+v", got.Layers[0])
	}
	if got.IndexStart != h.DataEnd || got.IndexEnd != h.DataEnd {
		t.Fatalf("index region = %d-%d, want %d", got.IndexStart, got.IndexEnd, h.DataEnd)
	}
}

func TestSecondaryIndexCreateUpdateDeleteBranchReopen(t *testing.T) {
	path := t.TempDir() + "/idx.bsl"
	layers, err := ParseLayers("0:Accounts(name!),1:Users(email!,role)")
	if err != nil {
		t.Fatal(err)
	}
	db, err := Init(path, layers, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(DefaultBranch, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	acct, err := tx.Create("Accounts", 0, []string{"Acme"})
	if err != nil {
		t.Fatal(err)
	}
	var users [3]*Record
	for i := 0; i < 3; i++ {
		users[i], err = tx.Create("Users", acct.ID, []string{fmt.Sprintf("u%d@x", i), "member"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	ids, ok := mustIDs(t, db, "Users", "email", "u1@x")
	if !ok || len(ids) != 1 || ids[0] != users[1].ID {
		t.Fatalf("lookup u1 = %v ok=%v", ids, ok)
	}
	if _, ok = mustIDs(t, db, "Users", "role", "member"); ok {
		t.Fatal("unindexed role reported an index")
	}
	if _, ok = mustIDs(t, db, "Users", "nope", "member"); ok {
		t.Fatal("missing attribute reported an index")
	}

	tx, err = db.Begin(DefaultBranch, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Update("Users", users[1].ID, []string{"moved@x", "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if ids, _ = mustIDs(t, db, "Users", "email", "u1@x"); len(ids) != 0 {
		t.Fatalf("old email still indexed: %v", ids)
	}
	if ids, _ = mustIDs(t, db, "Users", "email", "moved@x"); len(ids) != 1 || ids[0] != users[1].ID {
		t.Fatalf("new email = %v", ids)
	}

	if err := db.CreateBranch("scratch", DefaultBranch, testMeta()); err != nil {
		t.Fatal(err)
	}
	tx, err = db.Begin("scratch", testMeta())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Update("Accounts", acct.ID, []string{"Other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	mainIDs, _ := mustIDs(t, db, "Accounts", "name", "Acme")
	scratchIDs, _ := mustIDsBranch(t, db, "scratch", "Accounts", "name", "Other")
	if len(mainIDs) != 1 || mainIDs[0] != acct.ID {
		t.Fatalf("main name = %v", mainIDs)
	}
	if len(scratchIDs) != 1 || scratchIDs[0] != acct.ID {
		t.Fatalf("scratch name = %v", scratchIDs)
	}

	tx, err = db.Begin(DefaultBranch, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	n, err := tx.Delete("Accounts", acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("deleted = %d, want 4", n)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if ids, _ = mustIDs(t, db, "Accounts", "name", "Acme"); len(ids) != 0 {
		t.Fatalf("deleted account still indexed: %v", ids)
	}
	if ids, _ = mustIDs(t, db, "Users", "email", "u0@x"); len(ids) != 0 {
		t.Fatalf("cascaded user still indexed: %v", ids)
	}
	// The other branch keeps its own snapshot of the rows.
	if ids, _ = mustIDsBranch(t, db, "scratch", "Users", "email", "u0@x"); len(ids) != 1 {
		t.Fatalf("scratch lost the user: %v", ids)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.PageLoads() != 0 {
		t.Fatalf("open decoded %d pages, want 0", db.PageLoads())
	}
	if ids, _ = mustIDsBranch(t, db, "scratch", "Accounts", "name", "Other"); len(ids) != 1 {
		t.Fatalf("reopen scratch = %v", ids)
	}
	if ids, _ = mustIDs(t, db, "Users", "email", "u0@x"); len(ids) != 0 {
		t.Fatalf("reopen main user = %v", ids)
	}
	// One point read decodes a single page.
	snap, err := db.Snapshot("scratch")
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := snap.Record(0, scratchIDs[0])
	if !ok || rec.Attrs[0] != "Other" {
		t.Fatalf("record = %+v ok=%v", rec, ok)
	}
	if got := db.PageLoads(); got != 1 {
		t.Fatalf("page loads after one record = %d, want 1", got)
	}
}

func TestCreateIndexConflictsInFlightTx(t *testing.T) {
	layers, err := ParseLayers("0:Accounts(name,plan)")
	if err != nil {
		t.Fatal(err)
	}
	db, err := Init(t.TempDir()+"/ci.bsl", layers, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tx, err := db.Begin(DefaultBranch, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Create("Accounts", 0, []string{"Acme", "pro"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateIndex("Accounts", "name"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err == nil {
		t.Fatal("commit after CREATE INDEX succeeded, want ErrConflict")
	}
	if err := db.CreateIndex("Accounts", "name"); err != nil {
		t.Fatalf("second CREATE INDEX: %v", err)
	}
	if err := db.CreateIndex("Accounts", "missing"); err == nil {
		t.Fatal("index on missing attribute succeeded")
	}
}

func TestLazyOpenTouchesOnePage(t *testing.T) {
	path := t.TempDir() + "/lazy.bsl"
	layers, err := ParseLayers("0:Users(email!,role)")
	if err != nil {
		t.Fatal(err)
	}
	db, err := Init(path, layers, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	const n = 2048
	tx, err := db.Begin(DefaultBranch, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Create("Users", 0, []string{fmt.Sprintf("u%d@x", i), "m"}); err != nil {
			t.Fatal(err)
		}
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
	defer db.Close()
	if db.PageLoads() != 0 {
		t.Fatalf("open page loads = %d, want 0", db.PageLoads())
	}
	snap, err := db.Snapshot(DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Count(0) != n {
		t.Fatalf("count = %d, want %d", snap.Count(0), n)
	}
	miss, ok := snap.IDsByAttr(0, "email", "nobody@x")
	if !ok || len(miss) != 0 {
		t.Fatalf("miss = %v ok=%v", miss, ok)
	}
	if db.PageLoads() != 0 {
		t.Fatalf("index miss loaded %d pages", db.PageLoads())
	}
	hit, ok := snap.IDsByAttr(0, "email", "u10@x")
	if !ok || len(hit) != 1 {
		t.Fatalf("hit = %v ok=%v", hit, ok)
	}
	if _, ok := snap.Record(0, hit[0]); !ok {
		t.Fatal("indexed id missing")
	}
	if got := db.PageLoads(); got != 1 {
		t.Fatalf("loads after first hit = %d, want 1", got)
	}
	if _, ok := snap.Record(0, hit[0]); !ok {
		t.Fatal("second read failed")
	}
	if got := db.PageLoads(); got != 1 {
		t.Fatalf("cached reread loads = %d, want 1", got)
	}
	far, ok := snap.IDsByAttr(0, "email", "u1500@x")
	if !ok || len(far) != 1 {
		t.Fatalf("far = %v", far)
	}
	if _, ok := snap.Record(0, far[0]); !ok {
		t.Fatal("far record missing")
	}
	if got := db.PageLoads(); got != 2 {
		t.Fatalf("loads after second page = %d, want 2", got)
	}

	db.SetCacheLimit(1)
	if _, ok := snap.Record(0, hit[0]); !ok {
		t.Fatal("uncached reread failed")
	}
	if got := db.PageLoads(); got != 3 {
		t.Fatalf("loads with tiny cache = %d, want 3", got)
	}
}

func TestIndexReplayedFromJournal(t *testing.T) {
	path := t.TempDir() + "/journal.bsl"
	layers, err := ParseLayers("0:Pages(url!)")
	if err != nil {
		t.Fatal(err)
	}
	db, err := Init(path, layers, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(DefaultBranch, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	keep, err := tx.Create("Pages", 0, []string{"https://keep"})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := tx.Create("Pages", 0, []string{"https://old"})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := tx.Create("Pages", 0, []string{"https://gone"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.Begin(DefaultBranch, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Update("Pages", moved.ID, []string{"https://new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Delete("Pages", gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Crash before checkpoint so open has to rebuild the index from the journal.
	if err := db.abandon(); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if ids, ok := mustIDs(t, db2, "Pages", "url", "https://keep"); !ok || len(ids) != 1 || ids[0] != keep.ID {
		t.Fatalf("keep = %v ok=%v", ids, ok)
	}
	if ids, _ := mustIDs(t, db2, "Pages", "url", "https://old"); len(ids) != 0 {
		t.Fatalf("old url still indexed: %v", ids)
	}
	if ids, ok := mustIDs(t, db2, "Pages", "url", "https://new"); !ok || len(ids) != 1 || ids[0] != moved.ID {
		t.Fatalf("new url = %v ok=%v", ids, ok)
	}
	if ids, _ := mustIDs(t, db2, "Pages", "url", "https://gone"); len(ids) != 0 {
		t.Fatalf("deleted url still indexed: %v", ids)
	}
}

func mustIDs(t *testing.T, db *DB, layer, field, val string) ([]uint64, bool) {
	t.Helper()
	return mustIDsBranch(t, db, DefaultBranch, layer, field, val)
}

func mustIDsBranch(t *testing.T, db *DB, branch, layer, field, val string) ([]uint64, bool) {
	t.Helper()
	snap, err := db.Snapshot(branch)
	if err != nil {
		t.Fatal(err)
	}
	h, err := snap.height(layer)
	if err != nil {
		t.Fatal(err)
	}
	return snap.IDsByAttr(h, field, val)
}
