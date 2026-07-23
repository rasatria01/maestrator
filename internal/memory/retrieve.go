package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rasatria01/theorm/internal/store"
)

// defaultTopK is the §5.7 budget line: L3 gives the prompt its top 6 reranked.
const defaultTopK = 6

// Retriever is the read side of L3: it embeds the query, asks the store to fuse
// the dense and sparse lanes with the exact lane, then reranks on CPU. The
// dashboard and the agents share this one path (§9), so a bad memory is found
// where it was used.
type Retriever struct {
	Store *store.Store
	Embed *Embedder
	TopK  int
}

type Hit struct {
	Subject string
	MemType string
	Content string
	Score   float64 // cross-encoder rerank score; higher is more relevant
}

// Search returns the top reranked records for a query. text is the task title
// and description; subjects are read-set prefixes for the exact lane (may be
// empty). It touches Postgres and the CPU sidecar, never the GPU.
func (r *Retriever) Search(ctx context.Context, repoID, text string, subjects []string) ([]Hit, error) {
	vecs, err := r.Embed.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	cands, err := r.Store.RetrieveCandidates(ctx, repoID, vecs[0], text, subjects)
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, nil
	}
	docs := make([]string, len(cands))
	for i, c := range cands {
		docs[i] = c.Content
	}
	scores, err := r.Embed.Rerank(ctx, text, docs)
	if err != nil {
		return nil, err
	}
	if len(scores) != len(cands) {
		return nil, fmt.Errorf("rerank returned %d scores for %d candidates", len(scores), len(cands))
	}
	return topHits(cands, scores, r.topK()), nil
}

// SearchTask is Search with the §5.6 query already built from a task: the goal,
// the task title, and the read-set subjects, which also seed the exact lane.
func (r *Retriever) SearchTask(ctx context.Context, repoID, goal, title string, subjects []string) ([]Hit, error) {
	text := strings.TrimSpace(goal + ". " + title)
	if len(subjects) > 0 {
		text += " " + strings.Join(subjects, " ")
	}
	return r.Search(ctx, repoID, text, subjects)
}

// Contents is the retrieved records' text in rank order, ready for the prompt's
// memory section.
func Contents(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Content
	}
	return out
}

func (r *Retriever) topK() int {
	if r.TopK > 0 {
		return r.TopK
	}
	return defaultTopK
}

// topHits pairs each candidate with its rerank score, sorts by score, and caps
// at k. Pure so the ranking is testable without a database or the sidecar.
func topHits(cands []store.Candidate, scores []float64, k int) []Hit {
	hits := make([]Hit, len(cands))
	for i, c := range cands {
		hits[i] = Hit{Subject: c.Subject, MemType: c.MemType, Content: c.Content, Score: scores[i]}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}
