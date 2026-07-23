package store

import "testing"

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
