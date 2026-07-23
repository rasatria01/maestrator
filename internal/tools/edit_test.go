package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInWriteSet(t *testing.T) {
	ws := []string{"internal/auth/**", "docs/api.md", "cmd/*/main.go"}
	yes := []string{"internal/auth/token.go", "internal/auth/sub/deep.go", "docs/api.md", "cmd/theorm/main.go"}
	no := []string{"internal/store/store.go", "docs/other.md", "cmd/theorm/aux.go", "cmd/a/b/main.go", "README.md"}
	for _, p := range yes {
		if !inWriteSet(ws, p) {
			t.Errorf("%q should be in %v", p, ws)
		}
	}
	for _, p := range no {
		if inWriteSet(ws, p) {
			t.Errorf("%q should be outside %v", p, ws)
		}
	}
}

// The coder may write only where the task said it could: resolve stops escapes,
// inWriteSet stops in-tree paths the contract never granted.
func TestWriteFileRespectsWriteSet(t *testing.T) {
	dir := t.TempDir()
	e := Env{Dir: dir, WriteSet: []string{"internal/auth/**"},
		Allowed: []string{"write_file", "str_replace"}}
	r := New(Native()...)
	ctx := t.Context()

	if res := call(r, ctx, e, "write_file", map[string]any{
		"path": "internal/auth/new.go", "content": "package auth\n"}); res.Error != "" {
		t.Fatalf("in-set write refused: %s", res.Error)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "internal", "auth", "new.go")); err != nil || string(b) != "package auth\n" {
		t.Fatalf("file not written: %v %q", err, b)
	}
	for _, path := range []string{"internal/store/store.go", "README.md", "../escape.go"} {
		if res := call(r, ctx, e, "write_file", map[string]any{"path": path, "content": "x"}); res.Error == "" {
			t.Errorf("write_file allowed %q, outside the write set", path)
		}
	}
}

func TestStrReplaceUniqueness(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	must := os.WriteFile(filepath.Join(dir, "a", "f.go"), []byte("x := 1\ny := 1\nx := 1\n"), 0o644)
	if must != nil {
		t.Fatal(must)
	}
	e := Env{Dir: dir, WriteSet: []string{"a/**"}, Allowed: []string{"str_replace"}}
	r := New(Native()...)
	ctx := t.Context()

	if res := call(r, ctx, e, "str_replace", map[string]any{
		"path": "a/f.go", "old_str": "x := 1", "new_str": "x := 2"}); res.Error == "" {
		t.Error("str_replace accepted an ambiguous (2x) old_str")
	}
	if res := call(r, ctx, e, "str_replace", map[string]any{
		"path": "a/f.go", "old_str": "z := 9", "new_str": "z := 0"}); res.Error == "" {
		t.Error("str_replace accepted an old_str that does not exist")
	}
	if res := call(r, ctx, e, "str_replace", map[string]any{
		"path": "a/f.go", "old_str": "y := 1", "new_str": "y := 2"}); res.Error != "" {
		t.Fatalf("unique replace refused: %s", res.Error)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a", "f.go"))
	if string(b) != "x := 1\ny := 2\nx := 1\n" {
		t.Errorf("replace wrote %q", b)
	}
}

func TestGitCommitStagesOnlyWriteSet(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	ctx := t.Context()
	for _, args := range [][]string{{"init", "-q"}, {"commit", "--allow-empty", "-qm", "root"}} {
		if out, err := git(ctx, dir, append(agentIdentity, args...)...); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	e := Env{Dir: dir, WriteSet: []string{"src/**"},
		Allowed: []string{"write_file", "git_commit"}}
	r := New(Native()...)

	// An out-of-set file exists on disk but must never be committed.
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("keep out"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := call(r, ctx, e, "write_file", map[string]any{"path": "src/app.go", "content": "package app\n"}); res.Error != "" {
		t.Fatal(res.Error)
	}
	if res := call(r, ctx, e, "git_commit", map[string]any{"message": "add app"}); res.Error != "" {
		t.Fatalf("git_commit refused: %s", res.Error)
	}

	tracked, _ := git(ctx, dir, "ls-files")
	if !strings.Contains(tracked, "src/app.go") {
		t.Errorf("src/app.go was not committed: %q", tracked)
	}
	if strings.Contains(tracked, "secret.txt") {
		t.Error("git_commit staged a file outside the write set")
	}
	// Nothing changed since: a second commit has nothing in-set to do.
	if res := call(r, ctx, e, "git_commit", map[string]any{"message": "again"}); res.Error == "" {
		t.Error("git_commit committed with no write-set changes")
	}
}
