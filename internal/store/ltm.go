package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// PromotableLevels are the L1 verification levels eligible for L3 (§5.8 rule 1).
// An `asserted` claim — the model's unsupported word — dies with its run.
var PromotableLevels = []string{"tool_observed", "gate_passed", "review_passed", "human_confirmed"}

// RunMeta returns a run's repo id and title, which the Curator needs for the
// episodic record and the promotion queries.
func (s *Store) RunMeta(ctx context.Context, runID string) (repoID, title string, err error) {
	err = s.pool.QueryRow(ctx, `SELECT repo_id, title FROM runs WHERE id=$1`, runID).Scan(&repoID, &title)
	return
}

// PromotableClaims returns the run's non-superseded claims at a promotable
// verification level (§5.8 rule 1). The kind→mem_type mapping is the Curator's.
func (s *Store) PromotableClaims(ctx context.Context, runID string) ([]Claim, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, kind, subject, predicate, object, confidence, verified, created_at
		FROM stm_claims
		WHERE run_id=$1 AND superseded_by IS NULL AND verified = ANY($2)
		ORDER BY confidence DESC, created_at DESC`, runID, PromotableLevels)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Claim
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.ID, &c.Kind, &c.Subject, &c.Predicate, &c.Object,
			&c.Confidence, &c.Verified, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.RunID = runID
		out = append(out, c)
	}
	return out, rows.Err()
}

// Neighbor is an existing L3 record found by a promotion check: the near-
// duplicate (rule 2) or the contradiction (rule 3).
type Neighbor struct {
	ID         string
	Confidence float64
	Cosine     float64 // similarity in [0,1]; only set by NearestActive
}

// NearestActive returns the closest active record of the same mem_type by cosine
// similarity, for the near-duplicate check (§5.8 rule 2). nil when the repo has
// no such record yet.
func (s *Store) NearestActive(ctx context.Context, repoID, memType string, vec []float32) (*Neighbor, error) {
	var n Neighbor
	var dist float64
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, confidence, (embedding <=> $3::vector)
		FROM ltm_records
		WHERE repo_id=$1 AND mem_type=$2 AND status='active' AND embedding IS NOT NULL
		ORDER BY embedding <=> $3::vector LIMIT 1`,
		repoID, memType, vector(vec)).Scan(&n.ID, &n.Confidence, &dist)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	n.Cosine = 1 - dist // pgvector <=> is cosine distance
	return &n, nil
}

// Contradiction returns an active record with the same subject and predicate but
// a different object — a structured disagreement (§5.8 rule 3), highest
// confidence first. nil when nothing contradicts.
func (s *Store) Contradiction(ctx context.Context, repoID, subject, predicate, object string) (*Neighbor, error) {
	var n Neighbor
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, confidence FROM ltm_records
		WHERE repo_id=$1 AND status='active'
		  AND metadata->>'subject'=$2 AND metadata->>'predicate'=$3 AND metadata->>'object'<>$4
		ORDER BY confidence DESC LIMIT 1`,
		repoID, subject, predicate, object).Scan(&n.ID, &n.Confidence)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// BumpHit refreshes a near-duplicate instead of writing a redundant row (§5.8
// rule 2): the existing record earns a hit and a fresh timestamp.
func (s *Store) BumpHit(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE ltm_records SET hit_count=hit_count+1, updated_at=now() WHERE id=$1`, id)
	return err
}

// LTMRecord is a promoted record bound for L3: semantic, procedural or episodic.
// Entity records go through ReplaceEntities instead.
type LTMRecord struct {
	RepoID      string
	MemType     string
	Subject     string
	Content     string
	Metadata    map[string]any
	Confidence  float64
	VerifiedBy  string
	SourceRun   string
	SourceClaim string // "" for the auto-written episodic record
	Embedding   []float32
}

func (s *Store) WriteLTM(ctx context.Context, r LTMRecord) (string, error) {
	if len(r.Embedding) == 0 {
		return "", fmt.Errorf("record %q has no embedding", r.Subject)
	}
	meta, err := json.Marshal(r.Metadata)
	if err != nil {
		return "", err
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO ltm_records (repo_id, mem_type, subject, content, metadata,
		    source_run, source_claim, verified_by, confidence, embedding)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::vector) RETURNING id::text`,
		r.RepoID, r.MemType, r.Subject, r.Content, meta, nz(r.SourceRun), nz(r.SourceClaim),
		r.VerifiedBy, r.Confidence, vector(r.Embedding)).Scan(&id)
	return id, err
}

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

// ReplaceSpecKnowledge writes a spec's knowledge records under its provenance
// ("spec:<sha>"), replacing any prior records with the same provenance in one
// transaction. Idempotent per spec version: re-compiling never duplicates, and
// RetireSpec on that provenance removes exactly these (§4.6 control 6, B5).
func (s *Store) ReplaceSpecKnowledge(ctx context.Context, repoID, provenance string, recs []LTMRecord) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`DELETE FROM ltm_records WHERE repo_id=$1 AND verified_by=$2`, repoID, provenance); err != nil {
		return fmt.Errorf("clearing prior spec knowledge: %w", err)
	}
	for _, r := range recs {
		if len(r.Embedding) == 0 {
			return fmt.Errorf("record %q has no embedding", r.Subject)
		}
		meta, err := json.Marshal(r.Metadata)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO ltm_records (repo_id, mem_type, subject, content, metadata,
			    verified_by, confidence, embedding)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8::vector)`,
			r.RepoID, r.MemType, r.Subject, r.Content, meta,
			r.VerifiedBy, r.Confidence, vector(r.Embedding)); err != nil {
			return fmt.Errorf("inserting %q: %w", r.Subject, err)
		}
	}
	return tx.Commit(ctx)
}

// RetireSpec retires every record from a spec version. Retired records are kept,
// not deleted, and excluded from retrieval, so a revoked spec's knowledge stops
// being served without losing the audit trail (§5.8).
func (s *Store) RetireSpec(ctx context.Context, provenance string) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE ltm_records SET status='retired', updated_at=now()
		 WHERE verified_by=$1 AND status<>'retired'`, provenance)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Decay is the §5.8 mechanism 3 nightly job: records nothing has retrieved in 60
// days lose a tenth of their confidence, and those that fall below the floor
// retire. Human-confirmed records are immune. Returns (decayed, retired).
// ponytail: meant to run once a day. Run it far more often and confidence drifts
// down faster than intended; guard on updated_at < now()-'1 day' if that bites.
func (s *Store) Decay(ctx context.Context) (decayed, retired int64, err error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE ltm_records SET confidence = confidence * 0.9, updated_at = now()
		WHERE status='active'
		  AND (last_hit_at IS NULL OR last_hit_at < now() - INTERVAL '60 days')
		  AND verified_by <> 'human'`)
	if err != nil {
		return 0, 0, err
	}
	decayed = tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `
		UPDATE ltm_records SET status='retired', updated_at=now()
		WHERE status='active' AND confidence < 0.25`)
	if err != nil {
		return decayed, 0, err
	}
	return decayed, tag.RowsAffected(), nil
}

// MarkStale is §5.8 mechanism 4: the entity record for a changed or deleted file
// goes stale (its summary no longer describes the file), and a procedural record
// that references one loses 0.2 confidence. The invalidation signal is free —
// it is just the list of files a run touched. Returns (stale, weakened).
func (s *Store) MarkStale(ctx context.Context, repoID string, changed []string) (stale, weakened int64, err error) {
	if len(changed) == 0 {
		return 0, 0, nil
	}
	subjects := staleSubjects(changed)
	tag, err := s.pool.Exec(ctx, `
		UPDATE ltm_records SET status='stale', updated_at=now()
		WHERE repo_id=$1 AND mem_type='entity' AND status='active' AND subject = ANY($2)`,
		repoID, subjects)
	if err != nil {
		return 0, 0, err
	}
	stale = tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `
		UPDATE ltm_records SET confidence = greatest(confidence - 0.2, 0), updated_at=now()
		WHERE repo_id=$1 AND mem_type='procedural' AND status='active'
		  AND metadata->>'subject' = ANY($2)`,
		repoID, subjects)
	if err != nil {
		return stale, 0, err
	}
	return stale, tag.RowsAffected(), nil
}

// RunBase returns a run's repo id and base commit, so `theorm stale` can diff
// exactly what a run changed.
func (s *Store) RunBase(ctx context.Context, runID string) (repoID, baseCommit string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT repo_id, coalesce(base_commit,'') FROM runs WHERE id=$1`, runID).Scan(&repoID, &baseCommit)
	return
}

// staleSubjects turns git's changed-file paths into entity subjects. The "file:"
// convention is the one indexing writes, so it must match exactly or nothing is
// marked stale. Git already emits forward-slash paths.
func staleSubjects(changed []string) []string {
	out := make([]string, len(changed))
	for i, p := range changed {
		out[i] = "file:" + p
	}
	return out
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
