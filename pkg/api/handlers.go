// Package api implements the BasalDB REST API used by cmd/basald: BQL
// queries over MVCC snapshots plus branch and commit management.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"basaldb/pkg/bql"
	"basaldb/pkg/engine"
)

// Server exposes a *engine.DB over HTTP/JSON.
type Server struct {
	db *engine.DB
}

// NewServer wraps a database in the REST API.
func NewServer(db *engine.DB) *Server { return &Server{db: db} }

// Handler returns the routed http.Handler for the /v1 API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/meta", s.handleMeta)
	mux.HandleFunc("/v1/query", s.handleQuery)
	mux.HandleFunc("/v1/execute", s.handleExecute)
	mux.HandleFunc("/v1/branches", s.handleBranches)
	mux.HandleFunc("/v1/commits", s.handleCommits)
	return mux
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorBody{Error: err.Error()})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("GET only"))
		return
	}
	snap, err := s.db.Snapshot(branchParam(r))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	type layerInfo struct {
		Height  int    `json:"height"`
		Name    string `json:"name"`
		Records int    `json:"records"`
	}
	meta := map[string]any{
		"version":  engine.CurrentVersion,
		"format":   engine.FormatMatrixSlottedPage,
		"path":     s.db.Path(),
		"branch":   snap.Branch,
		"commit":   snap.Commit,
		"branches": s.db.Branches(),
	}
	layers := []layerInfo{}
	for _, l := range snap.Layers() {
		layers = append(layers, layerInfo{Height: l.Height, Name: l.Name, Records: snap.Count(l.Height)})
	}
	meta["layers"] = layers
	writeJSON(w, http.StatusOK, meta)
}

// handleQuery executes read-only BQL (GET / SHOW) against an immutable
// snapshot; concurrent writes never block or corrupt a running query.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("GET only"))
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		writeErr(w, http.StatusBadRequest, errors.New("missing query parameter q"))
		return
	}
	stmt, err := bql.Parse(q)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	switch stmt.(type) {
	case *bql.GetStmt, *bql.ShowStmt:
	default:
		writeErr(w, http.StatusBadRequest, errors.New("/v1/query is read-only; use /v1/execute for writes"))
		return
	}
	res, err := bql.ExecStmt(s.execContext(r, engine.TxMeta{}), stmt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type executeBody struct {
	Statement string `json:"statement"`
	Branch    string `json:"branch"`
	Author    string `json:"author"`
	Summary   string `json:"summary"`
	Reason    string `json:"reason"`
}

// handleExecute runs any BQL statement. Writes require the mandatory audit
// metadata (author, summary, reason) and append an immutable commit block.
func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("POST only"))
		return
	}
	var body executeBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(body.Statement) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("missing statement"))
		return
	}
	meta := engine.TxMeta{Author: body.Author, Summary: body.Summary, Reason: body.Reason}
	stmt, err := bql.Parse(body.Statement)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := bql.ExecStmt(s.execContext(r, meta), stmt)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	if res.UseBranch != "" {
		w.Header().Set("X-Basal-Branch", res.UseBranch)
	}
	writeJSON(w, http.StatusOK, res)
}

type branchBody struct {
	Name     string `json:"name"`
	ForkFrom string `json:"fork_from"`
	Author   string `json:"author"`
	Summary  string `json:"summary"`
	Reason   string `json:"reason"`
}

func (s *Server) handleBranches(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.db.Branches())
	case http.MethodPost:
		var body branchBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		meta := engine.TxMeta{Author: body.Author, Summary: body.Summary, Reason: body.Reason}
		if err := s.db.CreateBranch(body.Name, body.ForkFrom, meta); err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusCreated, s.db.Branches())
	default:
		writeErr(w, http.StatusMethodNotAllowed, errors.New("GET or POST only"))
	}
}

func (s *Server) handleCommits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("GET only"))
		return
	}
	type commitJSON struct {
		Kind      string   `json:"kind"`
		ID        uint64   `json:"id,omitempty"`
		Branch    string   `json:"branch,omitempty"`
		Parent    uint64   `json:"parent,omitempty"`
		Name      string   `json:"name,omitempty"`
		Fork      string   `json:"fork,omitempty"`
		Author    string   `json:"author"`
		Summary   string   `json:"summary"`
		Reason    string   `json:"reason"`
		Timestamp int64    `json:"timestamp"`
		Ops       []string `json:"ops,omitempty"`
	}
	out := []commitJSON{}
	for _, e := range s.db.Commits(branchParam(r)) {
		if e.Commit != nil {
			c := e.Commit
			out = append(out, commitJSON{
				Kind: "commit", ID: c.ID, Branch: c.Branch, Parent: c.Parent,
				Author: c.Author, Summary: c.Summary, Reason: c.Reason,
				Timestamp: c.Timestamp, Ops: c.Ops,
			})
		} else if e.Branch != nil {
			ev := e.Branch
			out = append(out, commitJSON{
				Kind: "branch", Name: ev.Name, Fork: ev.Fork,
				Author: ev.Author, Summary: ev.Summary, Reason: ev.Reason,
				Timestamp: ev.Timestamp,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) execContext(r *http.Request, meta engine.TxMeta) *bql.ExecContext {
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = r.Header.Get("X-Basal-Branch")
	}
	if branch == "" {
		branch = engine.DefaultBranch
	}
	return &bql.ExecContext{DB: s.db, Branch: branch, Meta: meta}
}

func branchParam(r *http.Request) string {
	if b := r.URL.Query().Get("branch"); b != "" {
		return b
	}
	return engine.DefaultBranch
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, engine.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, engine.ErrBranchTaken):
		return http.StatusConflict
	case errors.Is(err, engine.ErrTxDone):
		return http.StatusConflict
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unknown branch"),
		strings.Contains(msg, "not found"),
		strings.Contains(msg, "unknown layer"):
		return http.StatusNotFound
	case strings.Contains(msg, "required"),
		strings.Contains(msg, "bql:"):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// FormatUint is a small helper for CLI/API numeric fields.
func FormatUint(n uint64) string { return strconv.FormatUint(n, 10) }
