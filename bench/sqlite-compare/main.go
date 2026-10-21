// Command sqlite-compare runs the basal-bench workload against SQLite so
// the two can be compared on the same machine. Same dataset, same random
// seed, same queries and sample counts as cmd/basal-bench.
//
// SQLite gets its best normal setup: WAL, foreign keys with indexes on
// every parent column, and statements prepared once. Writes without fsync
// use synchronous=OFF (basal-bench appends to the journal without fsync);
// the fsync row uses synchronous=FULL with fullfsync, which is what Go's
// File.Sync does on macOS.
//
//	go run . -accounts 1000 -teams 10 -users 20 -file /tmp/bench.sqlite
//	go run . -accounts 5000 -teams 10 -users 20 -index email
//
// It is its own module so the main module stays dependency-free. It needs
// cgo (github.com/mattn/go-sqlite3 builds the SQLite amalgamation).
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var nilCtx = context.Background()

var regions = []string{"us-east", "us-west", "eu", "apac"}
var plans = []string{"free", "pro", "enterprise"}
var roles = []string{"admin", "dev", "rep", "viewer"}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func fsize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

// dbSize is the main file plus WAL, which is what a copy would need.
func dbSize(p string) int64 { return fsize(p) + fsize(p+"-wal") }

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

func open(path string) *sql.DB {
	db, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_foreign_keys=on&_synchronous=OFF&cache=private")
	must(err)
	db.SetMaxOpenConns(runtime.NumCPU())
	db.SetMaxIdleConns(runtime.NumCPU())
	return db
}

// rows drains a query into strings, as BQL materializes result rows.
func rows(st *sql.Stmt, args ...any) int {
	r, err := st.Query(args...)
	must(err)
	defer r.Close()
	cols, _ := r.Columns()
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n := 0
	for r.Next() {
		must(r.Scan(ptrs...))
		n++
	}
	must(r.Err())
	return n
}

func exec(st *sql.Stmt, args ...any) {
	_, err := st.Exec(args...)
	must(err)
}

func prep(db *sql.DB, q string) *sql.Stmt {
	st, err := db.Prepare(q)
	must(err)
	return st
}

func copyFile(dst, src string) {
	in, err := os.Open(src)
	must(err)
	defer in.Close()
	out, err := os.Create(dst)
	must(err)
	_, err = io.Copy(out, in)
	must(err)
	must(out.Close())
}

func main() {
	accts := flag.Int("accounts", 1000, "")
	teams := flag.Int("teams", 10, "")
	users := flag.Int("users", 20, "")
	path := flag.String("file", "bench.sqlite", "")
	index := flag.String("index", "", "set to email to add a unique index on users.email")
	flag.Parse()
	for _, p := range []string{*path, *path + "-wal", *path + "-shm", *path + ".branch"} {
		os.Remove(p)
	}
	total := *accts + *accts**teams + *accts**teams**users
	fmt.Printf("== dataset: %d accounts x %d teams x %d users = %d records\n", *accts, *teams, *users, total)
	fmt.Println("== sqlite: WAL, foreign keys + parent indexes, prepared statements, synchronous=OFF")

	db := open(*path)
	_, err := db.Exec(`
CREATE TABLE accounts(id INTEGER PRIMARY KEY, name TEXT, plan TEXT, region TEXT);
CREATE TABLE teams(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE, name TEXT);
CREATE TABLE users(id INTEGER PRIMARY KEY, team_id INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE, name TEXT, email TEXT, role TEXT, age INTEGER);
CREATE INDEX teams_account ON teams(account_id);
CREATE INDEX users_team ON users(team_id);`)
	must(err)
	if *index == "email" {
		_, err = db.Exec(`CREATE UNIQUE INDEX users_email ON users(email)`)
		must(err)
	}

	// 1. bulk load: one transaction per account subtree
	rng := rand.New(rand.NewSource(1))
	var acctIDs, teamIDs, userIDs []int64
	t0 := time.Now()
	for a := 0; a < *accts; a++ {
		tx, err := db.Begin()
		must(err)
		ia, _ := tx.Prepare(`INSERT INTO accounts(name,plan,region) VALUES(?,?,?)`)
		it, _ := tx.Prepare(`INSERT INTO teams(account_id,name) VALUES(?,?)`)
		iu, _ := tx.Prepare(`INSERT INTO users(team_id,name,email,role,age) VALUES(?,?,?,?,?)`)
		r, err := ia.Exec(fmt.Sprintf("acct-%d", a), plans[a%3], regions[a%4])
		must(err)
		aid, _ := r.LastInsertId()
		acctIDs = append(acctIDs, aid)
		for t := 0; t < *teams; t++ {
			r, err := it.Exec(aid, fmt.Sprintf("team-%d-%d", a, t))
			must(err)
			tid, _ := r.LastInsertId()
			teamIDs = append(teamIDs, tid)
			for u := 0; u < *users; u++ {
				r, err := iu.Exec(tid, fmt.Sprintf("user-%d-%d-%d", a, t, u), fmt.Sprintf("u%d.%d.%d@ex.io", a, t, u), roles[rng.Intn(4)], 18+rng.Intn(50))
				must(err)
				uid, _ := r.LastInsertId()
				userIDs = append(userIDs, uid)
			}
		}
		ia.Close()
		it.Close()
		iu.Close()
		must(tx.Commit())
	}
	load := time.Since(t0)
	fmt.Printf("\n[load] %d records in %v  (%.0f records/s, %d commits)\n", total, load.Round(time.Millisecond), float64(total)/load.Seconds(), *accts)

	// Planner statistics, as any real deployment would have. Without them
	// SQLite scans users for the by-name join (about 60 ms at 1.05M).
	t0 = time.Now()
	_, err = db.Exec(`ANALYZE`)
	must(err)
	fmt.Printf("[stat] ANALYZE %v\n", time.Since(t0).Round(time.Millisecond))

	t0 = time.Now()
	_, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	must(err)
	fmt.Printf("[ckpt] wal checkpoint %v\n", time.Since(t0).Round(time.Millisecond))
	sz := dbSize(*path)
	fmt.Printf("[disk] file size: %.1f MB (%.0f bytes/record)\n", float64(sz)/1e6, float64(sz)/float64(total))
	must(db.Close())

	// Cold open: SQLite opens lazily, so time open plus the first read.
	t0 = time.Now()
	db = open(*path)
	var one int
	must(db.QueryRow(`SELECT count(*) FROM accounts WHERE id=1`).Scan(&one))
	fmt.Printf("[open] cold open + first read: %v\n", time.Since(t0).Round(time.Millisecond))

	getAcct := prep(db, `SELECT id,name,plan,region FROM accounts WHERE id=?`)
	getUser := prep(db, `SELECT id,team_id,name,email,role,age FROM users WHERE id=?`)
	byEmail := prep(db, `SELECT id,team_id,name,email,role,age FROM users WHERE email=?`)
	subtree := prep(db, `SELECT u.id,u.team_id,u.name,u.email,u.role,u.age FROM teams t JOIN users u ON u.team_id=t.id WHERE t.account_id=?`)
	byName := prep(db, `SELECT u.id,u.team_id,u.name,u.email,u.role,u.age FROM accounts a JOIN teams t ON t.account_id=a.id JOIN users u ON u.team_id=t.id WHERE a.name=?`)
	filter := prep(db, `SELECT name FROM users WHERE age>60 AND role='admin'`)
	last10 := prep(db, `SELECT id,team_id,name,email,role,age FROM users ORDER BY id DESC LIMIT 10`)

	fmt.Println("\n[reads] via SQL, in-process, prepared")
	lat("GET Accounts[id=N]", 200, func(i int) { rows(getAcct, acctIDs[rng.Intn(len(acctIDs))]) })
	lat("GET Users[id=N]", 50, func(i int) { rows(getUser, userIDs[rng.Intn(len(userIDs))]) })
	lat("GET Users[email=X] (attr lookup)", 50, func(i int) { rows(byEmail, "u5.5.5@ex.io") })
	var n int
	lat("GET Accounts[id=N] -> Teams -> Users (subtree)", 200, func(i int) { n = rows(subtree, acctIDs[rng.Intn(len(acctIDs))]) })
	fmt.Printf("    (subtree returns %d rows)\n", n)
	lat("GET Accounts[name=X] -> Teams -> Users", 50, func(i int) { rows(byName, fmt.Sprintf("acct-%d", rng.Intn(*accts))) })
	lat("GET Users[age=(gt=60),role=admin] (filter scan)", 20, func(i int) { n = rows(filter) })
	fmt.Printf("    (filter returns %d rows)\n", n)
	lat("GET Users[*][limit=10,sort=desc]", 20, func(i int) { rows(last10) })

	// parallel reads: 12 goroutines, WAL readers on a connection pool
	workers := runtime.NumCPU()
	var wg sync.WaitGroup
	per := 400
	t0 = time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(w)))
			for i := 0; i < per; i++ {
				rows(subtree, acctIDs[r.Intn(len(acctIDs))])
			}
		}(w)
	}
	wg.Wait()
	el := time.Since(t0)
	fmt.Printf("  parallel subtree reads, %d goroutines: %.0f queries/s (vs serial above)\n", workers, float64(workers*per)/el.Seconds())

	fmt.Println("\n[writes] via SQL, one commit each, in-process (synchronous=OFF)")
	ins := prep(db, `INSERT INTO users(team_id,name,email,role,age) VALUES(?,?,?,?,?)`)
	upd := prep(db, `UPDATE users SET role='admin' WHERE id=?`)
	before := dbSize(*path)
	lat("CREATE Users PARENT Teams[id=N]", 500, func(i int) {
		exec(ins, teamIDs[rng.Intn(len(teamIDs))], fmt.Sprintf("new%d", i), fmt.Sprintf("n%d@ex.io", i), "dev", 30)
	})
	lat("UPDATE Users[id=N][role=X]", 200, func(i int) { exec(upd, userIDs[rng.Intn(len(userIDs))]) })
	fmt.Printf("  wal growth for 700 writes: %.1f KB (%.0f bytes/write)\n", float64(dbSize(*path)-before)/1e3, float64(dbSize(*path)-before)/700)

	delAcct := prep(db, `DELETE FROM accounts WHERE id=?`)
	delTeam := prep(db, `DELETE FROM teams WHERE id=?`)
	t0 = time.Now()
	exec(delAcct, acctIDs[0])
	fmt.Printf("  DELETE one account (cascade): %v\n", time.Since(t0).Round(time.Microsecond))
	lat("DELETE Teams[id=N] (cascade ~users)", 50, func(i int) { exec(delTeam, teamIDs[len(teamIDs)-1-i]) })

	// fsync on: a dedicated connection so the pragmas apply to it.
	conn, err := db.Conn(nilCtx)
	must(err)
	for _, p := range []string{`PRAGMA synchronous=FULL`, `PRAGMA fullfsync=ON`, `PRAGMA checkpoint_fullfsync=ON`} {
		_, err = conn.ExecContext(nilCtx, p)
		must(err)
	}
	lat("CREATE with fsync on", 100, func(i int) {
		_, err := conn.ExecContext(nilCtx, `INSERT INTO users(team_id,name) VALUES(?,?)`, teamIDs[len(teamIDs)/4+i], fmt.Sprintf("s%d", i))
		must(err)
	})
	must(conn.Close())

	// SQLite has no branches. The closest equivalent is a copy of the file.
	fmt.Println("\n[branches] (SQLite: copy of the database)")
	_, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	must(err)
	base := dbSize(*path)
	t0 = time.Now()
	copyFile(*path+".branch", *path)
	fmt.Printf("  branch create (file copy): %v\n", time.Since(t0).Round(time.Microsecond))
	b := open(*path + ".branch")
	_, err = b.Exec(`UPDATE accounts SET plan='enterprise' WHERE id=?`, acctIDs[5])
	must(err)
	_, err = b.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	must(err)
	fmt.Printf("  storage %.1f MB -> %.1f MB after branch + 1 update\n", float64(base)/1e6, float64(base+dbSize(*path+".branch"))/1e6)
	must(b.Close())
	t0 = time.Now()
	_, err = db.Exec(fmt.Sprintf(`VACUUM INTO '%s.vac'`, *path))
	must(err)
	fmt.Printf("  VACUUM INTO (consistent online copy): %v\n", time.Since(t0).Round(time.Millisecond))
	os.Remove(*path + ".vac")
	must(db.Close())
	fmt.Printf("\n[final] file %s = %.1f MB\n", *path, float64(dbSize(*path))/1e6)
}
