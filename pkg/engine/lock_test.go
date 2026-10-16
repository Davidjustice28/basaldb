package engine

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestLockTimeout(t *testing.T) {
	db := openTestDB(t)
	path := db.Path()
	_, err := OpenTimeout(path, 40*time.Millisecond)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second open err = %v, want ErrLocked", err)
	}
	db.Close()
	again, err := OpenTimeout(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}

func TestFileLockParallelWriters(t *testing.T) {
	if os.Getenv("BASAL_LOCK_CHILD") == "1" {
		path := os.Getenv("BASAL_LOCK_FILE")
		name := os.Getenv("BASAL_LOCK_NAME")
		db, err := Open(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		tx, err := db.Begin(DefaultBranch, TxMeta{Author: "c", Summary: "add", Reason: "lock"})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			db.Close()
			os.Exit(1)
		}
		if _, err := tx.Create("Accounts", 0, []string{name}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			db.Close()
			os.Exit(1)
		}
		if _, err := tx.Commit(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			db.Close()
			os.Exit(1)
		}
		if err := db.Close(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	db := openTestDB(t)
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	const n = 8
	cmds := make([]*exec.Cmd, n)
	for i := 0; i < n; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=TestFileLockParallelWriters", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"BASAL_LOCK_CHILD=1",
			"BASAL_LOCK_FILE="+path,
			"BASAL_LOCK_NAME="+strconv.Itoa(i),
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = cmd
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	got, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	snap, err := got.Snapshot(DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	recs := snap.Records(0)
	if len(recs) != n {
		t.Fatalf("records = %d, want %d", len(recs), n)
	}
}
