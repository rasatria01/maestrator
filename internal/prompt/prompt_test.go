package prompt

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rasatria01/theorm/internal/spec"
	"github.com/rasatria01/theorm/internal/store"
)

// The memory section's policy is "drop lowest rerank score first". Records
// arrive highest score first, so on overflow the tail is what goes.
func TestFitMemoryDropsLowestScoreFirst(t *testing.T) {
	s := &Section{Budget: 1000}
	fitMemory(s, []string{"top record", "next record"})
	if !strings.Contains(s.Text, "top record") || !strings.Contains(s.Text, "next record") || s.Dropped != 0 {
		t.Errorf("both should fit: text=%q dropped=%d", s.Text, s.Dropped)
	}

	big := strings.Repeat("x", 400) // ~127 tokens; two will not fit in 200
	s = &Section{Budget: 200}
	fitMemory(s, []string{"keep " + big, "drop " + big})
	if !strings.Contains(s.Text, "keep") || strings.Contains(s.Text, "drop") {
		t.Errorf("only the top record should survive: %q", s.Text)
	}
	if s.Dropped != 1 {
		t.Errorf("dropped=%d, want 1", s.Dropped)
	}
}

func TestSectionBudgetsMatchTheWindow(t *testing.T) {
	sum := 0
	for _, b := range budgets {
		sum += b.Tokens
	}
	if want := WindowDeep - reserveOutput - safetyMargin; sum != want {
		t.Fatalf("section budgets sum to %d, want %d", sum, want)
	}
}

// Needs the compose database: docker compose up -d db
func fixture(t *testing.T, class string) (*store.Store, string, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, store.DSN())
	if err != nil {
		t.Skipf("no database at %s: %v", store.DSN(), err)
	}
	t.Cleanup(s.Close)

	sp := &spec.Spec{
		Path: "test", SHA256: "test",
		Meta: spec.Meta{Repo: "github.com/x/y", Title: "add a refresh endpoint"},
		Tasks: []spec.Task{{
			ID: "T1", Title: "Add JWT refresh endpoint", Role: "coder", ResourceClass: class,
			WriteSet: []string{"internal/auth/**"},
			ReadSet: spec.ReadSet{
				Subjects:   []string{"file:internal/auth/"},
				ClaimKinds: []string{"constraint", "decision", "finding", "failure", "fact"},
				Limit:      200, // past the read-set ceiling, so the budget is what binds
			},
			Acceptance: []spec.Acceptance{{Kind: "cmd", Run: "go test ./internal/auth/..."}},
			Gates:      []string{"build", "test"},
			Tools:      []string{"read_file", "write_file", "memory_write"},
		}},
	}
	runID, err := s.CreateRun(ctx, sp, "compiled", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Pool().Exec(ctx, `DELETE FROM runs WHERE id=$1`, runID) })
	return s, runID, ctx
}

// Flood L1 past the claims budget and confirm the assembler drops by policy
// rather than truncating the prompt at the end.
func TestNoSectionExceedsItsBudget(t *testing.T) {
	s, runID, ctx := fixture(t, "gpu_deep")
	taskID, err := s.TaskID(ctx, runID, "T1")
	if err != nil {
		t.Fatal(err)
	}

	writeClaim := func(attempt string, c store.Claim) {
		c.RunID, c.TaskID, c.AttemptID = runID, taskID, attempt
		if _, err := s.WriteClaim(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 96; i++ {
		if i%store.MaxClaimsPerAttempt == 0 {
			if _, err := s.StartAttempt(ctx, taskID); err != nil {
				t.Fatal(err)
			}
		}
		attempt, err := s.StartAttempt(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		writeClaim(attempt, store.Claim{
			Kind: "finding", Subject: fmt.Sprintf("file:internal/auth/handler_%d.go", i),
			Predicate: "needs_attention",
			Object: fmt.Sprintf("finding number %d, with enough prose attached that a hundred "+
				"of them will not fit inside a 2500 token section of the window", i),
			Confidence: float32(i%10) / 10,
		})
	}
	writeClaim("", store.Claim{
		Kind: "constraint", Subject: "run", Predicate: "must_not_modify",
		Object: "internal/legacy/**", Confidence: 1,
	})

	p, err := Assemble(ctx, s, runID, "T1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range p.Sections {
		if sec.Tokens > sec.Budget {
			t.Errorf("section %s used %d tokens against a budget of %d", sec.Name, sec.Tokens, sec.Budget)
		}
	}
	if max := p.Window - reserveOutput - safetyMargin; p.Total > max {
		t.Errorf("prompt is %d tokens, budget is %d", p.Total, max)
	}

	claims := section(p, "claims")
	if claims.Dropped == 0 {
		t.Error("96 claims fit the claims budget, so the fixture is not exercising overflow")
	}
	// Dropping starts at the bottom of the sorted list, so the highest
	// confidence claim must survive.
	if !strings.Contains(claims.Text, "confidence 0.90") {
		t.Error("highest-confidence claim was dropped before lower-confidence ones")
	}
	// Constraints are binding and never compete for space with findings.
	if !strings.Contains(section(p, "constraints").Text, "must_not_modify") {
		t.Error("constraint missing from the assembled prompt")
	}
	if !strings.Contains(p.User, "<claims>") || !strings.Contains(p.User, "## Constraints") {
		t.Errorf("sections are not delimited in the rendered prompt:\n%s", p.User)
	}
	if !strings.Contains(p.User, "go test ./internal/auth/...") {
		t.Error("acceptance criteria missing from the task section")
	}
}

// A shallow slot gets the same shape of prompt at half the size.
func TestShallowClassHalvesTheBudgets(t *testing.T) {
	s, runID, ctx := fixture(t, "gpu_shallow")
	p, err := Assemble(ctx, s, runID, "T1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Window != WindowShallow {
		t.Fatalf("window is %d, want %d", p.Window, WindowShallow)
	}
	if got := section(p, "claims").Budget; got != 1250 {
		t.Errorf("claims budget is %d, want 1250", got)
	}
}

func section(p *Prompt, name string) Section {
	for _, s := range p.Sections {
		if s.Name == name {
			return s
		}
	}
	return Section{}
}
