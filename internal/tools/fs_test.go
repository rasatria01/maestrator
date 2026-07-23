package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repoEnv(t *testing.T) Env {
	t.Helper()
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "internal", "auth"), 0o755))
	must(os.MkdirAll(filepath.Join(dir, "node_modules", "junk"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "internal", "auth", "token.go"),
		[]byte("package auth\n\nfunc NewTokenService() {}\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "node_modules", "junk", "index.js"),
		[]byte("function NewTokenService() {}\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "README.md"), []byte("# repo\n"), 0o644))
	return Env{Dir: dir, Allowed: []string{"read_file", "list_dir", "grep"}}
}

// §8.7: every path an agent supplies is resolved against the worktree root, and
// anything that leaves it is refused.
func TestPathsCannotEscapeTheWorktree(t *testing.T) {
	e := repoEnv(t)
	secret := filepath.Join(filepath.Dir(e.Dir), "outside.txt")
	if err := os.WriteFile(secret, []byte("not yours"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(secret) })

	r := New(Native()...)
	for _, path := range []string{
		"../outside.txt",
		"internal/../../outside.txt",
		secret,
		"/etc/passwd",
	} {
		res := call(r, t.Context(), e, "read_file", map[string]any{"path": path})
		if res.Error == "" {
			t.Errorf("read_file accepted %q: %q", path, first(res.Summary, 40))
		}
	}
	// The legitimate case still works.
	if res := call(r, t.Context(), e, "read_file", map[string]any{"path": "internal/auth/token.go"}); res.Error != "" {
		t.Fatalf("a path inside the tree was rejected: %s", res.Error)
	}
}

func TestSymlinkOutOfTheTreeIsRefused(t *testing.T) {
	e := repoEnv(t)
	target := filepath.Join(filepath.Dir(e.Dir), "linked.txt")
	if err := os.WriteFile(target, []byte("not yours"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(target) })
	if err := os.Symlink(target, filepath.Join(e.Dir, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err) // Windows without developer mode
	}
	if res := call(New(Native()...), t.Context(), e, "read_file", map[string]any{"path": "escape.txt"}); res.Error == "" {
		t.Errorf("a symlink out of the tree was followed: %q", res.Summary)
	}
}

func TestReadListGrep(t *testing.T) {
	e := repoEnv(t)
	r := New(Native()...)
	ctx := t.Context()

	if res := call(r, ctx, e, "list_dir", map[string]any{}); !strings.Contains(res.Summary, "internal/") ||
		strings.Contains(res.Summary, "node_modules") {
		t.Errorf("list_dir: %q", res.Summary)
	}
	res := call(r, ctx, e, "read_file", map[string]any{"path": "internal/auth/token.go", "start_line": 3, "end_line": 3})
	if strings.TrimSpace(res.Summary) != "func NewTokenService() {}" {
		t.Errorf("line range read returned %q", res.Summary)
	}
	res = call(r, ctx, e, "grep", map[string]any{"pattern": "NewTokenService"})
	if !strings.Contains(res.Summary, "internal/auth/token.go:3:") {
		t.Errorf("grep missed the definition: %q", res.Summary)
	}
	if strings.Contains(res.Summary, "node_modules") {
		t.Error("grep walked into node_modules")
	}
	if res := call(r, ctx, e, "grep", map[string]any{"pattern": "func ("}); res.Error == "" {
		t.Error("an invalid regular expression was accepted")
	}
}
