package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rasatria01/theorm/internal/spec"
	"github.com/rasatria01/theorm/internal/store"
)

// Ingester writes a spec's knowledge into L3 (B5). Tagged `theorm:knowledge`
// blocks land at their stated confidence; untagged prose lands as semantic
// memory at reduced confidence, because silence is not consent (§4.3).
type Ingester struct {
	Store *store.Store
	Embed *Embedder
}

const proseConfidence = 0.4 // untagged prose is weaker than an explicit knowledge block

// IngestSpec writes the spec's knowledge records under its provenance and
// replaces any prior records from the same spec version. Returns how many were
// written.
func (ix *Ingester) IngestSpec(ctx context.Context, s *spec.Spec) (int, error) {
	recs := specRecords(s)
	if len(recs) == 0 {
		return 0, nil
	}
	texts := make([]string, len(recs))
	for i, r := range recs {
		texts[i] = r.Content
	}
	vecs, err := ix.Embed.Embed(ctx, texts)
	if err != nil {
		return 0, err
	}
	for i := range recs {
		recs[i].Embedding = vecs[i]
	}
	if err := ix.Store.ReplaceSpecKnowledge(ctx, s.Meta.Repo, provenance(s), recs); err != nil {
		return 0, err
	}
	return len(recs), nil
}

// provenance is the verified_by that ties a record to the exact spec version it
// came from, so retiring by sha removes exactly those.
func provenance(s *spec.Spec) string { return "spec:" + s.SHA256 }

// specRecords turns a spec's knowledge blocks and prose chunks into L3 records.
// Pure: no embedding, no store, so the mapping is testable on its own.
func specRecords(s *spec.Spec) []store.LTMRecord {
	prov := provenance(s)
	var recs []store.LTMRecord
	for _, k := range s.Knowledge {
		if strings.TrimSpace(k.Content) == "" {
			continue
		}
		mt := k.MemType
		if mt == "" {
			mt = "semantic"
		}
		conf := float64(k.Confidence)
		if conf == 0 {
			conf = 0.6
		}
		recs = append(recs, store.LTMRecord{
			RepoID: s.Meta.Repo, MemType: mt, Subject: k.Subject, Content: k.Content,
			Confidence: conf, VerifiedBy: prov,
			Metadata: map[string]any{"subject": k.Subject, "spec": s.Path, "line": k.Line},
		})
	}
	for _, c := range s.Prose {
		if strings.TrimSpace(c.Text) == "" {
			continue
		}
		recs = append(recs, store.LTMRecord{
			RepoID: s.Meta.Repo, MemType: "semantic", Subject: proseSubject(s, c), Content: c.Text,
			Confidence: proseConfidence, VerifiedBy: prov,
			Metadata: map[string]any{"spec": s.Path, "line": c.Line, "heading": c.Heading},
		})
	}
	return recs
}

func proseSubject(s *spec.Spec, c spec.Chunk) string {
	base := filepath.Base(s.Path)
	if c.Heading != "" {
		return base + "#" + c.Heading
	}
	return fmt.Sprintf("%s:%d", base, c.Line)
}
