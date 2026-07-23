package memory

import (
	"strings"
	"testing"

	"github.com/rasatria01/theorm/internal/spec"
)

// specRecords is the whole B5 mapping: knowledge blocks keep their confidence,
// prose lands as weaker semantic memory, and both carry the spec provenance that
// makes retire-by-hash exact.
func TestSpecRecords(t *testing.T) {
	s := &spec.Spec{
		Path:   "/x/.theorm/specs/0001-auth.md",
		SHA256: "abc123",
		Meta:   spec.Meta{Repo: "github.com/x/y"},
		Knowledge: []spec.Knowledge{
			{MemType: "procedural", Subject: "auth", Content: "rotate keys via config", Confidence: 0.9},
			{Subject: "db", Content: "postgres on 55432"}, // no mem_type, no confidence: defaults apply
			{Subject: "empty", Content: "   "},             // dropped
		},
		Prose: []spec.Chunk{
			{Heading: "Overview", Text: "The system does things.", Line: 3},
			{Text: "", Line: 9}, // dropped
		},
	}
	recs := specRecords(s)
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3 (2 knowledge + 1 prose)", len(recs))
	}
	for _, r := range recs {
		if r.VerifiedBy != "spec:abc123" {
			t.Errorf("provenance = %q, want spec:abc123", r.VerifiedBy)
		}
		if r.RepoID != "github.com/x/y" {
			t.Errorf("repo = %q", r.RepoID)
		}
	}
	// Knowledge defaults.
	if recs[0].MemType != "procedural" || recs[0].Confidence != float64(float32(0.9)) {
		t.Errorf("explicit knowledge: %+v", recs[0])
	}
	if recs[1].MemType != "semantic" || recs[1].Confidence != 0.6 {
		t.Errorf("defaulted knowledge: mem_type=%q conf=%v", recs[1].MemType, recs[1].Confidence)
	}
	// Prose: weaker, semantic, subject from the heading.
	prose := recs[2]
	if prose.MemType != "semantic" || prose.Confidence != proseConfidence {
		t.Errorf("prose: mem_type=%q conf=%v", prose.MemType, prose.Confidence)
	}
	if !strings.Contains(prose.Subject, "#Overview") {
		t.Errorf("prose subject = %q, want a heading anchor", prose.Subject)
	}
}
