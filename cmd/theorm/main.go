// theorm — multi-agent orchestrator. Design: docs/theorm-v2-design.md
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
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
	case "memory":
		os.Exit(memoryCmd(os.Args[2:]))
	case "curate":
		os.Exit(curate(os.Args[2:]))
	case "decay":
		os.Exit(decay(os.Args[2:]))
	case "stale":
		os.Exit(stale(os.Args[2:]))
	case "retire":
		os.Exit(retire(os.Args[2:]))
	default:
		fmt.Fprintln(os.Stderr, "usage: theorm <compile|run-task|index|memory|curate|decay|stale|retire|debug-prompt|doctor|version>")
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
		Retriever: &memory.Retriever{Store: db, Embed: memory.NewEmbedder()},
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

// decay is the §5.8 mechanism 3 nightly job: age unretrieved records and retire
// the ones that fall through the floor. No git, no repo — it runs over all of L3.
func decay(args []string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := store.Open(ctx, store.DSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		return 1
	}
	defer db.Close()
	decayed, retired, err := db.Decay(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "decay: %v\n", err)
		return 1
	}
	fmt.Printf("decayed %d records, retired %d\n", decayed, retired)
	return 0
}

// stale is §5.8 mechanism 4: mark the entity records of the files a run changed
// stale so retrieval stops serving outdated file summaries. --run pulls the repo
// and base commit from the run; --base overrides the diff point.
func stale(args []string) int {
	fs := flag.NewFlagSet("stale", flag.ExitOnError)
	repo := fs.String("repo", ".", "working tree to diff")
	run := fs.String("run", "", "take repo id and base commit from this run")
	base := fs.String("base", "", "diff against this ref instead (default: the run base, else uncommitted changes)")
	fs.Parse(args)
	abs, err := filepath.Abs(*repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stale: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := store.Open(ctx, store.DSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		return 1
	}
	defer db.Close()

	repoID := repoIDFor(abs)
	baseRef := *base
	if *run != "" {
		rid, bc, err := db.RunBase(ctx, *run)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stale: %v\n", err)
			return 1
		}
		repoID = rid
		if baseRef == "" {
			baseRef = bc
		}
	}

	// git already emits forward-slash paths relative to the repo root, which is
	// exactly the "file:<path>" subject the index wrote.
	diffArgs := []string{"diff", "--name-only", "HEAD"}
	if baseRef != "" {
		diffArgs = []string{"diff", "--name-only", baseRef, "HEAD"}
	}
	changed, err := gitLines(abs, diffArgs...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stale: git diff: %v\n", err)
		return 1
	}
	s, w, err := db.MarkStale(ctx, repoID, changed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stale: %v\n", err)
		return 1
	}
	fmt.Printf("%d files changed; marked %d entity records stale, weakened %d procedural records in %s\n",
		len(changed), s, w, repoID)
	return 0
}

func gitLines(dir string, args ...string) ([]string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var lines []string
	for ln := range strings.SplitSeq(string(out), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			lines = append(lines, s)
		}
	}
	return lines, nil
}

// curate runs the §5.8 Curator over a finished run: promote up to 10 claims into
// L3, refresh near-duplicates, park contradictions, and write one episodic
// record. The scheduler (Phase C) will trigger this at end of run; for now it is
// on demand.
func curate(args []string) int {
	fs := flag.NewFlagSet("curate", flag.ExitOnError)
	run := fs.String("run", "", "run id")
	fs.Parse(args)
	if *run == "" {
		fmt.Fprintln(os.Stderr, "usage: theorm curate --run <run_id>")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := store.Open(ctx, store.DSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		return 1
	}
	defer db.Close()

	c := &memory.Curator{Store: db, Embed: memory.NewEmbedder()}
	res, err := c.Curate(ctx, *run)
	if err != nil {
		fmt.Fprintf(os.Stderr, "curate: %v\n", err)
		return 1
	}
	fmt.Printf("promoted %d of %d claims, bumped %d duplicates, parked %d conflicts, skipped %d over cap; 1 episodic\n",
		res.Promoted, res.Claims, res.Bumped, res.Parked, res.Skipped)
	return 0
}

// memoryCmd is the read side of L3: the same hybrid retrieval agents get, so a
// bad memory can be found where it was used (§9).
func memoryCmd(args []string) int {
	if len(args) == 0 || args[0] != "search" {
		fmt.Fprintln(os.Stderr, `usage: theorm memory search [--repo id] [--subjects a,b] [--top n] <query>`)
		return 2
	}
	fs := flag.NewFlagSet("memory search", flag.ExitOnError)
	repo := fs.String("repo", "", "repo id (default: go.mod module path of the cwd)")
	subjects := fs.String("subjects", "", "comma-separated read-set subjects for the exact lane")
	top := fs.Int("top", 6, "results to return after rerank")
	fs.Parse(args[1:])
	q := strings.Join(fs.Args(), " ")
	if q == "" {
		fmt.Fprintln(os.Stderr, "memory search: empty query")
		return 2
	}
	repoID := *repo
	if repoID == "" {
		cwd, _ := filepath.Abs(".")
		repoID = repoIDFor(cwd)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := store.Open(ctx, store.DSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		return 1
	}
	defer db.Close()

	var subs []string
	if *subjects != "" {
		subs = strings.Split(*subjects, ",")
	}
	r := &memory.Retriever{Store: db, Embed: memory.NewEmbedder(), TopK: *top}
	hits, err := r.Search(ctx, repoID, q, subs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memory search: %v\n", err)
		return 1
	}
	if len(hits) == 0 {
		fmt.Printf("no records for %q in %s (run `theorm index` first?)\n", q, repoID)
		return 0
	}
	for i, h := range hits {
		fmt.Printf("%2d. [%.3f] %-10s %s\n     %s\n", i+1, h.Score, h.MemType, h.Subject, trunc(h.Content, 160))
	}
	return 0
}

func trunc(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
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

	// Retrieve L3 best-effort so the printed prompt matches what an agent sees.
	// If the sidecar is down, note it and show the memory section empty rather
	// than failing — this command is for reading prompts, not for gating on deps.
	var mem []string
	if t, err := db.Task(ctx, *run, *task); err == nil {
		ret := &memory.Retriever{Store: db, Embed: memory.NewEmbedder()}
		if hits, err := ret.SearchTask(ctx, t.RepoID, t.RunTitle, t.Title, t.Contract.ReadSet.Subjects); err == nil {
			mem = memory.Contents(hits)
		} else {
			fmt.Fprintf(os.Stderr, "note: L3 retrieval unavailable (%v); memory section shown empty\n", err)
		}
	}

	p, err := prompt.Assemble(ctx, db, *run, *task, nil, mem)
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

	// Embedding the knowledge blocks can take a moment on a cold sidecar, so the
	// ceiling is generous; it returns as soon as the work is done.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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

	// B5: a spec's knowledge outlives its run. Ingestion is best-effort — the run
	// is already committed, and a down sidecar should not fail a compile.
	if len(s.Knowledge) > 0 || len(s.Prose) > 0 {
		ix := &memory.Ingester{Store: db, Embed: memory.NewEmbedder()}
		if n, err := ix.IngestSpec(ctx, s); err != nil {
			fmt.Fprintf(os.Stderr, "note: knowledge not ingested (%v); run `theorm compile` again once the sidecar is up\n", err)
		} else {
			fmt.Printf("  ingested %d knowledge records (provenance spec:%s)\n", n, s.SHA256[:12])
		}
	}
	fmt.Printf("\n  Run with: theorm run --compiled %s\n", runID)
	return 0
}

// retire revokes a spec's knowledge from L3 by its provenance hash. Records are
// retired, not deleted, so the audit trail survives (§5.8).
func retire(args []string) int {
	fs := flag.NewFlagSet("retire", flag.ExitOnError)
	hash := fs.String("hash", "", "spec sha256, when the file is gone")
	fs.Parse(args)
	prov := ""
	switch {
	case *hash != "":
		prov = "spec:" + *hash
	case fs.NArg() == 1:
		raw, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			fmt.Fprintf(os.Stderr, "retire: %v\n", err)
			return 1
		}
		sum := sha256.Sum256(raw)
		prov = "spec:" + hex.EncodeToString(sum[:])
	default:
		fmt.Fprintln(os.Stderr, "usage: theorm retire <spec.md> | theorm retire --hash <sha256>")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := store.Open(ctx, store.DSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		return 1
	}
	defer db.Close()
	n, err := db.RetireSpec(ctx, prov)
	if err != nil {
		fmt.Fprintf(os.Stderr, "retire: %v\n", err)
		return 1
	}
	fmt.Printf("retired %d records with provenance %s\n", n, prov)
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
