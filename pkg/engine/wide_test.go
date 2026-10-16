package engine

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// A 70 KB attribute used to commit, then make the whole file unopenable
// because the tuple length was a uint16 (issue #44).
func TestLargeAttributeSurvivesReopen(t *testing.T) {
	path := t.TempDir() + "/wide.bsl"
	db, err := Init(path, []Layer{{Height: 0, Name: "Files", Attrs: []string{"path", "body"}}}, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("a", 70_000)
	tx, err := db.Begin(DefaultBranch, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := tx.Create("Files", 0, []string{"SKILL.md", body})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Crash before checkpoint: the value has to survive journal replay.
	if err := db.abandon(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open after journal write: %v", err)
	}
	got, ok := mustRecord(t, db2, rec.ID)
	if !ok || got.Attrs[1] != body {
		t.Fatalf("replayed attribute len=%d ok=%v", attrLen(got), ok)
	}
	if err := db2.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if db2.Header().Version != CurrentVersion {
		t.Fatalf("version after checkpoint = %s", db2.Header().Version)
	}
	if err := db2.Close(); err != nil {
		t.Fatal(err)
	}

	db3, err := Open(path)
	if err != nil {
		t.Fatalf("Open after checkpoint: %v", err)
	}
	defer db3.Close()
	got, ok = mustRecord(t, db3, rec.ID)
	if !ok || got.Attrs[1] != body {
		t.Fatalf("checkpointed attribute len=%d ok=%v", attrLen(got), ok)
	}
}

// More than 65,535 children used to checkpoint into a truncated tuple.
func TestManyChildrenSurviveCheckpoint(t *testing.T) {
	path := t.TempDir() + "/kids.bsl"
	db, err := Init(path, []Layer{{Height: 0, Name: "Nodes"}}, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(DefaultBranch, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := tx.Create("Nodes", 0, []string{"root"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	const n = 70_000
	kids := make([]uint64, n)
	for i := range kids {
		kids[i] = uint64(i + 1)
	}
	snap, err := db.Snapshot(DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	cur := snap.layers[0].record(parent.ID)
	if cur == nil {
		t.Fatal("parent missing")
	}
	clone := cur.Clone()
	clone.Children = kids
	page := snap.layers[0].page(clone.Slot / SlotsPerPage)
	if err := page.Put(clone); err != nil {
		t.Fatal(err)
	}
	if err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db2.Close()
	got, ok := mustRecord(t, db2, parent.ID)
	if !ok {
		t.Fatal("parent missing after reopen")
	}
	if len(got.Children) != n || got.Children[n-1] != uint64(n) {
		t.Fatalf("children = %d, last=%d", len(got.Children), lastID(got.Children))
	}
}

// A file written by the v1.0 tuple layout still opens, and a later
// checkpoint rewrites it as the current version.
func TestV10FileStillOpens(t *testing.T) {
	path := t.TempDir() + "/v10.bsl"
	page := NewPage(0, 0)
	if err := page.Put(&Record{ID: 1, Slot: 0, Attrs: []string{"kept"}}); err != nil {
		t.Fatal(err)
	}
	pageBytes, err := page.marshalBinary(false)
	if err != nil {
		t.Fatal(err)
	}
	branch := "main"
	payload := make([]byte, 2+len(branch)+len(pageBytes))
	binary.LittleEndian.PutUint16(payload[:2], uint16(len(branch)))
	copy(payload[2:], branch)
	copy(payload[2+len(branch):], pageBytes)
	data := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(data[:4], uint32(len(payload)))
	copy(data[4:], payload)

	journalRec, err := marshalJournalCommitV10(&Commit{
		ID: 2, Branch: "main", Parent: 1, Timestamp: 1,
		Ops: []string{"CREATE Nodes#1 id=2 parent=0"},
	}, []Op{{Kind: opCreate, Layer: 0, Record: &Record{ID: 2, Slot: 1, Attrs: []string{"from-journal"}}}})
	if err != nil {
		t.Fatal(err)
	}

	hdr := &Header{
		Version:        VersionV10,
		Format:         FormatMatrixSlottedPage,
		Layers:         map[int]Layer{0: {Height: 0, Name: "Nodes"}},
		Branches:       []string{"main"},
		CommitLogStart: HeaderBlockSize,
		CommitLogEnd:   HeaderBlockSize,
		DataStart:      HeaderBlockSize,
		DataEnd:        HeaderBlockSize + uint64(len(data)),
		JournalStart:   HeaderBlockSize + uint64(len(data)),
	}
	block, err := hdr.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	buf := append(append(block, data...), journalRec...)
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open v1.0: %v", err)
	}
	if db.Header().Version != VersionV10 {
		t.Fatalf("version = %s, want %s", db.Header().Version, VersionV10)
	}
	snap, err := db.Snapshot(DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Count(0) != 2 {
		t.Fatalf("records = %d, want 2 (page + v1.0 journal)", snap.Count(0))
	}
	if err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open upgraded file: %v", err)
	}
	defer db2.Close()
	if db2.Header().Version != CurrentVersion {
		t.Fatalf("upgraded version = %s, want %s", db2.Header().Version, CurrentVersion)
	}
	snap, err = db2.Snapshot(DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Count(0) != 2 {
		t.Fatalf("records after upgrade = %d, want 2", snap.Count(0))
	}
}

func mustRecord(t *testing.T, db *DB, id uint64) (*Record, bool) {
	t.Helper()
	snap, err := db.Snapshot(DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	return snap.Record(0, id)
}

func attrLen(r *Record) int {
	if r == nil || len(r.Attrs) < 2 {
		return -1
	}
	return len(r.Attrs[1])
}

func lastID(ids []uint64) uint64 {
	if len(ids) == 0 {
		return 0
	}
	return ids[len(ids)-1]
}
