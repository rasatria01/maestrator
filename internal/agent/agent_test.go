package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/rasatria01/theorm/internal/inference"
	"github.com/rasatria01/theorm/internal/spec"
	"github.com/rasatria01/theorm/internal/store"
	"github.com/rasatria01/theorm/internal/tools"
)

// scripted returns canned responses in order, so the loop's own behaviour is
// tested without a GPU. The last response repeats if the loop keeps going.
type scripted struct {
	steps   []inference.Response
	n       int
	prompts []string
}

func (m *scripted) Name() string { return "scripted" }

func (m *scripted) Chat(ctx context.Context, req inference.Request) (inference.Response, error) {
	m.prompts = append(m.prompts, req.Messages[1].Content)
	r := m.steps[min(m.n, len(m.steps)-1)]
	m.n++
	return r, nil
}

func toolCall(name string, args any) inference.Response {
	b, _ := json.Marshal(args)
	var c inference.ToolCall
	c.ID, c.Type, c.Function.Name, c.Function.Arguments = "call_1", "function", name, b
	return inference.Response{ToolCalls: []inference.ToolCall{c}}
}

func text(s string) inference.Response { return inference.Response{Content: s} }

// Needs the compose database: docker compose up -d db
func fixture(t *testing.T, role string, model inference.Model) (*Runner, *store.Store, string, context.Context) {
	t.Helper()
	t.Setenv("THEORM_ARTIFACTS", t.TempDir())
	ctx := context.Background()
	s, err := store.Open(ctx, store.DSN())
	if err != nil {
		t.Skipf("no database at %s: %v", store.DSN(), err)
	}
	t.Cleanup(s.Close)

	sp := &spec.Spec{
		Path: "test", SHA256: "test",
		Meta: spec.Meta{Theorm: "v1", Base: "HEAD", Repo: "github.com/rasatria01/theorm",
			Title: "understand the repository"},
		Tasks: []spec.Task{{
			ID: "T1", Title: "Map how tasks reach the model", Role: role, ResourceClass: "gpu_deep",
			WriteSet: []string{},
			ReadSet:  spec.ReadSet{ClaimKinds: []string{"constraint", "finding", "failure"}},
		}},
	}
	if d := spec.Validate(sp); len(d) != 0 {
		t.Fatalf("fixture spec: %v", d)
	}
	runID, err := s.CreateRun(ctx, sp, "compiled", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Pool().Exec(ctx, `DELETE FROM runs WHERE id=$1`, runID) })

	dir, _ := os.Getwd()
	return &Runner{Store: s, Tools: tools.New(tools.Native()...), Model: model,
		Dir: strings.TrimSuffix(dir, "\\internal\\agent")}, s, runID, ctx
}

// An Explorer investigates, records what it found, and stops.
func TestExplorerRunsAndRecordsFindings(t *testing.T) {
	model := &scripted{steps: []inference.Response{
		toolCall("list_dir", map[string]any{"path": "internal"}),
		toolCall("grep", map[string]any{"pattern": "func Assemble", "glob": "*.go"}),
		toolCall("memory_write", map[string]any{
			"kind": "finding", "subject": "file:internal/prompt/prompt.go",
			"predicate": "builds", "object": "the L0 context window under a hard budget",
			"confidence": 0.9,
		}),
		toolCall("task_complete", map[string]any{"summary": "prompt assembly lives in internal/prompt"}),
	}}
	r, s, runID, ctx := fixture(t, "explorer", model)

	out, err := r.RunTask(ctx, runID, "T1")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Completed || out.Steps != 4 {
		t.Fatalf("outcome: %+v", out)
	}

	taskID, err := s.TaskID(ctx, runID, "T1")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := s.ReadClaims(ctx, store.ClaimQuery{
		RunID: runID, TaskID: taskID, Kinds: []string{"finding"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || !strings.Contains(claims[0].Object, "hard budget") {
		t.Errorf("explorer's finding did not reach L1: %+v", claims)
	}

	// Each step's prompt carries the earlier steps, and only their summaries.
	last := model.prompts[len(model.prompts)-1]
	if !strings.Contains(last, "<scratchpad>") || !strings.Contains(last, "step 1: list_dir") {
		t.Errorf("scratchpad missing prior steps:\n%s", last)
	}
	if strings.Contains(last, "package prompt") {
		t.Error("a tool payload leaked into the prompt instead of its summary")
	}

	var outcome, detail string
	s.Pool().QueryRow(ctx, `SELECT outcome, detail FROM attempts WHERE id=$1`, out.AttemptID).Scan(&outcome, &detail)
	if outcome != "ok" || !strings.Contains(detail, "internal/prompt") {
		t.Errorf("attempt recorded as %q / %q", outcome, detail)
	}
}

// The Explorer is read-only: the tools it could do damage with are not offered
// to it at all, and calling one anyway comes back as an observation.
func TestExplorerCannotWrite(t *testing.T) {
	model := &scripted{steps: []inference.Response{
		toolCall("run_cmd", map[string]any{"cmd": "go build ./..."}),
		toolCall("task_complete", map[string]any{"summary": "gave up on running commands"}),
	}}
	r, s, runID, ctx := fixture(t, "explorer", model)

	offered := map[string]bool{}
	for _, spec := range toolSpecs(r.Tools.ForRole([]string{"read_file", "list_dir", "grep",
		"memory_read", "memory_write", "artifact_read", "task_complete"})) {
		offered[spec.Name] = true
	}
	for _, forbidden := range []string{"run_cmd", "write_file", "str_replace", "git_commit"} {
		if offered[forbidden] {
			t.Errorf("explorer was offered %s", forbidden)
		}
	}

	if _, err := r.RunTask(ctx, runID, "T1"); err != nil {
		t.Fatal(err)
	}
	var denied int
	s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM events WHERE run_id=$1 AND type='tool.denied'`, runID).Scan(&denied)
	if denied != 1 {
		t.Errorf("want the run_cmd call denied and recorded, got %d denials", denied)
	}
	// The denial is fed back, not raised: the agent still finished.
	if !strings.Contains(strings.Join(model.prompts, "\n"), "not available to role explorer") {
		t.Error("the denial was not returned to the model as an observation")
	}
}

// F6: narration is a failure. One nudge, then the attempt ends.
func TestNarrationGetsOneNudge(t *testing.T) {
	model := &scripted{steps: []inference.Response{
		text("Sure! I will now look at the repository structure."),
		text("Let me start by listing the directory."),
	}}
	r, _, runID, ctx := fixture(t, "explorer", model)

	out, err := r.RunTask(ctx, runID, "T1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Completed || !strings.Contains(out.Reason, "stalled") {
		t.Fatalf("outcome: %+v", out)
	}
	if out.Steps != 2 {
		t.Errorf("stalled after %d steps, want 2", out.Steps)
	}
	if !strings.Contains(model.prompts[1], "called no tool") {
		t.Error("the model was not nudged after its first step of narration")
	}
}

// F5: the same call with the same arguments, over and over, is a loop.
func TestIdenticalToolCallsAreALoop(t *testing.T) {
	model := &scripted{steps: []inference.Response{
		toolCall("list_dir", map[string]any{"path": "internal"}),
	}}
	r, _, runID, ctx := fixture(t, "explorer", model)

	out, err := r.RunTask(ctx, runID, "T1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Completed || !strings.Contains(out.Reason, "tool loop") {
		t.Fatalf("outcome: %+v", out)
	}
	if out.Steps != repeatLimit {
		t.Errorf("loop detected after %d steps, want %d", out.Steps, repeatLimit)
	}
}

// A live smoke test against whatever model server is configured. Skipped unless
// THEORM_LLAMA_URL and THEORM_MODEL point at a running one.
func TestAgainstARealModel(t *testing.T) {
	if os.Getenv("THEORM_MODEL") == "" {
		t.Skip("set THEORM_LLAMA_URL and THEORM_MODEL to run this")
	}
	r, s, runID, ctx := fixture(t, "explorer", inference.New())
	out, err := r.RunTask(ctx, runID, "T1")
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Pool().Query(ctx,
		`SELECT type, payload->>'name' FROM events WHERE run_id=$1 AND type LIKE 'tool.%' ORDER BY id`, runID)
	defer rows.Close()
	var calls []string
	for rows.Next() {
		var typ string
		var name *string
		rows.Scan(&typ, &name)
		if name != nil && typ == "tool.called" {
			calls = append(calls, *name)
		}
	}
	t.Logf("model=%s steps=%d completed=%v reason=%q tools=%v",
		inference.New().Name(), out.Steps, out.Completed, out.Reason, calls)
	if len(calls) == 0 {
		t.Error("the model called no tools at all")
	}
	fmt.Println(out.Summary)
}
