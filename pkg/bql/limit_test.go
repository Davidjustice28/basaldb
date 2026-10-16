package bql

import (
	"fmt"
	"testing"
	"time"

	"basaldb/pkg/engine"
)

func TestLimitStopsBeforeTheRestOfTheLayer(t *testing.T) {
	db, err := engine.Init(t.TempDir()+"/limit.bsl", []engine.Layer{
		{Height: 0, Name: "Memories", Attrs: []string{"text", "confidence"}},
	}, engine.TxMeta{Author: "Test", Summary: "init", Reason: "limit scan"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := &ExecContext{DB: db, Branch: engine.DefaultBranch}

	const n = 8000
	tx, err := db.Begin(engine.DefaultBranch, engine.TxMeta{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		conf := "0.1"
		if i >= n-100 {
			conf = "0.9"
		}
		if _, err := tx.Create("Memories", 0, []string{fmt.Sprintf("m%d", i), conf}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	res := mustExec(t, ctx, `GET Memories[text][limit=5]`)
	if len(res.Rows) != 5 || res.Rows[0].Attrs[0] != "m0" || res.Rows[4].Attrs[0] != "m4" {
		t.Fatalf("forward limit = %+v", attrsOf(res))
	}
	res = mustExec(t, ctx, `GET Memories[text][limit=5,sort=desc]`)
	if len(res.Rows) != 5 || res.Rows[0].Attrs[0] != fmt.Sprintf("m%d", n-1) || res.Rows[4].Attrs[0] != fmt.Sprintf("m%d", n-5) {
		t.Fatalf("desc limit = %+v", attrsOf(res))
	}
	res = mustExec(t, ctx, `GET Memories[text][confidence="0.9"][limit=3,sort=desc]`)
	if len(res.Rows) != 3 || res.Rows[0].Attrs[0] != fmt.Sprintf("m%d", n-1) {
		t.Fatalf("filtered desc limit = %+v", attrsOf(res))
	}
	// The oldest high-confidence row is n-100. Ascending stops there.
	res = mustExec(t, ctx, `GET Memories[text][confidence="0.9"][limit=2,sort=asc]`)
	if len(res.Rows) != 2 || res.Rows[0].Attrs[0] != fmt.Sprintf("m%d", n-100) {
		t.Fatalf("filtered asc limit = %+v", attrsOf(res))
	}

	start := time.Now()
	for i := 0; i < 20; i++ {
		mustExec(t, ctx, `GET Memories[text][limit=20]`)
	}
	limited := time.Since(start)
	start = time.Now()
	full := mustExec(t, ctx, `GET Memories[text]`)
	scanned := time.Since(start)
	if len(full.Rows) != n {
		t.Fatalf("full scan rows = %d, want %d", len(full.Rows), n)
	}
	// Twenty early-stop queries should still cost less than one full
	// materialization of the layer. This is the #46 regression.
	if limited > scanned {
		t.Fatalf("20 limited scans took %s, one full scan took %s", limited, scanned)
	}
}

func attrsOf(res *Result) []string {
	out := make([]string, len(res.Rows))
	for i, r := range res.Rows {
		if len(r.Attrs) > 0 {
			out[i] = r.Attrs[0]
		}
	}
	return out
}
