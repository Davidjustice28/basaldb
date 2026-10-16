// Command basal-bench builds a synthetic Accounts -> Teams -> Users
// hierarchy and measures load rate, file size, memory, open time, BQL
// read/write/delete latencies, parallel read throughput and branch
// storage cost. See BENCHMARKS.md for baseline numbers.
//
//	go run ./cmd/basal-bench -accounts 1000 -teams 10 -users 20
//	go run ./cmd/basal-bench -accounts 5000 -teams 10 -users 20 -index email
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"basaldb/pkg/bql"
	"basaldb/pkg/engine"
)

var regions = []string{"us-east", "us-west", "eu", "apac"}
var plans = []string{"free", "pro", "enterprise"}
var roles = []string{"admin", "dev", "rep", "viewer"}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func fsize(p string) int64 { st, err := os.Stat(p); must(err); return st.Size() }

func heapMB() float64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / 1e6
}

func pct(d []time.Duration, p float64) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[int(float64(len(s)-1)*p)]
}

func lat(name string, n int, f func(i int)) {
	ds := make([]time.Duration, n)
	t0 := time.Now()
	for i := 0; i < n; i++ {
		s := time.Now()
		f(i)
		ds[i] = time.Since(s)
	}
	tot := time.Since(t0)
	fmt.Printf("  %-46s n=%-5d p50=%-10v p99=%-10v ops/s=%.0f\n", name, n, pct(ds, .5).Round(time.Microsecond), pct(ds, .99).Round(time.Microsecond), float64(n)/tot.Seconds())
}

func main() {
	accts := flag.Int("accounts", 1000, "")
	teams := flag.Int("teams", 10, "")
	users := flag.Int("users", 20, "")
	path := flag.String("file", "bench.bsl", "")
	indexList := flag.String("index", "", "comma-separated attribute names to hash-index, e.g. email")
	generateGB := flag.Float64("generate-gb", 0, "write a flat file of about this many GiB and exit")
	openOnly := flag.Bool("open-only", false, "open -file, print open time and heap, look up one id, exit")
	flag.Parse()
	if *generateGB > 0 {
		target := int64(*generateGB * float64(1<<30))
		t0 := time.Now()
		n, err := engine.GenerateFlat(*path, target)
		must(err)
		fmt.Printf("[gen] %d records, %.2f GB file, in %v\n", n, float64(fsize(*path))/1e9, time.Since(t0).Round(time.Millisecond))
		return
	}
	if *openOnly {
		t0 := time.Now()
		db, err := engine.Open(*path)
		must(err)
		fmt.Printf("[open] cold open: %v\n", time.Since(t0).Round(time.Millisecond))
		fmt.Printf("[mem]  heap after open: %.0f MB\n", heapMB())
		ctx := &bql.ExecContext{DB: db, Branch: "main"}
		s := time.Now()
		if _, err := bql.Exec(ctx, `GET Users[id=1]`); err != nil {
			must(err)
		}
		fmt.Printf("[read] GET Users[id=1]: %v\n", time.Since(s).Round(time.Microsecond))
		must(db.Close())
		return
	}
	if *accts**teams < 300 {
		fmt.Fprintln(os.Stderr, "basal-bench: need accounts*teams >= 300")
		os.Exit(2)
	}
	os.Remove(*path)
	total := *accts + *accts**teams + *accts**teams**users
	indexed := map[string]bool{}
	for _, name := range strings.Split(*indexList, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			indexed[name] = true
		}
	}
	mark := func(name string) string {
		if indexed[strings.ToLower(name)] {
			return name + "!"
		}
		return name
	}
	spec := fmt.Sprintf("0:Accounts(%s,%s,%s),1:Teams(%s),2:Users(%s,%s,%s,%s)",
		mark("name"), mark("plan"), mark("region"), mark("name"),
		mark("name"), mark("email"), mark("role"), mark("age"))
	fmt.Printf("== dataset: %d accounts x %d teams x %d users = %d records\n", *accts, *teams, *users, total)
	fmt.Printf("== schema: %s\n", spec)

	layers, err := engine.ParseLayers(spec)
	must(err)
	meta := engine.TxMeta{Author: "bench", Summary: "init", Reason: "benchmark"}
	db, err := engine.Init(*path, layers, meta)
	must(err)

	// 1. bulk load: one transaction per account subtree
	rng := rand.New(rand.NewSource(1))
	var acctIDs, teamIDs, userIDs []uint64
	t0 := time.Now()
	for a := 0; a < *accts; a++ {
		tx, err := db.Begin("main", engine.TxMeta{})
		must(err)
		ar, err := tx.Create("Accounts", 0, []string{fmt.Sprintf("acct-%d", a), plans[a%3], regions[a%4]})
		must(err)
		acctIDs = append(acctIDs, ar.ID)
		for t := 0; t < *teams; t++ {
			tr, err := tx.Create("Teams", ar.ID, []string{fmt.Sprintf("team-%d-%d", a, t)})
			must(err)
			teamIDs = append(teamIDs, tr.ID)
			for u := 0; u < *users; u++ {
				ur, err := tx.Create("Users", tr.ID, []string{fmt.Sprintf("user-%d-%d-%d", a, t, u), fmt.Sprintf("u%d.%d.%d@ex.io", a, t, u), roles[rng.Intn(4)], fmt.Sprint(18 + rng.Intn(50))})
				must(err)
				userIDs = append(userIDs, ur.ID)
			}
		}
		_, err = tx.Commit()
		must(err)
	}
	load := time.Since(t0)
	fmt.Printf("\n[load] %d records in %v  (%.0f records/s, %d commits)\n", total, load.Round(time.Millisecond), float64(total)/load.Seconds(), *accts)
	fmt.Printf("[load] journal-only file size: %.1f MB\n", float64(fsize(*path))/1e6)
	fmt.Printf("[mem]  heap after load: %.0f MB (%.0f bytes/record)\n", heapMB(), heapMB()*1e6/float64(total))

	t0 = time.Now()
	must(db.Checkpoint())
	fmt.Printf("[ckpt] checkpoint (full rewrite) %v\n", time.Since(t0).Round(time.Millisecond))
	sz := fsize(*path)
	fmt.Printf("[disk] file size: %.1f MB (%.0f bytes/record)\n", float64(sz)/1e6, float64(sz)/float64(total))
	must(db.Close())

	t0 = time.Now()
	db, err = engine.Open(*path)
	must(err)
	fmt.Printf("[open] cold open (load whole file): %v\n", time.Since(t0).Round(time.Millisecond))
	fmt.Printf("[mem]  heap after open: %.0f MB\n", heapMB())

	ctx := &bql.ExecContext{DB: db, Branch: "main"}
	q := func(s string) *bql.Result { r, err := bql.Exec(ctx, s); must(err); return r }

	fmt.Println("\n[reads] via BQL, in-process")
	lat("GET Accounts[id=N]", 200, func(i int) { q(fmt.Sprintf("GET Accounts[id=%d]", acctIDs[rng.Intn(len(acctIDs))])) })
	lat("GET Users[id=N]  (scans whole layer)", 50, func(i int) { q(fmt.Sprintf("GET Users[id=%d]", userIDs[rng.Intn(len(userIDs))])) })
	const oneEmail = `GET Users[email="u5.5.5@ex.io"]`
	if indexed["email"] {
		s := time.Now()
		q(oneEmail)
		fmt.Printf("  GET Users[email=X] first lookup (cold page): %v\n", time.Since(s).Round(time.Microsecond))
	}
	lat("GET Users[email=X] (attr lookup)", 50, func(i int) { q(oneEmail) })
	if indexed["email"] {
		step := len(userIDs) / 50
		if step < 1 {
			step = 1
		}
		span := *teams * *users
		lat("GET Users[email=distinct] (indexed)", 50, func(i int) {
			idx := i * step
			a := idx / span
			rem := idx % span
			q(fmt.Sprintf(`GET Users[email="u%d.%d.%d@ex.io"]`, a, rem / *users, rem%*users))
		})
	}
	var n int
	lat("GET Accounts[id=N] -> Teams -> Users (subtree)", 200, func(i int) {
		n = len(q(fmt.Sprintf("GET Accounts[id=%d] -> Teams -> Users", acctIDs[rng.Intn(len(acctIDs))])).Rows)
	})
	fmt.Printf("    (subtree returns %d rows)\n", n)
	lat("GET Accounts[name=X] -> Teams -> Users", 50, func(i int) { q(fmt.Sprintf(`GET Accounts[name="acct-%d"] -> Teams -> Users`, rng.Intn(*accts))) })
	lat("GET Users[age=(gt=60),role=admin] (filter scan)", 20, func(i int) { n = len(q("GET Users[name][age=(gt=60),role=admin]").Rows) })
	fmt.Printf("    (filter returns %d rows)\n", n)
	lat("GET Users[*][limit=10,sort=desc]", 20, func(i int) { q("GET Users[*][limit=10,sort=desc]") })
	lat("SHOW LAYERS", 200, func(i int) { q("SHOW LAYERS") })

	// parallel reads
	workers := runtime.NumCPU()
	var wg sync.WaitGroup
	per := 400
	t0 = time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(w)))
			c := &bql.ExecContext{DB: db, Branch: "main"}
			for i := 0; i < per; i++ {
				_, err := bql.Exec(c, fmt.Sprintf("GET Accounts[id=%d] -> Teams -> Users", acctIDs[r.Intn(len(acctIDs))]))
				must(err)
			}
		}(w)
	}
	wg.Wait()
	el := time.Since(t0)
	fmt.Printf("  parallel subtree reads, %d goroutines: %.0f queries/s (vs serial above)\n", workers, float64(workers*per)/el.Seconds())

	fmt.Println("\n[writes] via BQL, one commit each, in-process (journal append, no fsync)")
	before := fsize(*path)
	lat("CREATE Users PARENT Teams[id=N]", 500, func(i int) {
		q(fmt.Sprintf(`CREATE Users PARENT Teams[id=%d] [name="new%d",email="n%d@ex.io",role="dev",age="30"]`, teamIDs[rng.Intn(len(teamIDs))], i, i))
	})
	lat("UPDATE Users[id=N][role=X]", 200, func(i int) { q(fmt.Sprintf(`UPDATE Users[id=%d][role="admin"]`, userIDs[rng.Intn(len(userIDs))])) })
	fmt.Printf("  journal growth for 700 writes: %.1f KB (%.0f bytes/write)\n", float64(fsize(*path)-before)/1e3, float64(fsize(*path)-before)/700)

	ctx2 := &bql.ExecContext{DB: db, Branch: "main"}
	t0 = time.Now()
	r, err := bql.Exec(ctx2, fmt.Sprintf("DELETE Accounts[id=%d]", acctIDs[0]))
	must(err)
	fmt.Printf("  DELETE one account (cascade): %v  -> %s\n", time.Since(t0).Round(time.Microsecond), r.Message)
	lat("DELETE Teams[id=N] (cascade ~users)", 50, func(i int) { bql.Exec(ctx2, fmt.Sprintf("DELETE Teams[id=%d]", teamIDs[len(teamIDs)-1-i])) })

	db.SetSync(true)
	lat("CREATE with fsync on", 100, func(i int) {
		q(fmt.Sprintf(`CREATE Users PARENT Teams[id=%d] [name="s%d"]`, teamIDs[len(teamIDs)/4+i], i))
	})
	db.SetSync(false)

	fmt.Println("\n[branches]")
	must(db.Checkpoint())
	base := fsize(*path)
	t0 = time.Now()
	must(db.CreateBranch("scratch", "main", meta))
	fmt.Printf("  branch create: %v\n", time.Since(t0).Round(time.Microsecond))
	c3 := &bql.ExecContext{DB: db, Branch: "scratch"}
	_, err = bql.Exec(c3, fmt.Sprintf(`UPDATE Accounts[id=%d][plan="enterprise"]`, acctIDs[5]))
	must(err)
	fmt.Printf("  heap with 2 branches: %.0f MB\n", heapMB())
	t0 = time.Now()
	must(db.Checkpoint())
	fmt.Printf("  checkpoint %v, file %.1f MB -> %.1f MB after branch + 1 update\n", time.Since(t0).Round(time.Millisecond), float64(base)/1e6, float64(fsize(*path))/1e6)
	must(db.Close())
	fmt.Printf("\n[final] file %s = %.1f MB\n", *path, float64(fsize(*path))/1e6)
}
