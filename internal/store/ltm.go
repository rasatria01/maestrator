package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// L3 long-term memory (§5.6). B1 writes entity records; B2 will read them back
// through hybrid retrieval. The table, its hnsw and gin indexes already exist in
// 0001_init.sql; this is the Go surface over it.

// LTMEntity is one per-file summary bound for L3, plus its CPU-generated
// embedding.
type LTMEntity struct {
	Subject   string    // "file:<relpath>"
	Content   string    // deterministic summary the model reads
	Embedding []float32 // 768 dims from the sidecar
}

// ReplaceEntities makes `theorm index` idempotent: for the subjects in this pass
// it deletes any prior entity record and inserts the fresh one, in one
// transaction, so re-indexing never duplicates and never half-writes.
// ponytail: delete+insert resets hit_count on re-index. Switch to ON CONFLICT on
// a unique (repo_id,subject) index once decay (B4) needs the counter preserved.
func (s *Store) ReplaceEntities(ctx context.Context, repoID string, recs []LTMEntity) error {
	if len(recs) == 0 {
		return nil
	}
	subjects := make([]string, len(recs))
	for i, r := range recs {
		subjects[i] = r.Subject
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`DELETE FROM ltm_records WHERE repo_id=$1 AND mem_type='entity' AND subject = ANY($2)`,
		repoID, subjects); err != nil {
		return fmt.Errorf("clearing prior entities: %w", err)
	}
	for _, r := range recs {
		if len(r.Embedding) == 0 {
			return fmt.Errorf("%s has no embedding", r.Subject)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO ltm_records (repo_id, mem_type, subject, content, verified_by, confidence, embedding)
			VALUES ($1, 'entity', $2, $3, 'tool_observation', 0.6, $4::vector)`,
			repoID, r.Subject, r.Content, vector(r.Embedding)); err != nil {
			return fmt.Errorf("inserting %s: %w", r.Subject, err)
		}
	}
	return tx.Commit(ctx)
}

// LTMCounts returns active record counts per mem_type for a repo, so `theorm
// index` can report what it wrote and the dashboard (E) can show memory health.
func (s *Store) LTMCounts(ctx context.Context, repoID string) (map[string]int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT mem_type, count(*) FROM ltm_records WHERE repo_id=$1 AND status='active' GROUP BY mem_type`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, rows.Err()
}

// Candidate is one record surviving the fusion stage, before rerank. Content
// is carried so the reranker scores it without a second round-trip.
type Candidate struct {
	ID      string
	Subject string
	MemType string
	Content string
}

// RetrieveCandidates runs the §5.6 fusion: pgvector cosine (top 30) and tsvector
// ts_rank_cd (top 30) fused by reciprocal rank (k=60) to a top 20, unioned with
// the exact lane — records whose subject starts with a read-set prefix, always
// included because a task naming a file must see that file's memory regardless of
// what the embeddings think. Rerank happens above this, in the memory package.
func (s *Store) RetrieveCandidates(ctx context.Context, repoID string, queryVec []float32, queryText string, subjectPrefixes []string) ([]Candidate, error) {
	patterns := make([]string, 0, len(subjectPrefixes))
	for _, p := range subjectPrefixes {
		patterns = append(patterns, p+"%")
	}
	rows, err := s.pool.Query(ctx, `
		WITH dense AS (
		  SELECT id, ROW_NUMBER() OVER (ORDER BY embedding <=> $1::vector) AS rank
		  FROM ltm_records
		  WHERE repo_id = $2 AND status = 'active' AND embedding IS NOT NULL
		  ORDER BY embedding <=> $1::vector LIMIT 30
		),
		sparse AS (
		  SELECT id, ROW_NUMBER() OVER (ORDER BY ts_rank_cd(content_tsv, query) DESC) AS rank
		  FROM ltm_records, plainto_tsquery('english', $3) query
		  WHERE repo_id = $2 AND status = 'active' AND content_tsv @@ query
		  ORDER BY ts_rank_cd(content_tsv, query) DESC LIMIT 30
		),
		fused AS (
		  SELECT COALESCE(d.id, s.id) AS id
		  FROM dense d FULL OUTER JOIN sparse s USING (id)
		  ORDER BY COALESCE(1.0/(60 + d.rank), 0) + COALESCE(1.0/(60 + s.rank), 0) DESC
		  LIMIT 20
		),
		ids AS (
		  SELECT id FROM fused
		  UNION
		  SELECT id FROM ltm_records
		   WHERE repo_id = $2 AND status = 'active' AND subject LIKE ANY($4)
		)
		SELECT r.id::text, r.subject, r.mem_type, r.content
		FROM ltm_records r JOIN ids USING (id)`,
		vector(queryVec), repoID, queryText, patterns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.ID, &c.Subject, &c.MemType, &c.Content); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// vector renders a float slice as pgvector's text input form: [f1,f2,...]. The
// column is cast $n::vector, so no pgx type registration is needed for one write
// path.
func vector(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 8)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
