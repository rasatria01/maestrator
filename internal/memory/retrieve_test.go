package memory

import (
	"testing"

	"github.com/rasatria01/theorm/internal/store"
)

// Fusion returns candidates in an arbitrary order; the rerank score is what
// orders the final list, and the top-k cap must not drop a high-scoring one.
func TestTopHits(t *testing.T) {
	cands := []store.Candidate{
		{Subject: "file:a", Content: "a"},
		{Subject: "file:b", Content: "b"},
		{Subject: "file:c", Content: "c"},
	}
	got := topHits(cands, []float64{0.1, 0.9, 0.5}, 2)
	if len(got) != 2 {
		t.Fatalf("top-k=2 returned %d", len(got))
	}
	if got[0].Subject != "file:b" || got[1].Subject != "file:c" {
		t.Errorf("wrong order: %s then %s", got[0].Subject, got[1].Subject)
	}
	if all := topHits(cands, []float64{0.1, 0.9, 0.5}, 0); len(all) != 3 {
		t.Errorf("k=0 should return all, got %d", len(all))
	}
}
