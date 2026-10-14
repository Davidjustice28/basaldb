// Command basal is the BasalDB CLI utility: initialize containers, inspect
// their ASCII header / commit log, run BasalQL statements, and manage
// branches.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"basaldb/pkg/bql"
	"basaldb/pkg/config"
	"basaldb/pkg/engine"
)

const usage = `basal - BasalDB single-file database utility

Usage:
  basal init    [-file DATA.bsl] [-layers 0:Accounts,1:Teams,2:Users] [-author A -summary S -reason R]
  basal inspect [-file DATA.bsl] [-branch NAME] [-header] [-json]
  basal query   [-file DATA.bsl] [-branch NAME] [-author A -summary S -reason R] 'BQL STATEMENT'
  basal branch  list [-file DATA.bsl]
  basal branch  create NAME [-file DATA.bsl] [-from SRC] [-author A -summary S -reason R]
  basal config  list | get KEY | set KEY VALUE | unset KEY

Defaults (flags always win):
  -file   from 'basal config set file',   else ./basal.bsl
  -branch from 'basal config set branch', else main
  Keys: file, branch. Config path: $BASAL_CONFIG or the user config
  directory ('basal config list' prints it).

BQL examples:
  GET Accounts[id=0] -> Teams -> Users
  GET Users[Name,Age][Age=(gt=21)][limit=5,sort=desc]
  CREATE Users PARENT Teams[id=101] ("Sam", "sam@dev.io")
  DELETE Accounts[id=1]
  SHOW COMMITS
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "inspect":
		err = cmdInspect(os.Args[2:])
	case "query":
		err = cmdQuery(os.Args[2:])
	case "branch":
		err = cmdBranch(os.Args[2:])
	case "config":
		err = cmdConfig(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "basal: %v\n", err)
		os.Exit(1)
	}
}

// splitArgs separates flag arguments from positionals even when positionals
// come first (Go's flag package stops parsing at the first non-flag token).
// All flags take a value except those listed in boolFlags.
func splitArgs(args []string, boolFlags map[string]bool) (flagArgs, posArgs []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flagArgs = append(flagArgs, a)
			name := strings.TrimLeft(a, "-")
			if boolFlags[name] || strings.Contains(a, "=") {
				continue
			}
			if i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		posArgs = append(posArgs, a)
	}
	return flagArgs, posArgs
}

func parseLayersFlag(value string) ([]engine.Layer, error) {
	return engine.ParseLayers(value)
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	file := fs.String("file", "", "path of the .bsl container to create (default: 'basal config set file', else ./basal.bsl)")
	layers := fs.String("layers", "Accounts,Teams,Users", "layer definitions; optional attribute names enable symbolic BQL (\"0:Accounts(name,date),1:Teams(name)\")")
	author := fs.String("author", "basal <basal@localhost>", "Author of the initialization commit")
	summary := fs.String("summary", "Database initialized", "Summary of the initialization commit")
	reason := fs.String("reason", "manual initialization", "Reason of the initialization commit")
	_ = fs.Parse(args)
	path, err := config.ResolveFile(*file)
	if err != nil {
		return err
	}
	ls, err := parseLayersFlag(*layers)
	if err != nil {
		return err
	}
	meta := engine.TxMeta{Author: *author, Summary: *summary, Reason: *reason}
	db, err := engine.Init(path, ls, meta)
	if err != nil {
		return err
	}
	defer db.Close()
	fmt.Printf("initialized %s\nlayers: %s\ninitial commit: %d (%s / %s / %s)\n",
		path, *layers, 1, meta.Author, meta.Summary, meta.Reason)
	return nil
}

func cmdInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	file := fs.String("file", "", "path of the .bsl container (default: 'basal config set file', else ./basal.bsl)")
	branch := fs.String("branch", "", "branch to inspect (default: 'basal config set branch', else main)")
	header := fs.Bool("header", false, "print the raw ASCII header block")
	asJSON := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)
	path, err := config.ResolveFile(*file)
	if err != nil {
		return err
	}
	branchName, err := config.ResolveBranch(*branch)
	if err != nil {
		return err
	}
	db, err := engine.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()

	if *header {
		block, err := db.Header().Marshal()
		if err != nil {
			return err
		}
		fmt.Println(strings.TrimRight(string(block), "\n"))
		return nil
	}
	snap, err := db.Snapshot(branchName)
	if err != nil {
		return err
	}
	if *asJSON {
		out := map[string]any{
			"path":     db.Path(),
			"version":  db.Header().Version,
			"format":   db.Header().Format,
			"branch":   snap.Branch,
			"commit":   snap.Commit,
			"branches": db.Branches(),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	hdr := db.Header()
	fmt.Printf("file:    %s\nversion: %s\nformat:  %s\n", db.Path(), hdr.Version, hdr.Format)
	fmt.Printf("regions: commit log [%d,%d), data [%d,%d), journal [%d,...)\n",
		hdr.CommitLogStart, hdr.CommitLogEnd, hdr.DataStart, hdr.DataEnd, hdr.JournalStart)
	fmt.Printf("branch:  %s @ commit %d\n", snap.Branch, snap.Commit)
	fmt.Println("layers:")
	for _, l := range snap.Layers() {
		fmt.Printf("  %d:%-12s %d record(s)\n", l.Height, l.Name, snap.Count(l.Height))
	}
	fmt.Println("branches:")
	for _, b := range db.Branches() {
		fmt.Printf("  %s\n", b)
	}
	fmt.Println("commits:")
	for _, e := range db.Commits(branchName) {
		author, summary, reason := e.Meta()
		if e.Commit != nil {
			fmt.Printf("  [%04d] %s | %s | %s | %s\n", e.Commit.ID, e.Commit.Branch, author, summary, reason)
		} else {
			fmt.Printf("  [fork ] %s <- %s | %s | %s | %s\n", e.Branch.Name, e.Branch.Fork, author, summary, reason)
		}
	}
	return nil
}

// cmdConfig reads and writes the per-user defaults (like gcloud config):
// the container and branch later commands use when -file/-branch are
// left off.
func cmdConfig(args []string) error {
	action := "list"
	if len(args) > 0 {
		action, args = args[0], args[1:]
	}
	switch action {
	case "list":
		if len(args) > 0 {
			return fmt.Errorf("config: unexpected argument %q", args[0])
		}
		path, err := config.Path()
		if err != nil {
			return err
		}
		c, err := config.Load()
		if err != nil {
			return err
		}
		fmt.Printf("config: %s\nfile:   %s\nbranch: %s\n", path, orUnset(c.File), orUnset(c.Branch))
		return nil
	case "get":
		if len(args) != 1 {
			return fmt.Errorf("config: usage: basal config get KEY")
		}
		value, err := config.Get(args[0])
		if err != nil {
			return err
		}
		fmt.Println(value)
		return nil
	case "set":
		if len(args) != 2 {
			return fmt.Errorf("config: usage: basal config set KEY VALUE")
		}
		if err := config.Set(args[0], args[1]); err != nil {
			return err
		}
		fmt.Printf("%s = %s\n", strings.ToLower(args[0]), args[1])
		return nil
	case "unset":
		if len(args) != 1 {
			return fmt.Errorf("config: usage: basal config unset KEY")
		}
		if err := config.Unset(args[0]); err != nil {
			return err
		}
		fmt.Printf("%s unset\n", strings.ToLower(args[0]))
		return nil
	default:
		return fmt.Errorf("config: unknown action %q (want list, get, set or unset)", action)
	}
}

func orUnset(v string) string {
	if v == "" {
		return "(unset)"
	}
	return v
}

func cmdQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	file := fs.String("file", "", "path of the .bsl container (default: 'basal config set file', else ./basal.bsl)")
	branch := fs.String("branch", "", "branch to run against (default: 'basal config set branch', else main)")
	author := fs.String("author", "", "stamps the commit block (required for BRANCH CREATE)")
	summary := fs.String("summary", "", "stamps the commit block (required for BRANCH CREATE)")
	reason := fs.String("reason", "", "stamps the commit block (required for BRANCH CREATE)")
	asJSON := fs.Bool("json", false, "print JSON")
	flagArgs, posArgs := splitArgs(args, map[string]bool{"json": true})
	_ = fs.Parse(flagArgs)
	if len(posArgs) != 1 {
		return fmt.Errorf("query: exactly one BQL statement required")
	}
	path, err := config.ResolveFile(*file)
	if err != nil {
		return err
	}
	branchName, err := config.ResolveBranch(*branch)
	if err != nil {
		return err
	}
	db, err := engine.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := &bql.ExecContext{
		DB:     db,
		Branch: branchName,
		Meta:   engine.TxMeta{Author: *author, Summary: *summary, Reason: *reason},
	}
	res, err := bql.Exec(ctx, posArgs[0])
	if err != nil {
		return err
	}
	return printResult(res, *asJSON)
}

func cmdBranch(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("branch: expected list or create")
	}
	sub := args[0]
	switch sub {
	case "list":
		fs := flag.NewFlagSet("branch list", flag.ExitOnError)
		file := fs.String("file", "", "path of the .bsl container (default: 'basal config set file', else ./basal.bsl)")
		flagArgs, posArgs := splitArgs(args[1:], nil)
		if len(posArgs) > 0 {
			return fmt.Errorf("branch list: unexpected argument %q", posArgs[0])
		}
		_ = fs.Parse(flagArgs)
		path, err := config.ResolveFile(*file)
		if err != nil {
			return err
		}
		db, err := engine.Open(path)
		if err != nil {
			return err
		}
		defer db.Close()
		for _, b := range db.Branches() {
			fmt.Println(b)
		}
		return nil
	case "create":
		fs := flag.NewFlagSet("branch create", flag.ExitOnError)
		file := fs.String("file", "", "path of the .bsl container (default: 'basal config set file', else ./basal.bsl)")
		from := fs.String("from", engine.DefaultBranch, "source branch to fork at its head")
		author := fs.String("author", "basal <basal@localhost>", "Author of the branch event")
		summary := fs.String("summary", "Branch created", "Summary of the branch event")
		reason := fs.String("reason", "feature isolation", "Reason of the branch event")
		flagArgs, posArgs := splitArgs(args[1:], nil)
		_ = fs.Parse(flagArgs)
		if len(posArgs) != 1 {
			return fmt.Errorf("branch create: exactly one branch name required")
		}
		path, err := config.ResolveFile(*file)
		if err != nil {
			return err
		}
		db, err := engine.Open(path)
		if err != nil {
			return err
		}
		defer db.Close()
		meta := engine.TxMeta{Author: *author, Summary: *summary, Reason: *reason}
		if err := db.CreateBranch(posArgs[0], *from, meta); err != nil {
			return err
		}
		fmt.Printf("created branch %q from %q\n", posArgs[0], *from)
		return nil
	default:
		return fmt.Errorf("branch: unknown subcommand %q", sub)
	}
}

func printResult(res *bql.Result, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	if len(res.Rows) > 0 {
		fmt.Println(strings.Join(res.Columns, " | "))
		for _, r := range res.Rows {
			if len(r.Cells) > 0 {
				fmt.Println(strings.Join(r.Cells, " | "))
				continue
			}
			kids := make([]string, len(r.Children))
			for i, c := range r.Children {
				kids[i] = strconv.FormatUint(c, 10)
			}
			attrs := make([]string, len(r.Attrs))
			for i, a := range r.Attrs {
				attrs[i] = engine.Quote(a)
			}
			fmt.Printf("%s[%d#%d] parent=%d children=[%s] %s\n",
				r.Layer, r.ID, r.Slot, r.Parent, strings.Join(kids, ","), strings.Join(attrs, " "))
		}
	}
	if res.Message != "" {
		fmt.Println(res.Message)
	}
	if res.Commit != nil {
		fmt.Printf("commit %d on %s | %s | %s | %s\n",
			res.Commit.ID, res.Commit.Branch, res.Commit.Author, res.Commit.Summary, res.Commit.Reason)
	}
	return nil
}
