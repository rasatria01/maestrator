// theorm — multi-agent orchestrator. Design: docs/theorm-v2-design.md
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rasatria01/theorm/internal/agent"
	"github.com/rasatria01/theorm/internal/inference"
	"github.com/rasatria01/theorm/internal/memory"
	"github.com/rasatria01/theorm/internal/prompt"
	"github.com/rasatria01/theorm/internal/spec"
	"github.com/rasatria01/theorm/internal/store"
	"github.com/rasatria01/theorm/internal/tools"
)

var version = "0.0.0-dev"

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "version":
		fmt.Println("theorm", version)
	case "doctor":
		os.Exit(doctor())
	case "compile":
		os.Exit(compile(os.Args[2:]))
	case "debug-prompt":
		os.Exit(debugPrompt(os.Args[2:]))
	case "run-task":
		os.Exit(runTask(os.Args[2:]))
	case "index":
		os.Exit(index(os.Args[2:]))
	default:
		fmt.Fprintln(os.Stderr, "usage: theorm <compile|run-task|index|debug-prompt|doctor|version>")
		os.Exit(2)
	}
}

// runTask executes one task's agent loop. The scheduler that picks tasks and
// orders waves arrives in Phase C; this runs the one you name.
func runTask(args []string) int {
	fs := flag.NewFlagSet("run-task", flag.ExitOnError)
	run := fs.String("run", "", "run id")
	task := fs.String("task", "", "task key, e.g. T1")
	dir := fs.String("dir", ".", "working tree the agent may read and write")
	fs.Parse(args)
	if *run == "" || *task == "" {
		fmt.Fprintln(os.Stderr, "usage: theorm run-task --run <run_id> --task T1 [--dir .]")
		return 2
	}
	ctx := context.Background()
	db, err := store.Open(ctx, store.DSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		return 1
	}
	defer db.Close()

	r := &agent.Runner{
		Store: db, Tools: tools.New(tools.Native()...),
		Model: inference.New(), Dir: *dir,
	}
	out, err := r.RunTask(ctx, *run, *task)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run-task: %v\n", err)
		return 1
	}
	fmt.Printf("%s %s in %d steps\n", *task, map[bool]string{true: "completed", false: "stopped"}[out.Completed], out.Steps)
	if out.Completed {
		fmt.Println(out.Summary)
		return 0
	}
	fmt.Fprintln(os.Stderr, out.Reason)
	return 1
}

// index runs the §5.9 cold-start pass: an entity record per source file, so a
// fresh repo's first runs are not blind. Deterministic, no model, CPU embeddings.
func index(args []string) int {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	repo := fs.String("repo", "", "repo id for L3 (default: go.mod module path, else dir name)")
	fs.Parse(args)
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "index: %v\n", err)
		return 1
	}
	repoID := *repo
	if repoID == "" {
		repoID = repoIDFor(abs)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := store.Open(ctx, store.DSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		return 1
	}
	defer db.Close()

	ix := &memory.Indexer{Store: db, Embed: memory.NewEmbedder()}
	n, err := ix.Index(ctx, repoID, abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "index: %v\n", err)
		return 1
	}
	fmt.Printf("indexed %d files into %s\n", n, repoID)
	counts, _ := db.LTMCounts(ctx, repoID)
	for _, t := range []string{"entity", "semantic", "procedural", "episodic"} {
		if counts[t] > 0 {
			fmt.Printf("  %-11s %d\n", t, counts[t])
		}
	}
	return 0
}

// repoIDFor keys L3 by the same identity a spec uses (§A2): the module path when
// there is a go.mod, otherwise the directory name.
func repoIDFor(dir string) string {
	if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
		for ln := range strings.SplitSeq(string(b), "\n") {
			if m, ok := strings.CutPrefix(strings.TrimSpace(ln), "module "); ok {
				return strings.TrimSpace(m)
			}
		}
	}
	return filepath.Base(dir)
}

// debugPrompt prints the exact prompt a task would receive, with the per-section
// token table. Diagnosing a bad attempt should not require reading a log file.
func debugPrompt(args []string) int {
	fs := flag.NewFlagSet("debug-prompt", flag.ExitOnError)
	run := fs.String("run", "", "run id")
	task := fs.String("task", "", "task key, e.g. T4")
	quiet := fs.Bool("table-only", false, "print the token table without the prompt")
	fs.Parse(args)
	if *run == "" || *task == "" {
		fmt.Fprintln(os.Stderr, "usage: theorm debug-prompt --run <run_id> --task T4 [--table-only]")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := store.Open(ctx, store.DSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		return 1
	}
	defer db.Close()

	p, err := prompt.Assemble(ctx, db, *run, *task, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "assemble: %v\n", err)
		return 1
	}
	if !*quiet {
		fmt.Printf("=== system ===\n%s\n\n=== user ===\n%s\n", p.System, p.User)
	}
	fmt.Printf("%-14s %8s %8s %8s   %s\n", "section", "tokens", "budget", "dropped", "overflow policy")
	for _, s := range p.Sections {
		dropped := ""
		if s.Dropped > 0 {
			dropped = fmt.Sprint(s.Dropped)
		}
		fmt.Printf("%-14s %8d %8d %8s   %s\n", s.Name, s.Tokens, s.Budget, dropped, s.Overflow)
	}
	fmt.Printf("%-14s %8d %8d\n", "TOTAL", p.Total, p.Window-4000-2500)
	fmt.Printf("%-14s %8d          (reserved for output, never allocated to input)\n", "output", 4000)
	return 0
}

// compile validates a spec and emits a run of pending tasks (§4.4 step 10).
// Zero model calls.
func compile(args []string) int {
	fs := flag.NewFlagSet("compile", flag.ExitOnError)
	explain := fs.Bool("explain", false, "print the projected execution report")
	check := fs.Bool("check", false, "validate only, do not create a run")
	acceptDrift := fs.Bool("accept-drift", false, "proceed despite conflicting drift (§4.5)")
	repo := fs.String("repo", ".", "repository root, for drift detection")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: theorm compile [--explain] [--check] [--accept-drift] [--repo dir] <spec.md>")
		return 2
	}
	path := fs.Arg(0)

	s, diags := spec.ParseFile(path)
	if s != nil {
		diags = append(diags, spec.Validate(s)...)
	}
	if len(diags) > 0 {
		// Fail loudly and completely: a half-executed plan is worse than none.
		for _, d := range diags {
			fmt.Fprintf(os.Stderr, "%s:%d: %s\n", path, d.Line, d.Msg)
		}
		fmt.Fprintf(os.Stderr, "\n%d problem(s), no run created\n", len(diags))
		return 1
	}
	drift := spec.CheckDrift(*repo, s)
	if *explain {
		fmt.Print(spec.Explain(s, drift))
	} else {
		fmt.Printf("ok  %s: %d tasks, %d knowledge records, drift %s\n",
			path, len(s.Tasks), len(s.Knowledge), drift.Verdict)
	}
	switch drift.Verdict {
	case "unresolvable":
		fmt.Fprintln(os.Stderr, "\nbase does not resolve, re-anchor the spec (§4.5)")
		return 1
	case "conflicting":
		if !*acceptDrift {
			fmt.Fprintln(os.Stderr, "\nconflicting drift, re-run with --accept-drift to proceed (§4.5)")
			return 1
		}
	}
	if *check {
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := store.Open(ctx, store.DSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "\ndatabase: %v\n", err)
		return 1
	}
	defer db.Close()

	runID, err := db.CreateRun(ctx, s, "compiled", drift.Head)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\ncreate run: %v\n", err)
		return 1
	}
	fmt.Printf("\n  Run with: theorm run --compiled %s\n", runID)
	return 0
}

// doctor reports whether the three out-of-process dependencies are reachable.
// ponytail: reachability only. Slot count, context per slot and VRAM (C1) land
// with the llama-server backend in internal/inference.
func doctor() int {
	checks := []struct {
		name, addr string
		probe      func(string) error
	}{
		{"postgres", store.DSN(), pingDB},
		{"llama-server", env("THEORM_LLAMA_URL", "http://127.0.0.1:8081") + "/v1/models", probeHTTP},
		{"embed sidecar", env("THEORM_EMBED_URL", "http://127.0.0.1:8090") + "/health", probeHTTP},
	}
	bad := 0
	for _, c := range checks {
		mark := "ok"
		if err := c.probe(c.addr); err != nil {
			mark, bad = "FAIL: "+err.Error(), bad+1
		}
		fmt.Printf("%-14s %-46s %s\n", c.name, c.addr, mark)
	}
	return bad
}

func pingDB(dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	db, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	db.Close()
	return nil
}

func probeHTTP(addr string) error {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(addr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
