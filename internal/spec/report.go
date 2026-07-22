package spec

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Drift is the §4.5 verdict: a spec is authored against a commit and executed
// later.
type Drift struct {
	Verdict   string // clean|clean_drift|conflicting|unresolvable|no_repo
	Base      string
	Head      string
	Conflicts map[string][]string // task id -> changed files it cares about
}

func CheckDrift(dir string, s *Spec) Drift {
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	head, err := git("rev-parse", "--short", "HEAD")
	if err != nil {
		return Drift{Verdict: "no_repo"}
	}
	base, err := git("rev-parse", "--short", s.Meta.Base+"^{commit}")
	if err != nil {
		return Drift{Verdict: "unresolvable", Base: s.Meta.Base, Head: head}
	}
	if base == head {
		return Drift{Verdict: "clean", Base: base, Head: head}
	}
	changed, _ := git("diff", "--name-only", base, head)
	files := strings.Fields(changed)
	conflicts := map[string][]string{}
	for _, t := range s.Tasks {
		scope := append(append([]string{}, t.WriteSet...), t.ReadSet.Subjects...)
		for _, f := range files {
			for _, g := range scope {
				p := strings.TrimPrefix(globPrefix(g), "file:")
				if p != "" && strings.HasPrefix(f, p) {
					conflicts[t.ID] = append(conflicts[t.ID], f)
					break
				}
			}
		}
	}
	if len(conflicts) > 0 {
		return Drift{Verdict: "conflicting", Base: base, Head: head, Conflicts: conflicts}
	}
	return Drift{Verdict: "clean_drift", Base: base, Head: head}
}

// Explain renders the compile report (§4.4): every command that will run, every
// path that will be written, every memory record that will be created. This is
// the human review artifact (§4.6 control 5).
func Explain(s *Spec, d Drift) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	p("  ok  format %s, %d tasks, %d knowledge records, %d prose chunks",
		s.Meta.Theorm, len(s.Tasks), len(s.Knowledge), len(s.Prose))
	p("  ok  DAG acyclic, entry (%s), exit (%s)",
		strings.Join(entries(s.Tasks), ", "), strings.Join(exits(s.Tasks), ", "))

	switch d.Verdict {
	case "clean":
		p("  ok  base %s resolves, HEAD is %s, no drift", d.Base, d.Head)
	case "clean_drift":
		p("  ok  base %s, HEAD is %s, drift does not touch any task's scope", d.Base, d.Head)
	case "conflicting":
		p("  !!  conflicting drift: base %s, HEAD %s (§4.5, needs --accept-drift)", d.Base, d.Head)
		for _, id := range sortedKeys(d.Conflicts) {
			p("        %s: %s", id, strings.Join(d.Conflicts[id], " "))
		}
	case "unresolvable":
		p("  !!  base %s does not resolve. Re-anchor the spec", d.Base)
	case "no_repo":
		p("  --  not a git repository, drift not checked")
	}

	if len(s.Meta.Policy.AllowTools) > 0 {
		for _, t := range s.Tasks {
			p("  ok  capability clamp: %s %d tools -> %d (spec policy narrows)",
				t.Role, len(roleTools[t.Role]), len(t.Tools))
		}
	}
	maxCtx := 0
	for _, t := range s.Tasks {
		if t.EstContextTokens > maxCtx {
			maxCtx = t.EstContextTokens
		}
	}
	p("  ok  context feasibility: max %d est vs %d available in quality mode", maxCtx, CtxQuality)

	waves := Waves(s.Tasks)
	p("")
	p("  Projected execution:")
	secs := 0
	for i, w := range waves {
		kind := "sequential"
		if w.Degree > 1 {
			kind = fmt.Sprintf("parallel x%d", w.Degree)
		}
		p("    wave %d  %-16s %-12s %-12s %-11s ~%dk ctx   %s",
			i+1, "["+strings.Join(w.Tasks, ", ")+"]", kind, w.Class, w.Mode+" mode", w.MaxCtx/1000, w.Reason)
		secs += 40 * waveSteps(w, s.Tasks)
	}

	p("")
	p("  Commands that will run:  %s", strings.Join(commands(s), ", "))
	p("  Paths that will be written:  %s", strings.Join(paths(s), ", "))
	p("  Memory records that will be written:  %d from theorm:knowledge, %d prose chunks at confidence 0.6",
		len(s.Knowledge), len(s.Prose))
	p("")
	p("  Estimated: %d tasks, %d waves, ~%d min. No cloud calls.", len(s.Tasks), len(waves), secs/60)
	return b.String()
}

func waveSteps(w Wave, tasks []Task) int {
	byID, max := byIDs(tasks), 0
	for _, id := range w.Tasks {
		if n := estSteps(byID[id]); n > max {
			max = n
		}
	}
	return max
}

func commands(s *Spec) []string {
	set := map[string]bool{}
	for _, t := range s.Tasks {
		for _, a := range t.Acceptance {
			if a.Kind == "cmd" {
				set[a.Run] = true
			}
		}
		for _, g := range t.Gates {
			set["gate:"+g] = true
		}
	}
	return sortedKeys(set)
}

func paths(s *Spec) []string {
	set := map[string]bool{}
	for _, t := range s.Tasks {
		for _, g := range t.WriteSet {
			set[g] = true
		}
	}
	if len(set) == 0 {
		return []string{"(none, read-only run)"}
	}
	return sortedKeys(set)
}

func entries(tasks []Task) []string {
	var out []string
	for _, t := range tasks {
		if len(t.DependsOn) == 0 {
			out = append(out, t.ID)
		}
	}
	return out
}

func exits(tasks []Task) []string {
	depended := map[string]bool{}
	for _, t := range tasks {
		for _, d := range t.DependsOn {
			depended[d] = true
		}
	}
	var out []string
	for _, t := range tasks {
		if !depended[t.ID] {
			out = append(out, t.ID)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
