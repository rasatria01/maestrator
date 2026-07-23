package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The coder's hands (§8.2). Every write is bounded twice: resolve keeps it
// inside the worktree, inWriteSet keeps it inside what the task declared it may
// touch. A model cannot widen its own write set by asking nicely.

// inWriteSet reports whether rel (repo-relative, slash-separated) is covered by
// any glob in the task's write set. Globs use ** to cross directory boundaries
// and filepath.Match syntax within a segment, matching the spec's write_set.
// A path outside every glob is refused, so under-matching a legitimate write is
// safer here than allowing a stray one.
func inWriteSet(writeSet []string, rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, g := range writeSet {
		if matchGlob(filepath.ToSlash(g), rel) {
			return true
		}
	}
	return false
}

func matchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true // ** at the end matches everything below
			}
			for i := 0; i <= len(name); i++ { // and zero or more segments here
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := filepath.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// resolveWrite is resolve plus the write-set gate: the two checks every mutating
// tool shares.
func resolveWrite(env Env, rel string) (string, error) {
	full, err := resolve(env, rel)
	if err != nil {
		return "", err
	}
	if !inWriteSet(env.WriteSet, filepath.ToSlash(rel)) {
		return "", fmt.Errorf("%q is outside this task's write set %v; you may not modify it", rel, env.WriteSet)
	}
	return full, nil
}

// --- write_file ---

type writeFile struct{}

func (writeFile) Name() string { return "write_file" }
func (writeFile) Description() string {
	return "Create or overwrite a file with the given content. The path must be inside the task's " +
		"write set and relative to the repository root. Overwrites the whole file; use str_replace for an edit."
}
func (writeFile) Annotations() Annotations { return Annotations{Destructive: true} }
func (writeFile) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "path":    {"type": "string"},
	    "content": {"type": "string"}
	  },
	  "required": ["path", "content"]
	}`)
}

func (writeFile) Execute(ctx context.Context, args json.RawMessage, env Env) (Output, error) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(args, &in); err != nil {
		return Output{}, err
	}
	full, err := resolveWrite(env, in.Path)
	if err != nil {
		return Output{}, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return Output{}, fmt.Errorf("cannot create parent of %q: %s", in.Path, reason(err))
	}
	if err := os.WriteFile(full, []byte(in.Content), 0o644); err != nil {
		return Output{}, fmt.Errorf("cannot write %q: %s", in.Path, reason(err))
	}
	return Output{
		Content: fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path),
		Kind:    "tool_receipt", Summary: fmt.Sprintf("wrote %s (%d bytes)", in.Path, len(in.Content)),
	}, nil
}

// --- str_replace ---

type strReplace struct{}

func (strReplace) Name() string { return "str_replace" }
func (strReplace) Description() string {
	return "Replace an exact, unique block of text in a file. old_str must appear exactly once; " +
		"if it does not, add surrounding lines until it is unique. Paths must be inside the task's write set."
}
func (strReplace) Annotations() Annotations { return Annotations{Destructive: true} }
func (strReplace) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "path":    {"type": "string"},
	    "old_str": {"type": "string"},
	    "new_str": {"type": "string"}
	  },
	  "required": ["path", "old_str", "new_str"]
	}`)
}

func (strReplace) Execute(ctx context.Context, args json.RawMessage, env Env) (Output, error) {
	var in struct {
		Path string `json:"path"`
		Old  string `json:"old_str"`
		New  string `json:"new_str"`
	}
	if err := decode(args, &in); err != nil {
		return Output{}, err
	}
	if in.Old == "" {
		return Output{}, fmt.Errorf("old_str is empty: use write_file to create a file")
	}
	full, err := resolveWrite(env, in.Path)
	if err != nil {
		return Output{}, err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return Output{}, fmt.Errorf("cannot read %q: %s", in.Path, reason(err))
	}
	switch n := bytes.Count(b, []byte(in.Old)); {
	case n == 0:
		return Output{}, fmt.Errorf("old_str not found in %s: it must match exactly, whitespace included", in.Path)
	case n > 1:
		return Output{}, fmt.Errorf("old_str appears %d times in %s: add surrounding lines to make it unique", n, in.Path)
	}
	out := bytes.Replace(b, []byte(in.Old), []byte(in.New), 1)
	if err := os.WriteFile(full, out, 0o644); err != nil {
		return Output{}, fmt.Errorf("cannot write %q: %s", in.Path, reason(err))
	}
	return Output{
		Content: fmt.Sprintf("replaced 1 block in %s (%+d bytes)", in.Path, len(out)-len(b)),
		Kind:    "tool_receipt", Summary: fmt.Sprintf("edited %s", in.Path),
	}, nil
}

// --- git_commit ---

// agentIdentity attributes orchestrator commits to a stable bot rather than
// whoever configured git on the box. Passed per-command so it never mutates the
// repo's config.
// ponytail: one identity for all agents. Split per-role only if the history
// needs to show which role committed, which git trailers could do without this.
var agentIdentity = []string{"-c", "user.name=theorm-agent", "-c", "user.email=agent@theorm.local"}

type gitCommit struct{}

func (gitCommit) Name() string { return "git_commit" }
func (gitCommit) Description() string {
	return "Stage the files in this task's write set and commit them with the given message. " +
		"Only write-set paths are committed; files you did not change are left alone."
}
func (gitCommit) Annotations() Annotations { return Annotations{Destructive: true} }
func (gitCommit) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {"message": {"type": "string", "description": "commit message, imperative mood"}},
	  "required": ["message"]
	}`)
}

func (gitCommit) Execute(ctx context.Context, args json.RawMessage, env Env) (Output, error) {
	var in struct {
		Message string `json:"message"`
	}
	if err := decode(args, &in); err != nil {
		return Output{}, err
	}
	if strings.TrimSpace(in.Message) == "" {
		return Output{}, fmt.Errorf("commit message is empty")
	}
	if env.Dir == "" {
		return Output{}, fmt.Errorf("no working directory for this task")
	}
	if len(env.WriteSet) == 0 {
		return Output{}, fmt.Errorf("this task declares no write set, so there is nothing it may commit")
	}

	// Stage only the write set. :(glob) makes git's pathspec honour ** the same
	// way the write-set globs do, so what was allowed to be written is exactly
	// what gets staged.
	add := []string{"add", "--"}
	for _, g := range env.WriteSet {
		add = append(add, ":(glob)"+filepath.ToSlash(g))
	}
	if out, err := git(ctx, env.Dir, add...); err != nil {
		return Output{}, fmt.Errorf("git add: %s", out)
	}
	// Nothing staged means nothing in the write set changed.
	if _, err := git(ctx, env.Dir, "diff", "--cached", "--quiet"); err == nil {
		return Output{}, fmt.Errorf("no changes in the write set to commit")
	}
	if out, err := git(ctx, env.Dir, append(agentIdentity, "commit", "-m", in.Message)...); err != nil {
		return Output{}, fmt.Errorf("git commit: %s", out)
	}
	stat, _ := git(ctx, env.Dir, "show", "--stat", "--oneline", "HEAD")
	return Output{Content: stat, Kind: "diff", MediaType: "text/plain",
		Summary: firstLine(stat)}, nil
}

// git runs one git command with no shell and the same secret-stripped env as
// run_cmd. It returns combined output so a failure carries git's own message.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = filterEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
