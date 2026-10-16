// Package api implements the BasalDB REST API used by cmd/basald: BQL
// queries over MVCC snapshots plus branch and commit management.
package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"basaldb/pkg/bql"
	"basaldb/pkg/engine"
)

// maxRequestBytes caps a POST body so one client cannot force the process
// to buffer an unbounded payload. 32 MiB still fits a large attribute.
const maxRequestBytes = 32 << 20

// Server exposes a *engine.DB over HTTP/JSON.
type Server struct {
	db        *engine.DB
	token     string
	readToken string
	locks     sync.Map // branch name -> *sync.Mutex
}

// NewServer wraps a database in the REST API.
func NewServer(db *engine.DB) *Server { return &Server{db: db} }

// SetAuth requires a bearer token on every route except /v1/health.
// readToken, when set, may call the read-only routes and receives 403
// on writes. Empty token and readToken leave the API open, which is
// safe only on a loopback listen; basald refuses a public bind without
// a token unless -insecure-no-auth is set.
func (s *Server) SetAuth(token, readToken string) {
	s.token = token
	s.readToken = readToken
}

// Handler returns the routed http.Handler for the /v1 API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/meta", s.handleMeta)
	mux.HandleFunc("/v1/query", s.handleQuery)
	mux.HandleFunc("/v1/execute", s.handleExecute)
	mux.HandleFunc("/v1/branches", s.handleBranches)
	mux.HandleFunc("/v1/commits", s.handleCommits)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorize(r); err != nil {
			status := http.StatusUnauthorized
			if errors.Is(err, errReadOnly) {
				status = http.StatusForbidden
			}
			writeErr(w, status, err)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

var errReadOnly = errors.New("read-only token")

func (s *Server) authorize(r *http.Request) error {
	if r.URL.Path == "/v1/health" || (s.token == "" && s.readToken == "") {
		return nil
	}
	got := bearerToken(r)
	if s.token != "" && tokenMatch(got, s.token) {
		return nil
	}
	if s.readToken != "" && tokenMatch(got, s.readToken) {
		if readOnlyRequest(r) {
			return nil
		}
		return errReadOnly
	}
	return errors.New("unauthorized")
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// tokenMatch compares tokens in constant time. Hashing both sides first
// keeps the compare independent of the secret's length.
func tokenMatch(got, want string) bool {
	if want == "" {
		return false
	}
	g := sha256.Sum256([]byte(got))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

func readOnlyRequest(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	switch r.URL.Path {
	case "/v1/query", "/v1/meta", "/v1/branches", "/v1/commits", "/v1/health":
		return true
	}
	return false
}

// lockBranch serializes writers on one branch for the duration of a
// single statement. basald is the process that owns the file, so it can
// queue commits instead of returning 409 when two requests overlap
// (issue #47). Different branches do not block each other.
func (s *Server) lockBranch(branch string) func() {
	if branch == "" {
		branch = engine.DefaultBranch
	}
	v, _ := s.locks.LoadOrStore(branch, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
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

// handleExecute runs any BQL statement. Record writes are metadata-free
// (whatever the body carries stamps the commit block); BRANCH CREATE
// requires the audit metadata.
func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("POST only"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	var body executeBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeRequestErr(w, err)
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
	ctx := s.execContext(r, meta)
	if body.Branch != "" && r.URL.Query().Get("branch") == "" && r.Header.Get("X-Basal-Branch") == "" {
		ctx.Branch = body.Branch
	}
	unlock := s.lockBranch(ctx.Branch)
	defer unlock()
	res, err := bql.ExecStmt(ctx, stmt)
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
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		var body branchBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeRequestErr(w, err)
			return
		}
		meta := engine.TxMeta{Author: body.Author, Summary: body.Summary, Reason: body.Reason}
		unlock := s.lockBranch(body.Name)
		defer unlock()
		err := s.db.CreateBranch(body.Name, body.ForkFrom, meta)
		if err != nil {
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

func writeRequestErr(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeErr(w, http.StatusRequestEntityTooLarge, errors.New("request body too large"))
		return
	}
	writeErr(w, http.StatusBadRequest, err)
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
