package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// L2 (§5.5): large content never enters a prompt by value. It lands on disk,
// content-addressed, and enters context as a handle plus a summary.

type Artifact struct {
	ID      string
	Kind    string
	Size    int
	Summary string
	Path    string
}

type ArtifactInput struct {
	RunID     string
	TaskID    string
	Kind      string // test_output|diff|screenshot|a11y_snapshot|network_log|console_log|file_content
	MediaType string
	Content   []byte
}

func ArtifactDir() string {
	if v := os.Getenv("THEORM_ARTIFACTS"); v != "" {
		return v
	}
	return "artifacts"
}

func (s *Store) PutArtifact(ctx context.Context, in ArtifactInput) (Artifact, error) {
	if in.MediaType == "" {
		in.MediaType = "text/plain"
	}
	content := in.Content
	if strings.HasPrefix(in.MediaType, "text/") || in.MediaType == "application/json" {
		// F18: redact before storing. An artifact on disk with a live token in
		// it is an incident, and redacting at display time is too late.
		content = Redact(content)
	}
	sum := sha256.Sum256(content)
	id := hex.EncodeToString(sum[:])

	dir := filepath.Join(ArtifactDir(), in.RunID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Artifact{}, err
	}
	path := filepath.Join(dir, "sha256-"+id+ext(in.Kind, in.MediaType))
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return Artifact{}, err
	}

	a := Artifact{ID: id, Kind: in.Kind, Size: len(content), Summary: Summarise(in.Kind, content), Path: path}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO artifacts (id, run_id, task_id, kind, media_type, size_bytes, path, summary)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (id) DO NOTHING`,
		id, in.RunID, nz(in.TaskID), in.Kind, in.MediaType, len(content), path, a.Summary); err != nil {
		return Artifact{}, err
	}
	return a, nil
}

// ReadArtifact returns a window of an artifact's content. An agent that calls
// it has chosen to spend its own context budget, which is the right place for
// that decision.
func (s *Store) ReadArtifact(ctx context.Context, id string, offset, limit int) (string, error) {
	var path string
	if err := s.pool.QueryRow(ctx, `SELECT path FROM artifacts WHERE id = $1`, id).Scan(&path); err != nil {
		if isNoRows(err) {
			return "", fmt.Errorf("no artifact %s", id)
		}
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if offset > len(b) {
		return "", nil
	}
	b = b[offset:]
	if limit > 0 && limit < len(b) {
		b = b[:limit]
	}
	return string(b), nil
}

// Summarise is deterministic on purpose (§5.5): no model call, no cost, and
// nothing to hallucinate. Unstructured kinds fall back to a head excerpt.
func Summarise(kind string, content []byte) string {
	text := string(content)
	lines := strings.Split(text, "\n")
	var s string
	switch kind {
	case "test_output":
		fails := regexp.MustCompile(`(?m)^\s*--- FAIL: (\S+)`).FindAllStringSubmatch(text, -1)
		first := firstMatch(text, `(?m)^\s*\S+\.go:\d+:.*$`)
		if len(fails) == 0 {
			s = fmt.Sprintf("tests passed, %d lines", len(lines))
		} else {
			names := make([]string, 0, 3)
			for _, m := range fails[:min(3, len(fails))] {
				names = append(names, m[1])
			}
			s = fmt.Sprintf("%d failing test(s): %s. first error: %s",
				len(fails), strings.Join(names, ", "), strings.TrimSpace(first))
		}
	case "diff":
		files := regexp.MustCompile(`(?m)^\+\+\+ b/(.+)$`).FindAllStringSubmatch(text, -1)
		add, del := 0, 0
		for _, l := range lines {
			switch {
			case strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++"):
				add++
			case strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---"):
				del++
			}
		}
		names := make([]string, 0, len(files))
		for _, m := range files[:min(4, len(files))] {
			names = append(names, m[1])
		}
		s = fmt.Sprintf("%d file(s) changed, +%d -%d: %s", len(files), add, del, strings.Join(names, " "))
	case "console_log":
		errs := strings.Count(strings.ToLower(text), "error")
		s = fmt.Sprintf("%d console lines, %d mentioning error", len(lines), errs)
	case "claims":
		subjects := make([]string, 0, 3)
		for _, l := range lines[:min(3, len(lines))] {
			if _, after, ok := strings.Cut(l, "] "); ok {
				subjects = append(subjects, strings.Fields(after)[0])
			}
		}
		s = fmt.Sprintf("%d claims, about: %s", len(lines), strings.Join(subjects, ", "))
	case "command_output":
		last := strings.TrimSpace(lines[max(0, len(lines)-2)])
		s = fmt.Sprintf("%s, %d lines, ends: %s", strings.TrimPrefix(lines[0], "$ "), len(lines), last)
	default:
		head := strings.TrimSpace(strings.Join(lines[:min(2, len(lines))], " "))
		s = fmt.Sprintf("%s, %d bytes: %s", kind, len(content), head)
	}
	if len(s) > 300 {
		s = s[:297] + "..."
	}
	return s
}

// secretPatterns is intentionally short: the common shapes, not a scanner.
// ponytail: add patterns when a real leak gets past it, not speculatively.
var secretPatterns = regexp.MustCompile(
	`(?i)(AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9]{20,}|` +
		`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}|` +
		`\b(?:api[_-]?key|secret|password|token)\b["']?\s*[:=]\s*["']?[^\s"']{8,})`)

func Redact(b []byte) []byte {
	return secretPatterns.ReplaceAll(b, []byte("[REDACTED]"))
}

func firstMatch(text, pattern string) string {
	if m := regexp.MustCompile(pattern).FindString(text); m != "" {
		return m
	}
	return "none"
}

func ext(kind, mediaType string) string {
	switch {
	case kind == "diff":
		return ".diff"
	case kind == "screenshot":
		return ".png"
	case mediaType == "application/json":
		return ".json"
	case strings.HasPrefix(mediaType, "text/"):
		return ".txt"
	}
	return ".bin"
}
