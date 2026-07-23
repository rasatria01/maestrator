// Package prompt builds the L0 context window (§5.7). L0 is never a source of
// truth: it is rebuilt from L1, L2 and L3 on every call, to a hard budget, with
// a stated policy per section. Nothing here truncates at the end and hopes.
package prompt

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rasatria01/theorm/internal/role"
	"github.com/rasatria01/theorm/internal/store"
)

// Window sizes per resource class (§2.3). Budgets scale with the window, so a
// 7B slot gets the same shape of prompt at half the size.
const (
	WindowDeep    = 32000
	WindowShallow = 16000
)

// The §5.7 table. Reserve and margin are never allocated to input; the section
// budgets sum to exactly WindowDeep minus both.
var budgets = []struct {
	Name     string
	Tokens   int
	Overflow string
}{
	{"system", 800, "never truncated"},
	{"task", 700, "never truncated"},
	{"constraints", 500, "never truncated, drop a lower-priority section instead"},
	{"claims", 2500, "drop lowest confidence first, then oldest"},
	{"memory", 3000, "drop lowest rerank score first"},
	{"repository", 6000, "degrade from body to outline to path-only"},
	{"tool_results", 8000, "sliding window, oldest replaced by its own summary"},
	{"scratchpad", 4000, "compress turns older than the last 3"},
}

const (
	reserveOutput = 4000
	safetyMargin  = 2500
)

type Section struct {
	Name     string
	Budget   int
	Tokens   int
	Dropped  int // items that did not fit
	Overflow string
	Text     string
}

type Prompt struct {
	System   string
	User     string
	Sections []Section
	Window   int
	Total    int // input tokens, excluding the output reserve
}

// Estimate is deliberately pessimistic: code tokenizes worse than prose, so
// len/3.2 rather than the usual len/4. Over-estimating wastes a little window;
// under-estimating truncates mid-generation.
func Estimate(s string) int { return (len(s)*10 + 31) / 32 }

// Assemble builds the prompt for one task. It never returns an over-budget
// prompt: sections degrade by their stated policy, and a section marked
// "never truncated" that does not fit is an error, not a degradation (F4).
// turns are this attempt's prior steps, oldest first, one line each.
func Assemble(ctx context.Context, s *store.Store, runID, key string, turns []string) (*Prompt, error) {
	task, err := s.Task(ctx, runID, key)
	if err != nil {
		return nil, err
	}
	window := WindowDeep
	if task.ResourceClass == "gpu_shallow" {
		window = WindowShallow
	}
	scale := float64(window) / float64(WindowDeep)

	p := &Prompt{System: role.SystemPrompt(task.Role), Window: window}
	for _, b := range budgets {
		p.Sections = append(p.Sections, Section{
			Name: b.Name, Budget: int(float64(b.Tokens) * scale), Overflow: b.Overflow,
		})
	}
	sec := func(name string) *Section {
		for i := range p.Sections {
			if p.Sections[i].Name == name {
				return &p.Sections[i]
			}
		}
		panic("unknown section " + name)
	}

	sec("system").Text = p.System
	sec("task").Text = renderTask(task)

	q := store.ClaimQuery{
		RunID: runID, TaskID: task.ID, Role: task.Role,
		Kinds: task.Contract.ReadSet.ClaimKinds, Subjects: task.Contract.ReadSet.Subjects,
		Limit: task.Contract.ReadSet.Limit,
	}
	claims, err := s.ReadClaims(ctx, q)
	if err != nil {
		return nil, err
	}
	var constraints, rest []store.Claim
	for _, c := range claims {
		if c.Kind == "constraint" {
			constraints = append(constraints, c)
		} else {
			rest = append(rest, c)
		}
	}
	sec("constraints").Text = renderClaims(constraints)

	// Lowest confidence goes first, then oldest, because a low-confidence claim
	// that displaces a binding one is the worst trade in the budget.
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].Confidence != rest[j].Confidence {
			return rest[i].Confidence > rest[j].Confidence
		}
		return rest[i].CreatedAt.After(rest[j].CreatedAt)
	})
	fitClaims(sec("claims"), rest)

	artifacts, err := s.TaskArtifacts(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	fitArtifacts(sec("tool_results"), artifacts)
	fitTurns(sec("scratchpad"), turns)

	// memory (L3) and repository outlines have no source yet: hybrid retrieval
	// lands in Phase B and the repo index in B1. They stay in the table at zero
	// so the budget stays honest.

	for i := range p.Sections {
		s := &p.Sections[i]
		s.Tokens = Estimate(s.Text)
		if s.Tokens > s.Budget && strings.HasPrefix(s.Overflow, "never truncated") {
			return nil, fmt.Errorf("section %q needs %d tokens but its budget is %d (%s)",
				s.Name, s.Tokens, s.Budget, s.Overflow)
		}
		p.Total += s.Tokens
	}
	if max := window - reserveOutput - safetyMargin; p.Total > max {
		return nil, fmt.Errorf("assembled prompt is %d tokens, budget is %d", p.Total, max)
	}
	p.User = render(p.Sections)
	return p, nil
}

func fitClaims(s *Section, claims []store.Claim) {
	for i := range claims {
		text := renderClaims(claims[:i+1])
		if Estimate(text) > s.Budget {
			s.Dropped = len(claims) - i
			return
		}
		s.Text = text
	}
}

func fitArtifacts(s *Section, arts []store.Artifact) {
	var b strings.Builder
	for i, a := range arts {
		line := fmt.Sprintf("artifact://%s (%s, %d bytes) %s\n", a.ID[:12], a.Kind, a.Size, a.Summary)
		if Estimate(b.String()+line) > s.Budget {
			s.Dropped = len(arts) - i
			break
		}
		b.WriteString(line)
	}
	s.Text = b.String()
}

// fitTurns keeps the newest steps and replaces the rest with a count. The
// design calls for compressing older turns; a count is the deterministic
// version of that, and it needs no model call.
// ponytail: swap in the 7B summariser here if agents start losing the thread.
func fitTurns(s *Section, turns []string) {
	keep := len(turns)
	for keep > 0 {
		if Estimate(strings.Join(turns[len(turns)-keep:], "\n")) <= s.Budget-40 {
			break
		}
		keep--
	}
	s.Dropped = len(turns) - keep
	var b strings.Builder
	if s.Dropped > 0 {
		fmt.Fprintf(&b, "[%d earlier steps omitted]\n", s.Dropped)
	}
	for _, t := range turns[len(turns)-keep:] {
		b.WriteString(t + "\n")
	}
	s.Text = b.String()
}

func renderTask(t store.TaskRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Goal: %s\nRepository: %s\n\nTask %s: %s\n", t.RunTitle, t.RepoID, t.Key, t.Title)
	if len(t.WriteSet) > 0 {
		fmt.Fprintf(&b, "You may write only these paths: %s\n", strings.Join(t.WriteSet, ", "))
	} else {
		b.WriteString("You may not write any files in this task.\n")
	}
	if len(t.Contract.Acceptance) > 0 {
		b.WriteString("\nDone means all of:\n")
		for _, a := range t.Contract.Acceptance {
			switch a.Kind {
			case "cmd":
				fmt.Fprintf(&b, "  - `%s` exits 0\n", a.Run)
			case "file_contains":
				fmt.Fprintf(&b, "  - %s contains %s\n", a.Path, a.Pattern)
			case "browser":
				fmt.Fprintf(&b, "  - browser check at %s\n", a.URL)
			default:
				fmt.Fprintf(&b, "  - %s\n", a.Text)
			}
		}
	}
	if len(t.Contract.Gates) > 0 {
		fmt.Fprintf(&b, "\nGates that will run after you finish: %s\n", strings.Join(t.Contract.Gates, ", "))
	}
	return b.String()
}

func renderClaims(claims []store.Claim) string {
	var b strings.Builder
	for _, c := range claims {
		fmt.Fprintf(&b, "[%s] %s %s %s (confidence %.2f, %s)\n",
			c.Kind, c.Subject, c.Predicate, c.Object, c.Confidence, c.Verified)
	}
	return b.String()
}

// render wraps every non-instruction section in an explicit delimiter, so that
// what the model may act on and what it may only consult stay distinguishable.
func render(sections []Section) string {
	var b strings.Builder
	for _, s := range sections {
		if s.Name == "system" || strings.TrimSpace(s.Text) == "" {
			continue
		}
		switch s.Name {
		case "task":
			b.WriteString(s.Text)
		case "constraints":
			b.WriteString("\n## Constraints, binding on this task\n" + s.Text)
		default:
			fmt.Fprintf(&b, "\n<%s>\n%s</%s>\n", s.Name, s.Text, s.Name)
		}
	}
	return b.String()
}
