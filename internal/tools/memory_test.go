package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rasatria01/theorm/internal/spec"
	"github.com/rasatria01/theorm/internal/store"
)

// Needs the compose database: docker compose up -d db
func fixture(t *testing.T) (*store.Store, string, context.Context) {
	t.Helper()
	t.Setenv("THEORM_ARTIFACTS", t.TempDir())
	ctx := context.Background()
	s, err := store.Open(ctx, store.DSN())
	if err != nil {
		t.Skipf("no database at %s: %v", store.DSN(), err)
	}
	t.Cleanup(s.Close)

	sp := &spec.Spec{
		Path: "test", SHA256: "test", Meta: spec.Meta{Repo: "github.com/x/y", Title: "a1 fixture"},
		Tasks: []spec.Task{
			{ID: "T1", Title: "coder", Role: "coder", ResourceClass: "gpu_deep"},
			{ID: "T2", Title: "reviewer", Role: "reviewer", ResourceClass: "gpu_deep", DependsOn: []string{"T1"}},
			{ID: "T3", Title: "sibling", Role: "coder", ResourceClass: "gpu_deep"},
		},
	}
	runID, err := s.CreateRun(ctx, sp, "compiled", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Pool().Exec(ctx, `DELETE FROM runs WHERE id=$1`, runID) })
	return s, runID, ctx
}

func env(t *testing.T, s *store.Store, ctx context.Context, runID, key, role string, allowed ...string) Env {
	t.Helper()
	taskID, err := s.TaskID(ctx, runID, key)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := s.StartAttempt(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	return Env{
		Store: s, RunID: runID, TaskID: taskID, AttemptID: attemptID, Role: role,
		Allowed: allowed,
		ReadSet: store.ClaimQuery{
			Kinds:    []string{"constraint", "decision", "finding", "failure", "handoff", "fact"},
			Subjects: []string{"file:internal/auth/"},
		},
	}
}

func call(r *Registry, ctx context.Context, e Env, name string, args any) Result {
	b, _ := json.Marshal(args)
	return r.Execute(ctx, e, name, b)
}

// The A1 definition of done: a coder writes a finding, and a reviewer in the
// same run reads it back.
func TestCoderWritesReviewerReads(t *testing.T) {
	s, runID, ctx := fixture(t)
	r := New(Memory()...)

	coder := env(t, s, ctx, runID, "T1", "coder", "memory_write", "memory_read")
	res := call(r, ctx, coder, "memory_write", map[string]any{
		"kind": "finding", "subject": "pkg:auth", "predicate": "has_no_tests",
		"object": "coverage 0%", "confidence": 0.9,
	})
	if res.Error != "" {
		t.Fatalf("coder write: %s", res.Error)
	}

	reviewer := env(t, s, ctx, runID, "T2", "reviewer", "memory_read")
	res = call(r, ctx, reviewer, "memory_read", map[string]any{})
	if res.Error != "" {
		t.Fatalf("reviewer read: %s", res.Error)
	}
	if !strings.Contains(res.Summary, "has_no_tests") {
		t.Fatalf("reviewer did not inherit the coder's finding:\n%s", res.Summary)
	}

	// A sibling that this task does not depend on stays invisible: its claims
	// are mid-flight and unverified (§5.4).
	sibling := env(t, s, ctx, runID, "T3", "coder", "memory_write")
	call(r, ctx, sibling, "memory_write", map[string]any{
		"kind": "finding", "subject": "pkg:billing", "predicate": "is_vendored",
		"object": "under third_party/", "confidence": 0.8,
	})
	res = call(r, ctx, reviewer, "memory_read", map[string]any{})
	if strings.Contains(res.Summary, "pkg:billing") {
		t.Errorf("reviewer saw a non-ancestor sibling's claim:\n%s", res.Summary)
	}
}

// Constraints are binding on everyone, so they ignore the ancestor scope.
func TestConstraintsReachEveryTask(t *testing.T) {
	s, runID, ctx := fixture(t)
	r := New(Memory()...)

	writer := env(t, s, ctx, runID, "T3", "coder", "memory_write")
	call(r, ctx, writer, "memory_write", map[string]any{
		"kind": "constraint", "subject": "run", "predicate": "must_not_modify",
		"object": "internal/legacy/**", "confidence": 1.0,
	})
	reader := env(t, s, ctx, runID, "T1", "coder", "memory_read")
	if res := call(r, ctx, reader, "memory_read", map[string]any{}); !strings.Contains(res.Summary, "must_not_modify") {
		t.Fatalf("constraint did not reach an unrelated task:\n%s", res.Summary)
	}
}

func TestWriteEnforcement(t *testing.T) {
	s, runID, ctx := fixture(t)
	r := New(Memory()...)
	e := env(t, s, ctx, runID, "T1", "coder", "memory_write")

	// A fact must point at the tool result that observed it.
	res := call(r, ctx, e, "memory_write", map[string]any{
		"kind": "fact", "subject": "file:go.mod", "predicate": "declares_module",
		"object": "github.com/x/y", "confidence": 1.0,
	})
	if !strings.Contains(res.Error, "requires evidence") {
		t.Errorf("fact without evidence was accepted: %+v", res)
	}

	// With evidence that resolves to a real artifact, it is tool_observed.
	art, err := s.PutArtifact(ctx, store.ArtifactInput{
		RunID: runID, TaskID: e.TaskID, Kind: "file_content",
		MediaType: "text/plain", Content: []byte("module github.com/x/y\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	res = call(r, ctx, e, "memory_write", map[string]any{
		"kind": "fact", "subject": "file:go.mod", "predicate": "declares_module",
		"object": "github.com/x/y", "confidence": 1.0, "evidence": art.ID,
	})
	if !strings.Contains(res.Summary, "tool_observed") {
		t.Errorf("evidence-backed fact was not tool_observed: %+v", res)
	}

	// The same claim again is a no-op, not a duplicate row.
	res = call(r, ctx, e, "memory_write", map[string]any{
		"kind": "fact", "subject": "file:go.mod", "predicate": "declares_module",
		"object": "  GITHUB.COM/X/Y  ", "confidence": 1.0, "evidence": art.ID,
	})
	if !strings.Contains(res.Summary, "already known") {
		t.Errorf("normalised duplicate was not deduped: %+v", res)
	}

	// A different object for the same subject+predicate supersedes rather than
	// overwrites, because the new reading is not always the right one.
	res = call(r, ctx, e, "memory_write", map[string]any{
		"kind": "fact", "subject": "file:go.mod", "predicate": "declares_module",
		"object": "github.com/x/z", "confidence": 0.6, "evidence": art.ID,
	})
	if !strings.Contains(res.Summary, "supersede") {
		t.Errorf("contradiction was not recorded: %+v", res)
	}
	var contradictions int
	s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM events WHERE run_id=$1 AND type='memory.contradiction'`, runID).Scan(&contradictions)
	if contradictions != 1 {
		t.Errorf("want 1 memory.contradiction event, got %d", contradictions)
	}
}

// A model that writes 40 claims is narrating, not observing.
func TestClaimRateLimit(t *testing.T) {
	s, runID, ctx := fixture(t)
	r := New(Memory()...)
	e := env(t, s, ctx, runID, "T1", "coder", "memory_write")

	for i := 0; i < store.MaxClaimsPerAttempt; i++ {
		res := call(r, ctx, e, "memory_write", map[string]any{
			"kind": "decision", "subject": fmt.Sprintf("task:T%d", i), "predicate": "chose",
			"object": fmt.Sprintf("option %d", i), "confidence": 0.7,
		})
		if res.Error != "" {
			t.Fatalf("claim %d rejected early: %s", i, res.Error)
		}
	}
	res := call(r, ctx, e, "memory_write", map[string]any{
		"kind": "decision", "subject": "task:overflow", "predicate": "chose",
		"object": "one too many", "confidence": 0.7,
	})
	if !strings.Contains(res.Error, "claim limit") {
		t.Errorf("rate limit did not fire: %+v", res)
	}
}

// Capability filtering: a reviewer is not shown memory_write at all.
func TestCapabilityFiltering(t *testing.T) {
	s, runID, ctx := fixture(t)
	r := New(Memory()...)
	e := env(t, s, ctx, runID, "T2", "reviewer", "memory_read", "artifact_read")

	if names := toolNames(r.ForRole(e.Allowed)); len(names) != 2 || contains(names, "memory_write") {
		t.Errorf("reviewer was offered %v", names)
	}
	res := call(r, ctx, e, "memory_write", map[string]any{
		"kind": "finding", "subject": "x", "predicate": "y", "object": "z", "confidence": 0.5,
	})
	if !strings.Contains(res.Error, "not available") {
		t.Errorf("denied tool did not return an observation: %+v", res)
	}
}

// Small models emit every argument as a string. That should cost a coercion,
// not a step.
func TestStringifiedArgumentsAreAccepted(t *testing.T) {
	s, runID, ctx := fixture(t)
	r := New(Native()...)
	e := env(t, s, ctx, runID, "T1", "coder", "memory_write", "memory_read")

	res := call(r, ctx, e, "memory_write", map[string]any{
		"kind": "finding", "subject": "pkg:auth", "predicate": "has_no_tests",
		"object": "coverage 0%", "confidence": "0.9", // a string, not a number
	})
	if res.Error != "" {
		t.Fatalf("stringified confidence was rejected: %s", res.Error)
	}
	res = call(r, ctx, e, "memory_read", map[string]any{
		"limit": "20", "kinds": `["finding"]`, // a string holding an array
	})
	if res.Error != "" || !strings.Contains(res.Summary, "has_no_tests") {
		t.Fatalf("stringified arguments were rejected: %+v", res)
	}
	// A genuine string that happens to look numeric stays a string.
	if res := call(r, ctx, e, "memory_write", map[string]any{
		"kind": "fact", "subject": "42", "predicate": "is", "object": "the answer",
		"confidence": 1.0, "evidence": "none",
	}); res.Error != "" {
		t.Errorf("numeric-looking string argument was coerced: %s", res.Error)
	}
}

func TestArtifactReadWindow(t *testing.T) {
	s, runID, ctx := fixture(t)
	r := New(Memory()...)
	e := env(t, s, ctx, runID, "T1", "coder", "artifact_read")

	body := strings.Repeat("line of test output\n", 200)
	art, err := s.PutArtifact(ctx, store.ArtifactInput{
		RunID: runID, TaskID: e.TaskID, Kind: "test_output",
		MediaType: "text/plain", Content: []byte(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(art.Summary) > 300 {
		t.Errorf("summary is %d chars, must be <= 300", len(art.Summary))
	}
	res := call(r, ctx, e, "artifact_read", map[string]any{"id": art.ID, "limit": 40})
	if res.Error != "" || len(res.Summary) != 40 {
		t.Errorf("windowed read returned %d chars: %+v", len(res.Summary), res)
	}
	if res := call(r, ctx, e, "artifact_read", map[string]any{"id": "nope"}); res.Error == "" {
		t.Error("missing artifact should return an error the model can see")
	}
}

func toolNames(ts []Tool) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
