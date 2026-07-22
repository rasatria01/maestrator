// theorm — multi-agent orchestrator. Design: docs/theorm-v2-design.md
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/rasatria01/theorm/internal/spec"
	"github.com/rasatria01/theorm/internal/store"
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
	default:
		fmt.Fprintln(os.Stderr, "usage: theorm <compile|doctor|version>")
		os.Exit(2)
	}
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
		{"llama-server", env("THEORM_LLAMA_URL", "http://127.0.0.1:8081") + "/health", probeHTTP},
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
