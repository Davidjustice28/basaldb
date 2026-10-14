package engine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestSnapshotIsolationBetweenWrites(t *testing.T) {
	db := openTestDB(t)
	ids := seedTree(t, db)

	before, _ := db.Snapshot(DefaultBranch)
	tx, _ := db.Begin(DefaultBranch, testMeta())
	if _, err := tx.Create("Users", ids["team102"], []string{"Pepper", "pepper@stark.io"}); err != nil {
		t.Fatal(err)
	}
	snap2, err := tx.Commit()
	if err != nil {
		t.Fatal(err)
	}

	// The old snapshot must not observe the new write (MVCC).
	if got := before.Count(2); got != 3 {
		t.Errorf("old snapshot users = %d, want 3", got)
	}
	if got := snap2.Count(2); got != 4 {
		t.Errorf("new snapshot users = %d, want 4", got)
	}
}

func TestReadsAndWritesAtTheSameTime(t *testing.T) {
	db := openTestDB(t)
	ids := seedTree(t, db)

	const writers = 4
	const perWriter = 25
	var writersWG sync.WaitGroup
	var readersWG sync.WaitGroup
	errCh := make(chan error, writers+2)
	stop := make(chan struct{})

	// Readers loop over snapshots while writers commit concurrently. Each
	// snapshot must stay self-consistent for its entire read loop.
	readersWG.Add(1)
	go func() {
		defer readersWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			snap, err := db.Snapshot(DefaultBranch)
			if err != nil {
				errCh <- err
				return
			}
			n := snap.Count(2)
			for i := 0; i < 20; i++ {
				// Same snapshot, stable view: the count never changes
				// under concurrent commits.
				if got := snap.Count(2); got != n {
					errCh <- fmt.Errorf("snapshot mutated under readers: %d -> %d", n, got)
					return
				}
				for _, r := range snap.Records(2) {
					if _, ok := snap.Record(2, r.ID); !ok {
						errCh <- fmt.Errorf("record %d vanished inside a snapshot", r.ID)
						return
					}
				}
			}
		}
	}()

	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			for i := 0; i < perWriter; i++ {
				// Writers on distinct branches never conflict with each
				// other and never block readers.
				branch := fmt.Sprintf("load-%d", w)
				if i == 0 {
					if err := db.CreateBranch(branch, DefaultBranch, testMeta()); err != nil {
						errCh <- err
						return
					}
				}
				tx, err := db.Begin(branch, TxMeta{Author: "bench", Summary: "load write", Reason: "concurrency test"})
				if err != nil {
					errCh <- err
					return
				}
				if _, err := tx.Create("Users", ids["team101"], []string{"U", "u@dev.io"}); err != nil {
					errCh <- err
					return
				}
				if _, err := tx.Commit(); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}

	// Concurrent writes on the shared main branch as well: conflicting
	// ones must fail cleanly with ErrConflict, never corrupt state.
	writersWG.Add(1)
	go func() {
		defer writersWG.Done()
		for i := 0; i < perWriter; i++ {
			tx, err := db.Begin(DefaultBranch, TxMeta{Author: "bench", Summary: "main write", Reason: "concurrency test"})
			if err != nil {
				errCh <- err
				return
			}
			if _, err := tx.Create("Users", ids["team101"], []string{"M", "m@dev.io"}); err != nil {
				errCh <- err
				return
			}
			if _, err := tx.Commit(); err != nil && !errors.Is(err, ErrConflict) {
				errCh <- err
				return
			}
		}
	}()

	writersWG.Wait()
	close(stop)
	readersWG.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}

	snap, _ := db.Snapshot(DefaultBranch)
	if snap.Count(2) < 3 {
		t.Errorf("main branch lost records: %d", snap.Count(2))
	}
}

func TestBranchingDivergesWithoutMerge(t *testing.T) {
	db := openTestDB(t)
	ids := seedTree(t, db)

	if err := db.CreateBranch("feature", DefaultBranch, TxMeta{Author: "Dev", Summary: "start feature", Reason: "isolate work"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateBranch("feature", DefaultBranch, testMeta()); !errors.Is(err, ErrBranchTaken) {
		t.Fatalf("duplicate branch err = %v, want ErrBranchTaken", err)
	}

	// Write on the feature branch only.
	tx, _ := db.Begin("feature", testMeta())
	feat, err := tx.Create("Teams", ids["acc1"], []string{"Feature Team"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// main must be unaffected (divergence, no merge).
	main, _ := db.Snapshot(DefaultBranch)
	if main.Count(1) != 3 {
		t.Errorf("main teams = %d, want 3", main.Count(1))
	}
	featSnap, _ := db.Snapshot("feature")
	if featSnap.Count(1) != 4 {
		t.Errorf("feature teams = %d, want 4", featSnap.Count(1))
	}

	// A write on main must not leak into feature either.
	tx, _ = db.Begin(DefaultBranch, testMeta())
	if _, err := tx.Create("Accounts", 0, []string{"Wayne Inc"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	featSnap, _ = db.Snapshot("feature")
	if featSnap.Count(0) != 2 {
		t.Errorf("feature accounts = %d, want 2 (branch diverged)", featSnap.Count(0))
	}

	// Branch listing shows both heads and the fork point.
	var info BranchInfo
	for _, b := range db.Branches() {
		if b.Name == "feature" {
			info = b
		}
	}
	if info.Name != "feature" {
		t.Fatal("feature branch missing from listing")
	}
	src, _ := info.ForkSource()
	if src != DefaultBranch {
		t.Errorf("fork source = %q, want %q", src, DefaultBranch)
	}

	// Commits are per-branch.
	for _, e := range db.Commits("feature") {
		if e.Commit != nil && e.Commit.Branch != "feature" {
			t.Errorf("foreign commit %d in feature history", e.Commit.ID)
		}
	}
	if r, ok := featSnap.Record(1, feat.ID); !ok || r.Attrs[0] != "Feature Team" {
		t.Errorf("feature record = %+v ok=%v", r, ok)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	path := t.TempDir() + "/persist.bsl"
	db, err := Init(path, []Layer{
		{Height: 0, Name: "Accounts"},
		{Height: 1, Name: "Teams"},
		{Height: 2, Name: "Users"},
	}, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	ids := seedTree(t, db)
	tx, _ := db.Begin(DefaultBranch, testMeta())
	if _, err := tx.Delete("Teams", ids["team101"]); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateBranch("dev", DefaultBranch, testMeta()); err != nil {
		t.Fatal(err)
	}
	tx, _ = db.Begin("dev", testMeta())
	if _, err := tx.Create("Accounts", 0, []string{"Branch Co"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Close (checkpoint) and reopen: state must survive.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("Open after checkpoint: %v", err)
	}
	defer db2.Close()
	main2, _ := db2.Snapshot(DefaultBranch)
	if main2.Count(0) != 2 || main2.Count(1) != 2 || main2.Count(2) != 1 {
		t.Errorf("main counts after reopen = %d/%d/%d, want 2/2/1",
			main2.Count(0), main2.Count(1), main2.Count(2))
	}
	dev2, _ := db2.Snapshot("dev")
	if dev2.Count(0) != 3 {
		t.Errorf("dev accounts after reopen = %d, want 3", dev2.Count(0))
	}

	// Now write without checkpointing (journal-only) and recover again.
	tx, _ = db2.Begin(DefaultBranch, testMeta())
	if _, err := tx.Create("Users", ids["team103"], []string{"Sam", "sam@dev.io"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db2.f.Close(); err != nil { // simulate a crash: skip checkpoint
		t.Fatal(err)
	}

	db3, err := Open(path)
	if err != nil {
		t.Fatalf("Open after crash: %v", err)
	}
	defer db3.Close()
	main3, _ := db3.Snapshot(DefaultBranch)
	if main3.Count(2) != 2 {
		t.Errorf("users after journal replay = %d, want 2", main3.Count(2))
	}
	found := false
	for _, r := range main3.Records(2) {
		if len(r.Attrs) > 0 && r.Attrs[0] == "Sam" {
			found = true
		}
	}
	if !found {
		t.Error("journal-only commit lost in crash recovery")
	}
}

func TestCommitLogRegionCarriesAuditMetadata(t *testing.T) {
	path := t.TempDir() + "/audit.bsl"
	db, err := Init(path, []Layer{{Height: 0, Name: "Accounts"}}, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := db.Begin(DefaultBranch, TxMeta{Author: "Elena <elena@dev.io>", Summary: "add account", Reason: "customer onboarding"})
	if _, err := tx.Create("Accounts", 0, []string{"Globex"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	hdr := db.Header()
	if hdr.CommitLogEnd <= hdr.CommitLogStart {
		t.Fatalf("empty commit log region: %d-%d", hdr.CommitLogStart, hdr.CommitLogEnd)
	}
	entries := db.Commits("")
	if len(entries) != 2 {
		t.Fatalf("audit trail has %d entries, want 2", len(entries))
	}
	text := ""
	for _, e := range entries {
		author, summary, reason := e.Meta()
		if author == "" || summary == "" || reason == "" {
			t.Errorf("entry missing mandatory metadata: %+v", e)
		}
		if e.Commit != nil {
			text += e.Commit.Text()
		}
	}
	if !strings.Contains(text, "Author: Elena <elena@dev.io>") {
		t.Errorf("commit log text missing author:\n%s", text)
	}
}

func TestRollbackLeavesNoTrace(t *testing.T) {
	db := openTestDB(t)
	tx, _ := db.Begin(DefaultBranch, testMeta())
	if _, err := tx.Create("Accounts", 0, []string{"Ghost"}); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	snap, _ := db.Snapshot(DefaultBranch)
	if snap.Count(0) != 0 {
		t.Errorf("rollback leaked records: %d", snap.Count(0))
	}
	if _, err := tx.Commit(); !errors.Is(err, ErrTxDone) {
		t.Errorf("commit after rollback err = %v, want ErrTxDone", err)
	}
}
