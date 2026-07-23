package spec

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/rasatria01/theorm/internal/role"
)

// Slot sizes the feasibility check and Rule 5 are measured against (§2.3).
const (
	CtxQuality    = 32000 // 14B, one slot
	CtxThroughput = 16000 // 7B, four slots
	MaxWriteGlobs = 20    // §4.6 control 3
	MaxDegree     = 4
)

// Command allowlist (§8.7). Checked at compile time, not at run time.
var cmdAllow = map[string][]string{
	"go":    {"build", "test", "vet"},
	"gofmt": nil, // any argv
	"npm":   {"run"},
	"pnpm":  {"run"},
	"git":   {"diff", "status"}, // mutation goes through the git_commit tool, which stages only the write set

}

var resourceClasses = map[string]bool{"gpu_deep": true, "gpu_shallow": true, "cpu_only": true, "browser": true}

// Validate checks the spec and fills each task's effective tool set.
// It returns every problem it finds: a partially valid spec produces a report
// and no run (§4.4).
func Validate(s *Spec) []Diag {
	var d []Diag
	if s.Meta.Theorm != "v1" {
		d = append(d, Diag{1, fmt.Sprintf("unsupported format version %q, want v1", s.Meta.Theorm)})
	}
	if s.Meta.Repo == "" {
		d = append(d, Diag{1, "front matter: repo is required"})
	}
	if s.Meta.Base == "" {
		d = append(d, Diag{1, "front matter: base commit is required (§4.5)"})
	}

	seen := map[string]bool{}
	for i := range s.Tasks {
		t := &s.Tasks[i]
		switch {
		case t.ID == "":
			d = append(d, Diag{t.Line, "task: id is required"})
		case seen[t.ID]:
			d = append(d, Diag{t.Line, "duplicate task id " + t.ID})
		}
		seen[t.ID] = true

		if t.Title == "" {
			d = append(d, Diag{t.Line, t.ID + ": title is required"})
		}
		r, ok := role.Get(t.Role)
		if !ok {
			d = append(d, Diag{t.Line, fmt.Sprintf("%s: unknown role %q", t.ID, t.Role)})
		}
		t.Tools = clamp(r.Tools, s.Meta.Policy.AllowTools)

		if !resourceClasses[t.ResourceClass] {
			d = append(d, Diag{t.Line, fmt.Sprintf("%s: resource_class must be one of gpu_deep, gpu_shallow, cpu_only, browser", t.ID)})
		}
		if t.WriteSet == nil {
			d = append(d, Diag{t.Line, t.ID + ": write_set is required, use [] for a read-only task (§6.1)"})
		}
		d = append(d, checkWriteSet(t)...)

		if t.EstContextTokens > CtxQuality {
			d = append(d, Diag{t.Line, fmt.Sprintf("%s: est_context_tokens %d exceeds the largest slot (%d). Split the task",
				t.ID, t.EstContextTokens, CtxQuality)})
		}
		for _, a := range t.Acceptance {
			if a.Kind == "cmd" {
				if err := AllowedCmd(a.Run); err != nil {
					d = append(d, Diag{t.Line, fmt.Sprintf("%s: acceptance command rejected: %v", t.ID, err)})
				}
			}
		}
	}

	for _, t := range s.Tasks {
		for _, dep := range t.DependsOn {
			if !seen[dep] {
				d = append(d, Diag{t.Line, fmt.Sprintf("%s: depends_on %s, which does not exist", t.ID, dep)})
			}
		}
	}
	if cyc := findCycle(s.Tasks); cyc != "" {
		d = append(d, Diag{1, "dependency cycle: " + cyc})
	}
	sort.SliceStable(d, func(i, j int) bool { return d[i].Line < d[j].Line })
	return d
}

func checkWriteSet(t *Task) []Diag {
	var d []Diag
	if len(t.WriteSet) > MaxWriteGlobs {
		d = append(d, Diag{t.Line, fmt.Sprintf("%s: %d write globs exceeds the ceiling of %d", t.ID, len(t.WriteSet), MaxWriteGlobs)})
	}
	for _, g := range t.WriteSet {
		switch {
		case g == "**" || g == "*" || g == "/" || g == ".":
			d = append(d, Diag{t.Line, fmt.Sprintf("%s: write glob %q covers the whole repository", t.ID, g)})
		case strings.HasPrefix(g, "/") || strings.Contains(g, ":"):
			d = append(d, Diag{t.Line, fmt.Sprintf("%s: write glob %q is absolute", t.ID, g)})
		case strings.Contains(g, ".."):
			d = append(d, Diag{t.Line, fmt.Sprintf("%s: write glob %q escapes the repository root", t.ID, g)})
		}
	}
	return d
}

// AllowedCmd checks an argv-style command line against the §8.7 allowlist.
// No shell: metacharacters are rejected outright rather than escaped.
func AllowedCmd(cmd string) error {
	if strings.ContainsAny(cmd, "|;&$><`\n") {
		return fmt.Errorf("%q contains shell metacharacters", cmd)
	}
	argv := strings.Fields(cmd)
	if len(argv) == 0 {
		return fmt.Errorf("empty command")
	}
	subs, ok := cmdAllow[argv[0]]
	if !ok {
		return fmt.Errorf("%q is not an allowlisted binary", argv[0])
	}
	if subs == nil {
		return nil
	}
	if len(argv) < 2 || !contains(subs, argv[1]) {
		return fmt.Errorf("%s allows only %s", argv[0], strings.Join(subs, ", "))
	}
	return nil
}

// clamp is intersection, never union (§4.6 control 1).
func clamp(caps, allow []string) []string {
	if len(allow) == 0 {
		return caps
	}
	var out []string
	for _, c := range caps {
		if contains(allow, c) {
			out = append(out, c)
		}
	}
	return out
}

func findCycle(tasks []Task) string {
	deps := map[string][]string{}
	for _, t := range tasks {
		deps[t.ID] = t.DependsOn
	}
	state := map[string]int{} // 0 unvisited, 1 on stack, 2 done
	var path []string
	var walk func(string) string
	walk = func(id string) string {
		switch state[id] {
		case 1:
			return strings.Join(append(path, id), " -> ")
		case 2:
			return ""
		}
		state[id], path = 1, append(path, id)
		for _, dep := range deps[id] {
			if c := walk(dep); c != "" {
				return c
			}
		}
		state[id], path = 2, path[:len(path)-1]
		return ""
	}
	for _, t := range tasks {
		if c := walk(t.ID); c != "" {
			return c
		}
	}
	return ""
}

// --- projected execution (§6.3, §6.4) ---

type Wave struct {
	Tasks  []string
	Degree int
	Class  string // gpu_deep|gpu_shallow|cpu_only|browser
	Mode   string // quality|throughput|cpu|browser
	Reason string
	MaxCtx int
}

// Waves projects how the scheduler will batch the DAG. The runtime decision
// also consults free GPU slots (Rule 4), which do not exist at compile time,
// so this is the optimistic upper bound shown in --explain.
func Waves(tasks []Task) []Wave {
	byID := map[string]Task{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	level := map[string]int{}
	var depth func(string) int
	depth = func(id string) int {
		if l, ok := level[id]; ok {
			return l
		}
		level[id] = 0 // breaks cycles; Validate reports them separately
		max := 0
		for _, d := range byID[id].DependsOn {
			if l := depth(d) + 1; l > max {
				max = l
			}
		}
		level[id] = max
		return max
	}
	levels := map[int][]Task{}
	for _, t := range tasks {
		levels[depth(t.ID)] = append(levels[depth(t.ID)], t)
	}

	var waves []Wave
	for l := 0; l < len(levels); l++ {
		ready := levels[l]
		for _, batch := range batchLevel(ready) {
			waves = append(waves, batch)
		}
	}
	return waves
}

func batchLevel(ready []Task) []Wave {
	var waves []Wave
	var placed []Task
	for _, t := range ready {
		// Rule 2: a write-set overlap with anything already in this batch
		// forces a new batch. Rule 5: a deep task runs alone.
		soloed := t.ResourceClass != "gpu_shallow" && t.ResourceClass != "cpu_only"
		soloed = soloed || t.EstContextTokens > CtxThroughput
		i := len(waves) - 1
		if i < 0 || soloed || waves[i].Degree >= maxDegree(t.ResourceClass) ||
			!sameClass(placed, waves[i], t) || conflicts(t, waves[i], byIDs(ready)) {
			waves = append(waves, Wave{})
			i = len(waves) - 1
		}
		w := &waves[i]
		w.Tasks = append(w.Tasks, t.ID)
		w.Degree, w.Class = len(w.Tasks), t.ResourceClass
		if t.EstContextTokens > w.MaxCtx {
			w.MaxCtx = t.EstContextTokens
		}
		w.Mode, w.Reason = mode(t, w, ready)
		placed = append(placed, t)
	}
	return waves
}

func mode(t Task, w *Wave, ready []Task) (string, string) {
	switch t.ResourceClass {
	case "browser":
		return "browser", "browser lease is exclusive (A8)"
	case "cpu_only":
		return "cpu", "no GPU slot needed"
	}
	// Rule 6: enough shallow work to amortise a model swap.
	steps := 0
	for _, id := range w.Tasks {
		steps += estSteps(byIDs(ready)[id])
	}
	if t.ResourceClass == "gpu_shallow" && len(w.Tasks) >= 3 && len(w.Tasks)*steps >= 20 {
		return "throughput", "all gpu_shallow, disjoint write sets, swap amortises"
	}
	if w.Degree == 1 {
		return "quality", "deep context, runs alone (Rule 5)"
	}
	return "quality", "disjoint write sets, fits the slot"
}

func estSteps(t Task) int {
	if t.EstSteps == 0 {
		return 8
	}
	return t.EstSteps
}

func maxDegree(class string) int {
	switch class {
	case "cpu_only":
		return 8
	case "gpu_shallow":
		return MaxDegree
	default:
		return 1
	}
}

func sameClass(placed []Task, w Wave, t Task) bool {
	for _, p := range placed {
		if contains(w.Tasks, p.ID) && p.ResourceClass != t.ResourceClass {
			return false
		}
	}
	return true
}

func conflicts(t Task, w Wave, byID map[string]Task) bool {
	for _, id := range w.Tasks {
		if WriteSetsOverlap(byID[id].WriteSet, t.WriteSet) {
			return true
		}
	}
	return false
}

func byIDs(tasks []Task) map[string]Task {
	m := map[string]Task{}
	for _, t := range tasks {
		m[t.ID] = t
	}
	return m
}

// WriteSetsOverlap decides whether two tasks may run concurrently (§6.4 Rule 2).
// ponytail: compares literal prefixes up to the first wildcard, so it over-
// detects (`app/*.ts` vs `app/*.css` reads as a conflict) and never under-
// detects. Over-detection costs parallelism; under-detection corrupts the tree.
// Upgrade to real glob intersection only if the scheduler is starved.
func WriteSetsOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			px, py := globPrefix(x), globPrefix(y)
			if strings.HasPrefix(px, py) || strings.HasPrefix(py, px) {
				return true
			}
		}
	}
	return false
}

func globPrefix(g string) string {
	if i := strings.IndexAny(g, "*?["); i >= 0 {
		g = g[:i]
	}
	return strings.TrimPrefix(g, "./")
}

func contains(hay []string, needle string) bool { return slices.Contains(hay, needle) }
