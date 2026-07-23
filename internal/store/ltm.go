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
