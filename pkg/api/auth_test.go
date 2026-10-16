package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"basaldb/pkg/engine"
)

func TestAuth(t *testing.T) {
	db := testDB(t)
	srv := NewServer(db)
	srv.SetAuth("secret", "reader")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	health, err := http.Get(ts.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health without token = %d, want 200", health.StatusCode)
	}

	noAuth, err := http.Get(ts.URL + "/v1/query?q=SHOW%20LAYERS")
	if err != nil {
		t.Fatal(err)
	}
	noAuth.Body.Close()
	if noAuth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query without token = %d, want 401", noAuth.StatusCode)
	}

	bad := getAuth(t, ts, "/v1/query?q=SHOW%20LAYERS", "nope")
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query with wrong token = %d, want 401", bad.StatusCode)
	}

	ok := getAuth(t, ts, "/v1/query?q=SHOW%20LAYERS", "secret")
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("query with token = %d, want 200", ok.StatusCode)
	}

	readQuery := getAuth(t, ts, "/v1/query?q=SHOW%20LAYERS", "reader")
	readQuery.Body.Close()
	if readQuery.StatusCode != http.StatusOK {
		t.Fatalf("read token query = %d, want 200", readQuery.StatusCode)
	}

	readWrite := postAuth(t, ts, "/v1/execute", "reader", map[string]string{
		"statement": `CREATE Accounts [0="nope"]`,
	})
	readWrite.Body.Close()
	if readWrite.StatusCode != http.StatusForbidden {
		t.Fatalf("read token execute = %d, want 403", readWrite.StatusCode)
	}

	write := postAuth(t, ts, "/v1/execute", "secret", map[string]string{
		"statement": `CREATE Accounts [0="ok"]`,
	})
	write.Body.Close()
	if write.StatusCode != http.StatusOK {
		t.Fatalf("write with token = %d, want 200", write.StatusCode)
	}
}

func TestConcurrentExecuteNoConflict(t *testing.T) {
	db := testDB(t)
	ts := httptest.NewServer(NewServer(db).Handler())
	t.Cleanup(ts.Close)

	const writers = 8
	const each = 25
	var wg sync.WaitGroup
	var failures atomic.Int32
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				body, _ := json.Marshal(map[string]string{
					"statement": `CREATE Accounts [0="row"]`,
				})
				resp, err := http.Post(ts.URL+"/v1/execute", "application/json", bytes.NewReader(body))
				if err != nil {
					failures.Add(1)
					return
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					failures.Add(1)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d writer requests failed", failures.Load())
	}
	snap, err := db.Snapshot(engine.DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	if n := snap.Count(0); n != writers*each {
		t.Fatalf("records = %d, want %d", n, writers*each)
	}
}

func TestValidateListen(t *testing.T) {
	if err := ValidateListen("127.0.0.1:8080", "", false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateListen("localhost:8080", "", false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateListen("[::1]:8080", "", false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateListen("0.0.0.0:8080", "", false); err == nil {
		t.Fatal("public bind without a token was allowed")
	}
	if err := ValidateListen(":8080", "", false); err == nil {
		t.Fatal("all-interfaces bind without a token was allowed")
	}
	if err := ValidateListen("0.0.0.0:8080", "secret", false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateListen("0.0.0.0:8080", "", true); err != nil {
		t.Fatal(err)
	}
}

func testDB(t *testing.T) *engine.DB {
	t.Helper()
	db, err := engine.Init(t.TempDir()+"/api.bsl", []engine.Layer{
		{Height: 0, Name: "Accounts"},
	}, engine.TxMeta{Author: "Test", Summary: "init", Reason: "api tests"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func getAuth(t *testing.T, ts *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func postAuth(t *testing.T, ts *httptest.Server, path, token string, body any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
