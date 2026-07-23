package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Filesystem tools. Every path an agent supplies goes through resolve, which is
// the only place that turns a model's string into a real path (§8.7).

const (
	maxFileBytes = 1 << 20 // a file bigger than this is read in windows
	maxGrepHits  = 200
	maxDirNames  = 500
)

// resolve joins a task-supplied path to the worktree root and refuses anything
// that leaves it: absolute paths, .. traversal, and symlinks pointing outside.
func resolve(env Env, rel string) (string, error) {
	if env.Dir == "" {
		return "", fmt.Errorf("no working directory for this task")
	}
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.Contains(rel, ":") {
		return "", fmt.Errorf("path %q must be relative to the repository root", rel)
	}
	root, err := filepath.EvalSymlinks(env.Dir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	// EvalSymlinks fails on a path that does not exist yet, so check the
	// deepest existing ancestor instead of giving up.
	probe := full
	for {
		if real, err := filepath.EvalSymlinks(probe); err == nil {
			rp, err := filepath.Rel(root, real)
			if err != nil || rp == ".." || strings.HasPrefix(rp, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("path %q escapes the repository root", rel)
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	if rp, err := filepath.Rel(root, full); err != nil || strings.HasPrefix(rp, "..") {
		return "", fmt.Errorf("path %q escapes the repository root", rel)
	}
	return full, nil
}

// --- read_file ---

type readFile struct{}

func (readFile) Name() string { return "read_file" }
func (readFile) Description() string {
	return "Read a file from the repository, optionally a line range. Paths are relative to the repository root."
}
func (readFile) Annotations() Annotations { return Annotations{ReadOnly: true, Idempotent: true} }
func (readFile) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "path":       {"type": "string"},
	    "start_line": {"type": "integer", "minimum": 1},
	    "end_line":   {"type": "integer", "minimum": 1}
	  },
	  "required": ["path"]
	}`)
}

func (readFile) Execute(ctx context.Context, args json.RawMessage, env Env) (Output, error) {
	var in struct {
		Path  string `json:"path"`
		Start int    `json:"start_line"`
		End   int    `json:"end_line"`
	}
	if err := decode(args, &in); err != nil {
		return Output{}, err
	}
	full, err := resolve(env, in.Path)
	if err != nil {
		return Output{}, err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		// Report the path the agent used, not the absolute one on this machine.
		return Output{}, fmt.Errorf("cannot read %q: %s", in.Path, reason(err))
	}
	if len(b) > maxFileBytes && in.Start == 0 {
		return Output{}, fmt.Errorf("%s is %d bytes: read it with start_line and end_line", in.Path, len(b))
	}
	text := string(b)
	if in.Start > 0 {
		lines := strings.Split(text, "\n")
		end := in.End
		if end == 0 || end > len(lines) {
			end = len(lines)
		}
		if in.Start > len(lines) {
			return Output{}, fmt.Errorf("%s has %d lines", in.Path, len(lines))
		}
		text = strings.Join(lines[in.Start-1:end], "\n")
	}
	return Output{Content: text, Kind: "file_content", MediaType: "text/plain"}, nil
}

// --- list_dir ---

type listDir struct{}

func (listDir) Name() string { return "list_dir" }
func (listDir) Description() string {
	return "List the entries of a directory. Directories end with a slash. Ignores .git and node_modules."
}
func (listDir) Annotations() Annotations { return Annotations{ReadOnly: true, Idempotent: true} }
func (listDir) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {"path": {"type": "string", "description": "relative directory, default the repository root"}}
	}`)
}

func (listDir) Execute(ctx context.Context, args json.RawMessage, env Env) (Output, error) {
	var in struct {
		Path string `json:"path"`
	}
	if len(args) > 0 {
		if err := decode(args, &in); err != nil {
			return Output{}, err
		}
	}
	full, err := resolve(env, in.Path)
	if err != nil {
		return Output{}, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return Output{}, fmt.Errorf("cannot list %q: %s. Paths are relative to the repository root",
			cmp.Or(in.Path, "."), reason(err))
	}
	var names []string
	for _, e := range entries {
		if skipDir(e.Name()) {
			continue
		}
		if e.IsDir() {
			names = append(names, e.Name()+"/")
		} else {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) > maxDirNames {
		names = append(names[:maxDirNames], fmt.Sprintf("[%d more]", len(names)-maxDirNames))
	}
	return Output{Content: strings.Join(names, "\n"), Kind: "dir_listing", MediaType: "text/plain"}, nil
}

// --- grep ---

type grepTool struct{}

func (grepTool) Name() string { return "grep" }
func (grepTool) Description() string {
	return "Search the repository for a regular expression and return matching lines with file and line number."
}
func (grepTool) Annotations() Annotations { return Annotations{ReadOnly: true, Idempotent: true} }
func (grepTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "pattern": {"type": "string", "description": "Go regular expression"},
	    "path":    {"type": "string", "description": "relative directory to search, default the repository root"},
	    "glob":    {"type": "string", "description": "filename filter, e.g. *.go"}
	  },
	  "required": ["pattern"]
	}`)
}

func (grepTool) Execute(ctx context.Context, args json.RawMessage, env Env) (Output, error) {
	var in struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Glob    string `json:"glob"`
	}
	if err := decode(args, &in); err != nil {
		return Output{}, err
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return Output{}, fmt.Errorf("bad pattern: %w", err)
	}
	root, err := resolve(env, in.Path)
	if err != nil {
		return Output{}, err
	}
	base, err := filepath.EvalSymlinks(env.Dir)
	if err != nil {
		return Output{}, err
	}

	var out []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return nil // unreadable entries are skipped, not fatal
		case ctx.Err() != nil:
			return ctx.Err()
		case d.IsDir():
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		case len(out) >= maxGrepHits:
			return filepath.SkipAll
		}
		if in.Glob != "" {
			if ok, _ := filepath.Match(in.Glob, d.Name()); !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileBytes {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil || isBinary(b) {
			return nil
		}
		rel, _ := filepath.Rel(base, p)
		rel = filepath.ToSlash(rel)
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) {
				out = append(out, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
				if len(out) >= maxGrepHits {
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return Output{}, err
	}
	if len(out) == 0 {
		return Output{Content: "no matches", Kind: "tool_receipt"}, nil
	}
	return Output{Content: strings.Join(out, "\n"), Kind: "grep_hits", MediaType: "text/plain"}, nil
}

// reason strips the absolute path out of an os error.
func reason(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "no such file or directory"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	return "not readable"
}

func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".next", "vendor", "__pycache__", ".venv":
		return true
	}
	return false
}

func isBinary(b []byte) bool {
	if len(b) > 8000 {
		b = b[:8000]
	}
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}
