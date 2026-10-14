package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// BranchInfo describes one named branch: its own commit chain diverging from
// a fork point. Branches never merge back.
type BranchInfo struct {
	Name string `json:"name"`
	Fork string `json:"fork"` // "source@commit" ("main@0" for the default branch)
	Head uint64 `json:"head"` // current head commit id
}

// validateBranchName rejects names that would corrupt the ASCII header or
// commit log layout.
func validateBranchName(name string) error {
	if name == "" {
		return fmt.Errorf("engine: branch name is required")
	}
	for _, r := range name {
		if unicode.IsSpace(r) || r == ':' || r == '[' || r == ']' || r == '@' || r == ',' {
			return fmt.Errorf("engine: invalid branch name %q", name)
		}
	}
	return nil
}

// CreateBranch forks a new named branch at the current head of forkFrom
// (default: main). The new branch carries its own commit history from that
// point on; writes on either branch diverge independently (no merge).
func (db *DB) CreateBranch(name, forkFrom string, meta TxMeta) error {
	if err := validateBranchName(name); err != nil {
		return err
	}
	if err := meta.Validate(); err != nil {
		return err
	}
	if forkFrom == "" {
		forkFrom = DefaultBranch
	}
	if name == forkFrom {
		return fmt.Errorf("engine: branch %q already exists", name)
	}

	db.wmu.Lock()
	defer db.wmu.Unlock()

	db.mu.RLock()
	closed := db.closed
	db.mu.RUnlock()
	if closed {
		return fmt.Errorf("engine: database is closed")
	}

	st := db.state.Load()
	if _, taken := st.branches[name]; taken {
		return fmt.Errorf("%w: %q", ErrBranchTaken, name)
	}
	src := st.branches[forkFrom]
	if src == nil {
		return fmt.Errorf("engine: unknown branch %q", forkFrom)
	}

	ev := &BranchEvent{
		Name:      name,
		Fork:      fmt.Sprintf("%s@%d", forkFrom, src.Commit),
		Author:    meta.Author,
		Summary:   meta.Summary,
		Reason:    meta.Reason,
		Timestamp: time.Now().Unix(),
	}
	if err := ev.Validate(); err != nil {
		return err
	}
	if err := db.appendJournal(marshalJournalBranch(ev)); err != nil {
		return err
	}

	// The new branch starts from the same immutable snapshot; the first
	// write on either branch clones only the pages it touches.
	snap := &Snapshot{
		Branch:     name,
		Commit:     src.Commit,
		matrixView: matrixView{hdr: db.hdr, layers: src.layers},
	}
	next := cloneRefs(db.state.Load().branches)
	next[name] = snap
	db.state.Store(&dbState{branches: next})

	db.mu.Lock()
	db.forks[name] = ev.Fork
	db.events = append(db.events, LogEntry{Branch: ev})
	db.mu.Unlock()
	return nil
}

// Branches lists all branches with their fork points and head commits.
func (db *DB) Branches() []BranchInfo {
	st := db.state.Load()
	db.mu.RLock()
	forks := make(map[string]string, len(db.forks))
	for k, v := range db.forks {
		forks[k] = v
	}
	db.mu.RUnlock()

	out := make([]BranchInfo, 0, len(st.branches))
	for name, snap := range st.branches {
		fork := forks[name]
		if fork == "" {
			fork = fmt.Sprintf("%s@0", DefaultBranch)
		}
		out = append(out, BranchInfo{Name: name, Fork: fork, Head: snap.Commit})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Commits returns the audit trail (newest first). An empty branch returns
// every commit and branch event; a branch name filters its own chain.
func (db *DB) Commits(branch string) []LogEntry {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out []LogEntry
	for i := len(db.events) - 1; i >= 0; i-- {
		e := db.events[i]
		if branch != "" {
			if e.Commit != nil && e.Commit.Branch != branch {
				continue
			}
			if e.Branch != nil && e.Branch.Name != branch {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// Commit returns a single commit block by id.
func (db *DB) Commit(id uint64) (*Commit, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	c, ok := db.commits[id]
	return c, ok
}

// String renders a branch info line for the CLI.
func (b BranchInfo) String() string {
	return fmt.Sprintf("%s (fork %s, head %d)", b.Name, b.Fork, b.Head)
}

// ForkSource splits the fork reference into source branch and commit.
func (b BranchInfo) ForkSource() (string, uint64) {
	name, ref := splitFork(b.Fork)
	if name == "" {
		name = DefaultBranch
	}
	return name, ref
}

// Quote is a tiny helper used by CLI output for attribute lists.
func Quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
