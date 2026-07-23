package memory

import (
	"testing"

	"github.com/rasatria01/theorm/internal/store"
)

// The §5.8 promotion rules live in decide, so this is where they are pinned:
// the cap, the near-duplicate bump, the contradiction park, and their order.
func TestDecide(t *testing.T) {
	dup := &store.Neighbor{Cosine: 0.95}
	notDup := &store.Neighbor{Cosine: 0.5}
	stronger := &store.Neighbor{Confidence: 0.9}
	weaker := &store.Neighbor{Confidence: 0.2}

	cases := []struct {
		name     string
		promoted int
		conf     float64
		near     *store.Neighbor
		conflict *store.Neighbor
		want     verdict
	}{
		{"fresh claim promotes", 0, 0.8, nil, nil, vPromote},
		{"near-duplicate bumps", 0, 0.8, dup, nil, vBump},
		{"distant neighbour still promotes", 0, 0.8, notDup, nil, vPromote},
		{"stronger contradiction parks", 0, 0.5, nil, stronger, vPark},
		{"weaker contradiction does not park", 0, 0.5, nil, weaker, vPromote},
		{"over the cap skips", maxPerRun, 0.8, nil, nil, vSkip},
		{"contradiction beats the cap", maxPerRun, 0.5, nil, stronger, vPark},
		{"duplicate beats the cap", maxPerRun, 0.8, dup, nil, vBump},
		{"contradiction beats a duplicate", 0, 0.5, dup, stronger, vPark},
	}
	for _, c := range cases {
		if got := decide(c.promoted, c.conf, c.near, c.conflict); got != c.want {
			t.Errorf("%s: decide = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestMemTypeFor(t *testing.T) {
	for kind, want := range map[string]string{
		"fact": "semantic", "decision": "semantic", "constraint": "semantic",
		"finding": "semantic", "failure": "procedural", "question": "", "handoff": "",
	} {
		if got := memTypeFor(kind); got != want {
			t.Errorf("memTypeFor(%q) = %q, want %q", kind, got, want)
		}
	}
}
