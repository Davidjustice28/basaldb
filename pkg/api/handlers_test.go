package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"basaldb/pkg/engine"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	db, err := engine.Init(t.TempDir()+"/api.bsl", []engine.Layer{
		{Height: 0, Name: "Accounts"},
		{Height: 1, Name: "Teams"},
		{Height: 2, Name: "Users"},
	}, engine.TxMeta{Author: "Test", Summary: "init", Reason: "api tests"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	srv := httptest.NewServer(NewServer(db).Handler())
	t.Cleanup(func() {
		srv.Close()
		db.Close()
	})
	return srv
}

func post(t *testing.T, srv *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestQueryAndExecute(t *testing.T) {
	srv := newTestServer(t)

	// Write through /v1/execute with mandatory audit metadata.
	resp := post(t, srv, "/v1/execute", map[string]string{
		"statement": `CREATE Accounts [0="Acme Corp"]`,
		"author":    "Alex Rivera <alex@dev.io>",
		"summary":   "seed account",
		"reason":    "api test",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("execute status = %d", resp.StatusCode)
	}
	var res struct {
		Commit *struct {
			ID     uint64 `json:"id"`
			Author string `json:"author"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if res.Commit == nil || res.Commit.Author != "Alex Rivera <alex@dev.io>" {
		t.Fatalf("commit block = %+v", res.Commit)
	}

	// Record writes are metadata-free; the commit block carries none.
	resp = post(t, srv, "/v1/execute", map[string]string{"statement": `CREATE Accounts [0="X"]`})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metadata-less write status = %d, want 200", resp.StatusCode)
	}
	var bare struct {
		Commit *struct {
			Author string `json:"author"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&bare); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if bare.Commit == nil || bare.Commit.Author != "" {
		t.Fatalf("commit block = %+v, want an empty-author commit", bare.Commit)
	}

	// Read-only query endpoint.
	resp, err := http.Get(srv.URL + "/v1/query?q=" + urlEscape(`GET Accounts[*]`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query status = %d", resp.StatusCode)
	}
	var qres struct {
		Rows []struct {
			Attrs []string `json:"attrs"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&qres); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(qres.Rows) != 2 || qres.Rows[0].Attrs[0] != "Acme Corp" {
		t.Fatalf("query rows = %+v", qres.Rows)
	}

	// /v1/query rejects writes.
	resp, err = http.Get(srv.URL + "/v1/query?q=" + urlEscape(`DELETE Accounts[id=1]`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("write via query status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBranchesEndpoint(t *testing.T) {
	srv := newTestServer(t)

	resp := post(t, srv, "/v1/branches", map[string]string{
		"name":      "feature",
		"fork_from": "main",
		"author":    "Dev",
		"summary":   "start feature",
		"reason":    "isolation",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create branch status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = post(t, srv, "/v1/branches", map[string]string{
		"name": "feature", "author": "Dev", "summary": "dup", "reason": "dup",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate branch status = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err := http.Get(srv.URL + "/v1/branches")
	if err != nil {
		t.Fatal(err)
	}
	var branches []engine.BranchInfo
	if err := json.NewDecoder(resp.Body).Decode(&branches); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(branches) != 2 {
		t.Fatalf("branches = %+v", branches)
	}

	// Writes on the feature branch must not leak into main.
	resp = post(t, srv, "/v1/execute?branch=feature", map[string]string{
		"statement": `CREATE Accounts [0="Feature Co"]`,
		"author":    "Dev", "summary": "add", "reason": "test",
	})
	resp.Body.Close()
	resp, err = http.Get(srv.URL + "/v1/query?branch=main&q=" + urlEscape(`SHOW RECORDS Accounts`))
	if err != nil {
		t.Fatal(err)
	}
	var mainRows struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&mainRows); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(mainRows.Rows) != 0 {
		t.Fatalf("main sees feature records: %+v", mainRows.Rows)
	}
}

func TestCommitsEndpointCarriesAuditMetadata(t *testing.T) {
	srv := newTestServer(t)
	resp := post(t, srv, "/v1/execute", map[string]string{
		"statement": `CREATE Accounts [0="Audited"]`,
		"author":    "Elena <elena@dev.io>",
		"summary":   "add audited account",
		"reason":    "compliance",
	})
	resp.Body.Close()

	resp, err := http.Get(srv.URL + "/v1/commits")
	if err != nil {
		t.Fatal(err)
	}
	var commits []struct {
		Kind    string `json:"kind"`
		Author  string `json:"author"`
		Summary string `json:"summary"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&commits); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(commits) != 2 {
		t.Fatalf("commits = %+v", commits)
	}
	for _, c := range commits {
		if c.Author == "" || c.Summary == "" || c.Reason == "" {
			t.Errorf("commit missing mandatory metadata: %+v", c)
		}
	}
}

func urlEscape(q string) string {
	r := strings.NewReplacer(" ", "%20", "[", "%5B", "]", "%5D", `"`, "%22", "(", "%28", ")", "%29", "*", "%2A", "#", "%23", ",", "%2C")
	return r.Replace(q)
}
