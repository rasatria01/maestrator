package memory

import (
	"context"
	"fmt"
	"sort"

	"github.com/rasatria01/theorm/internal/store"
)

// The Curator runs once at end of run and decides which L1 claims outlive it
// (§5.8 mechanism 2). Every rule is enforced here in Go, never asked of a model.
// ponytail: this promotes every promotable claim, capped at 10 by confidence,
// rather than having a model choose. Promotable claims are already high-signal
// and few; add the model's `ltm_propose` selection only if the corpus gets noisy.
const (
	maxPerRun    = 10   // §5.8 rule 4
	dupThreshold = 0.92 // §5.8 rule 2: cosine similarity above this is a duplicate
)

type Curator struct {
	Store *store.Store
	Embed *Embedder
}

type CurateResult struct {
	Promoted int // new L3 records written
	Bumped   int // near-duplicates that refreshed an existing record instead
	Parked   int // contradictions parked for human review
	Skipped  int // eligible but over the per-run cap
	Claims   int // promotable claims considered
}

type verdict int

const (
	vPromote verdict = iota
	vBump
	vPark
	vSkip
)

// decide applies §5.8 rules 2–4 to one candidate. Contradiction and duplicate
// are checked before the cap because neither writes a new record, so neither
// should be starved by it; only a fresh promotion counts against the ten.
func decide(promoted int, candConf float64, near, conflict *store.Neighbor) verdict {
	if conflict != nil && conflict.Confidence > candConf {
		return vPark
	}
	if near != nil && near.Cosine > dupThreshold {
		return vBump
	}
	if promoted >= maxPerRun {
		return vSkip
	}
	return vPromote
}

// Curate promotes a run's claims into L3. It embeds each candidate, checks it
// against what already exists, and writes, bumps, or parks it accordingly, then
// always writes one episodic record for the run (rule 5).
func (c *Curator) Curate(ctx context.Context, runID string) (CurateResult, error) {
	repoID, title, err := c.Store.RunMeta(ctx, runID)
	if err != nil {
		return CurateResult{}, err
	}
	claims, err := c.Store.PromotableClaims(ctx, runID)
	if err != nil {
		return CurateResult{}, err
	}

	// Highest confidence first, so the cap keeps the best ten (rule 4).
	type cand struct {
		claim   store.Claim
		memType string
		content string
	}
	var cands []cand
	for _, cl := range claims {
		mt := memTypeFor(cl.Kind)
		if mt == "" {
			continue // question, handoff: run-local, never promoted
		}
		cands = append(cands, cand{cl, mt, cl.Subject + " " + cl.Predicate + " " + cl.Object})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return cands[i].claim.Confidence > cands[j].claim.Confidence
	})

	res := CurateResult{Claims: len(cands)}
	if len(cands) > 0 {
		texts := make([]string, len(cands))
		for i, cd := range cands {
			texts[i] = cd.content
		}
		vecs, err := c.Embed.Embed(ctx, texts)
		if err != nil {
			return res, err
		}
		for i, cd := range cands {
			near, err := c.Store.NearestActive(ctx, repoID, cd.memType, vecs[i])
			if err != nil {
				return res, err
			}
			conflict, err := c.Store.Contradiction(ctx, repoID, cd.claim.Subject, cd.claim.Predicate, cd.claim.Object)
			if err != nil {
				return res, err
			}
			switch decide(res.Promoted, float64(cd.claim.Confidence), near, conflict) {
			case vPark:
				res.Parked++
				_ = c.Store.Event(ctx, store.Event{RunID: runID, Type: "memory.conflict", Payload: map[string]any{
					"subject": cd.claim.Subject, "predicate": cd.claim.Predicate,
					"claim": cd.claim.ID, "existing": conflict.ID,
				}})
			case vBump:
				res.Bumped++
				if err := c.Store.BumpHit(ctx, near.ID); err != nil {
					return res, err
				}
			case vSkip:
				res.Skipped++
			case vPromote:
				if _, err := c.Store.WriteLTM(ctx, store.LTMRecord{
					RepoID: repoID, MemType: cd.memType, Subject: cd.claim.Subject, Content: cd.content,
					Metadata: map[string]any{
						"subject": cd.claim.Subject, "predicate": cd.claim.Predicate,
						"object": cd.claim.Object, "kind": cd.claim.Kind,
					},
					Confidence: float64(cd.claim.Confidence), VerifiedBy: verifiedByFor(cd.claim.Verified),
					SourceRun: runID, SourceClaim: cd.claim.ID, Embedding: vecs[i],
				}); err != nil {
					return res, err
				}
				res.Promoted++
			}
		}
	}

	// Rule 5: one episodic record per run, always, not model-decided.
	epContent := fmt.Sprintf("Run %q promoted %d records from %d promotable claims.", title, res.Promoted, res.Claims)
	epVec, err := c.Embed.Embed(ctx, []string{epContent})
	if err != nil {
		return res, err
	}
	if _, err := c.Store.WriteLTM(ctx, store.LTMRecord{
		RepoID: repoID, MemType: "episodic", Subject: "run:" + runID, Content: epContent,
		Confidence: 0.7, VerifiedBy: "tool_observation", SourceRun: runID, Embedding: epVec[0],
	}); err != nil {
		return res, err
	}
	return res, nil
}

// memTypeFor maps a claim kind to an L3 mem_type. A kind with no mapping is
// run-local and never promoted.
func memTypeFor(kind string) string {
	switch kind {
	case "fact", "decision", "constraint", "finding":
		return "semantic"
	case "failure":
		return "procedural" // a failure teaches what not to do next time
	default:
		return ""
	}
}

// verifiedByFor translates an L1 verification level to the L3 provenance vocab.
func verifiedByFor(level string) string {
	switch level {
	case "gate_passed":
		return "gate_pass"
	case "review_passed":
		return "review_pass"
	case "human_confirmed":
		return "human"
	default:
		return "tool_observation"
	}
}
