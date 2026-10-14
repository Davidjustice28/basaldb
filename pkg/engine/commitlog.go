package engine

import (
	"fmt"
	"strconv"
	"strings"
)

// Commit is one immutable commit block in the .bsl audit trail. Author,
// Summary and Reason stamp the block when provided; they are mandatory
// only on structural entries (initialization, branch events).
type Commit struct {
	ID        uint64   `json:"id"`
	Branch    string   `json:"branch"`
	Parent    uint64   `json:"parent"` // 0 = root commit
	Author    string   `json:"author"`
	Summary   string   `json:"summary"`
	Reason    string   `json:"reason"`
	Timestamp int64    `json:"timestamp"`
	Ops       []string `json:"ops,omitempty"`
}

// BranchEvent records the creation of a branch: a new named history forking
// the commit chain of another branch at its head (branches diverge; there is
// no merge).
type BranchEvent struct {
	Name      string `json:"name"`
	Fork      string `json:"fork"` // "branch@commit"
	Author    string `json:"author"`
	Summary   string `json:"summary"`
	Reason    string `json:"reason"`
	Timestamp int64  `json:"timestamp"`
}

// LogEntry is one chronological entry of the human-readable commit log.
type LogEntry struct {
	Commit *Commit
	Branch *BranchEvent
}

// Meta returns the audit metadata of the entry.
func (e LogEntry) Meta() (author, summary, reason string) {
	if e.Commit != nil {
		return e.Commit.Author, e.Commit.Summary, e.Commit.Reason
	}
	return e.Branch.Author, e.Branch.Summary, e.Branch.Reason
}

// Validate checks the structural fields of a commit. The audit
// metadata (Author, Summary, Reason) is optional: record writes may
// carry none, and it is enforced only where it is mandatory (container
// initialization, branch creation).
func (c *Commit) Validate() error {
	if strings.TrimSpace(c.Branch) == "" {
		return fmt.Errorf("engine: commit %d requires Branch", c.ID)
	}
	return nil
}

// Validate enforces the mandatory branch event metadata.
func (e *BranchEvent) Validate() error {
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("engine: branch event requires Name")
	}
	if strings.TrimSpace(e.Author) == "" || strings.TrimSpace(e.Summary) == "" || strings.TrimSpace(e.Reason) == "" {
		return fmt.Errorf("engine: branch %q requires Author, Summary and Reason", e.Name)
	}
	return nil
}

// Text renders the commit as a human-readable block, matching the .bsl
// commit log layout (symbol-delimited by "---").
func (c *Commit) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Commit:%04d]\n", c.ID)
	fmt.Fprintf(&b, "Branch: %s\n", c.Branch)
	fmt.Fprintf(&b, "Parent: %d\n", c.Parent)
	fmt.Fprintf(&b, "Author: %s\n", singleLine(c.Author))
	fmt.Fprintf(&b, "Reason: %s\n", singleLine(c.Reason))
	fmt.Fprintf(&b, "Summary: %s\n", singleLine(c.Summary))
	fmt.Fprintf(&b, "Timestamp: %d\n", c.Timestamp)
	if len(c.Ops) > 0 {
		b.WriteString("Ops:\n")
		for _, op := range c.Ops {
			b.WriteString("  " + singleLine(op) + "\n")
		}
	}
	b.WriteString("---\n")
	return b.String()
}

// Text renders the branch event as a human-readable audit block.
func (e *BranchEvent) Text() string {
	var b strings.Builder
	b.WriteString("[Branch]\n")
	fmt.Fprintf(&b, "Name: %s\n", singleLine(e.Name))
	fmt.Fprintf(&b, "Fork: %s\n", singleLine(e.Fork))
	fmt.Fprintf(&b, "Author: %s\n", singleLine(e.Author))
	fmt.Fprintf(&b, "Reason: %s\n", singleLine(e.Reason))
	fmt.Fprintf(&b, "Summary: %s\n", singleLine(e.Summary))
	fmt.Fprintf(&b, "Timestamp: %d\n", e.Timestamp)
	b.WriteString("---\n")
	return b.String()
}

func singleLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

// LogRegionText concatenates entries into the commit log region payload.
func LogRegionText(entries []LogEntry) string {
	var b strings.Builder
	for _, e := range entries {
		if e.Commit != nil {
			b.WriteString(e.Commit.Text())
		} else if e.Branch != nil {
			b.WriteString(e.Branch.Text())
		}
	}
	return b.String()
}

// ParseLogRegion parses the human-readable commit log region into ordered
// entries. Author, Summary, and Reason stamp each entry when provided
// and are mandatory on structural entries (initialization, branches).
func ParseLogRegion(region []byte) ([]LogEntry, error) {
	text := strings.ReplaceAll(string(region), "\r\n", "\n")
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")

	var entries []LogEntry
	var block []string
	inBlock := false
	flush := func() error {
		if !inBlock {
			return nil
		}
		entry, err := parseLogBlock(block)
		if err != nil {
			return err
		}
		entries = append(entries, entry)
		block = nil
		inBlock = false
		return nil
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "---" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		if strings.TrimSpace(line) == "" && !inBlock {
			continue
		}
		inBlock = true
		block = append(block, strings.TrimRight(line, "\r"))
	}
	if inBlock {
		return nil, fmt.Errorf("engine: unterminated commit log block %q", strings.TrimSpace(block[0]))
	}
	return entries, nil
}

func parseLogBlock(block []string) (LogEntry, error) {
	if len(block) == 0 {
		return LogEntry{}, fmt.Errorf("engine: empty commit log block")
	}
	head := strings.TrimSpace(block[0])
	var entry LogEntry
	switch {
	case strings.HasPrefix(head, "[Commit:"):
		idStr := strings.TrimSuffix(strings.TrimPrefix(head, "[Commit:"), "]")
		id, err := strconv.ParseUint(strings.TrimSpace(idStr), 10, 64)
		if err != nil || id == 0 {
			return LogEntry{}, fmt.Errorf("engine: bad commit header %q", head)
		}
		entry.Commit = &Commit{ID: id}
	case head == "[Branch]":
		entry.Branch = &BranchEvent{}
	default:
		return LogEntry{}, fmt.Errorf("engine: unknown commit log block %q", head)
	}

	fields := map[string]string{}
	var ops []string
	inOps := false
	for _, line := range block[1:] {
		if inOps && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) {
			ops = append(ops, strings.TrimSpace(line))
			continue
		}
		inOps = false
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return LogEntry{}, fmt.Errorf("engine: malformed commit log line %q", line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		fields[key] = value
		if key == "Ops" && value == "" {
			inOps = true
		}
	}

	if entry.Commit != nil {
		c := entry.Commit
		c.Branch = fields["Branch"]
		c.Author = fields["Author"]
		c.Summary = fields["Summary"]
		c.Reason = fields["Reason"]
		c.Ops = ops
		if v := fields["Parent"]; v != "" {
			p, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return LogEntry{}, fmt.Errorf("engine: bad Parent %q", v)
			}
			c.Parent = p
		}
		if v := fields["Timestamp"]; v != "" {
			ts, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return LogEntry{}, fmt.Errorf("engine: bad Timestamp %q", v)
			}
			c.Timestamp = ts
		}
		if err := c.Validate(); err != nil {
			return LogEntry{}, err
		}
	} else {
		ev := entry.Branch
		ev.Name = fields["Name"]
		ev.Fork = fields["Fork"]
		ev.Author = fields["Author"]
		ev.Summary = fields["Summary"]
		ev.Reason = fields["Reason"]
		if v := fields["Timestamp"]; v != "" {
			ts, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return LogEntry{}, fmt.Errorf("engine: bad Timestamp %q", v)
			}
			ev.Timestamp = ts
		}
		if err := ev.Validate(); err != nil {
			return LogEntry{}, err
		}
	}
	return entry, nil
}
