package store

import (
	"slices"
	"testing"
)

// The "file:" prefix must match exactly what indexing wrote, or a changed file's
// entity record is never found and never goes stale.
func TestStaleSubjects(t *testing.T) {
	got := staleSubjects([]string{"internal/auth/token.go", "README.md"})
	want := []string{"file:internal/auth/token.go", "file:README.md"}
	if !slices.Equal(got, want) {
		t.Errorf("staleSubjects = %v, want %v", got, want)
	}
}

// A malformed vector literal breaks every L3 insert silently, so pin the format
// pgvector expects: [f1,f2,...] with no spaces.
func TestVectorFormat(t *testing.T) {
	if got := vector([]float32{0.5, -1, 0}); got != "[0.5,-1,0]" {
		t.Errorf("vector = %q", got)
	}
	if got := vector(nil); got != "[]" {
		t.Errorf("empty vector = %q", got)
	}
}
