package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// L1, the blackboard (§5.2). Agents write typed claims, not prose, and every
// rule below is enforced here rather than asked for in a prompt (§5.3).
const (
	MaxClaimsPerAttempt = 12
	maxSubject          = 200
	maxPredicate        = 60
	maxObject           = 500
)

var claimKinds = map[string]bool{
	"fact": true, "decision": true, "constraint": true, "finding": true,
	"failure": true, "question": true, "handoff": true,
}

// evidenceRequired is the cheapest anti-hallucination control in the system:
// a claim about observed state must point at the tool result that observed it.
var evidenceRequired = map[string]bool{"fact": true, "failure": true}

type Claim struct {
	ID         string
	RunID      string
	TaskID     string
	AttemptID  string
	Kind       string
	Subject    string
	Predicate  string
	Object     string
	Confidence float32
	Verified   string
	Evidence   string
	CreatedAt  time.Time
}

type ClaimWrite struct {
	Status     string // written|already_known
	ID         string
	Verified   string
	Superseded []string // claims this one contradicted
}

func (s *Store) WriteClaim(ctx context.Context, c Claim) (ClaimWrite, error) {
	switch {
	case !claimKinds[c.Kind]:
		return ClaimWrite{}, fmt.Errorf("unknown claim kind %q", c.Kind)
	case c.Subject == "" || len(c.Subject) > maxSubject:
		return ClaimWrite{}, fmt.Errorf("subject must be 1..%d chars", maxSubject)
	case c.Predicate == "" || len(c.Predicate) > maxPredicate:
		return ClaimWrite{}, fmt.Errorf("predicate must be 1..%d chars", maxPredicate)
	case c.Object == "" || len(c.Object) > maxObject:
		return ClaimWrite{}, fmt.Errorf("object must be 1..%d chars, put larger content in an artifact", maxObject)
	case c.Confidence < 0 || c.Confidence > 1:
		return ClaimWrite{}, fmt.Errorf("confidence must be between 0 and 1")
	case evidenceRequired[c.Kind] && c.Evidence == "":
		return ClaimWrite{}, fmt.Errorf("a %s claim requires evidence: the artifact id of the tool result that showed it", c.Kind)
	}

	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM stm_claims WHERE attempt_id = $1`, nz(c.AttemptID)).Scan(&n); err != nil {
		return ClaimWrite{}, err
	}
	if n >= MaxClaimsPerAttempt {
		return ClaimWrite{}, fmt.Errorf("claim limit of %d per attempt reached: record findings, not narration", MaxClaimsPerAttempt)
	}

	// A claim is believed because something showed it to be true (§5.8). An
	// evidence ref that resolves to an artifact of this run earns tool_observed;
	// anything else is the model's word, which cannot reach L3.
	verified := "asserted"
	if c.Evidence != "" {
		var ok bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM artifacts WHERE id = $1 AND run_id = $2)`,
			c.Evidence, c.RunID).Scan(&ok); err != nil {
			return ClaimWrite{}, err
		}
		if ok {
			verified = "tool_observed"
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ClaimWrite{}, err
	}
	defer tx.Rollback(ctx)

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO stm_claims (run_id, task_id, attempt_id, kind, subject, predicate,
		                        object, confidence, verified, evidence, dedupe_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (run_id, dedupe_hash) DO NOTHING
		RETURNING id`,
		c.RunID, nz(c.TaskID), nz(c.AttemptID), c.Kind, c.Subject, c.Predicate,
		c.Object, c.Confidence, verified, nz(c.Evidence), dedupeHash(c)).Scan(&id)
	if err != nil {
		if isNoRows(err) {
			// Telling the model it already knows this is itself useful signal.
			return ClaimWrite{Status: "already_known"}, nil
		}
		return ClaimWrite{}, err
	}

	// Contradiction never overwrites. Both readings survive, because the new
	// one is not always the right one (§5.3).
	rows, err := tx.Query(ctx, `
		UPDATE stm_claims SET superseded_by = $1
		WHERE run_id = $2 AND subject = $3 AND predicate = $4 AND id <> $1
		  AND object <> $5 AND superseded_by IS NULL
		RETURNING id`,
		id, c.RunID, c.Subject, c.Predicate, c.Object)
	if err != nil {
		return ClaimWrite{}, err
	}
	var superseded []string
	for rows.Next() {
		var old string
		if err := rows.Scan(&old); err != nil {
			rows.Close()
			return ClaimWrite{}, err
		}
		superseded = append(superseded, old)
	}
	rows.Close()

	if len(superseded) > 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO events (run_id, task_id, type, payload) VALUES ($1,$2,'memory.contradiction',$3)`,
			c.RunID, nz(c.TaskID), map[string]any{
				"subject": c.Subject, "predicate": c.Predicate, "new": id, "superseded": superseded,
			}); err != nil {
			return ClaimWrite{}, err
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO events (run_id, task_id, type, payload) VALUES ($1,$2,'memory.claim_written',$3)`,
		c.RunID, nz(c.TaskID), map[string]any{"id": id, "kind": c.Kind, "subject": c.Subject, "verified": verified},
	); err != nil {
		return ClaimWrite{}, err
	}
	return ClaimWrite{Status: "written", ID: id, Verified: verified, Superseded: superseded}, tx.Commit(ctx)
}

// ClaimQuery is a task's resolved read-set (§5.4). It deliberately cannot ask
// for the whole blackboard.
type ClaimQuery struct {
	RunID    string
	TaskID   string   // scopes decision/finding/failure to this task and its ancestors
	Role     string   // receives handoffs addressed to role:<Role>
	Kinds    []string // from the spec's read_set.claim_kinds
	Subjects []string // subject prefixes, e.g. "file:internal/auth/"
	Limit    int
}

// ReadClaims resolves a read-set. Constraints are always included because they
// are short and binding; everything else is scoped. Sibling tasks running in
// the same wave are invisible: their claims are unverified and mid-flight, and
// that exclusion is what makes parallel execution memory-safe.
func (s *Store) ReadClaims(ctx context.Context, q ClaimQuery) ([]Claim, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	seen := map[string]bool{}
	var out []Claim
	collect := func(sql string, args ...any) error {
		rows, err := s.pool.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Claim
			if err := rows.Scan(&c.ID, &c.TaskID, &c.Kind, &c.Subject, &c.Predicate,
				&c.Object, &c.Confidence, &c.Verified, &c.Evidence, &c.CreatedAt); err != nil {
				return err
			}
			if !seen[c.ID] {
				seen[c.ID], c.RunID = true, q.RunID
				out = append(out, c)
			}
		}
		return rows.Err()
	}
	const cols = `SELECT id, coalesce(task_id::text,''), kind, subject, predicate, object,
	                     confidence, verified, coalesce(evidence,''), created_at FROM stm_claims`

	if err := collect(cols+` WHERE run_id=$1 AND kind='constraint' AND superseded_by IS NULL
	                         ORDER BY created_at`, q.RunID); err != nil {
		return nil, err
	}
	if q.Role != "" {
		if err := collect(cols+` WHERE run_id=$1 AND kind='handoff' AND subject=$2
		                         AND superseded_by IS NULL ORDER BY created_at`,
			q.RunID, "role:"+q.Role); err != nil {
			return nil, err
		}
	}
	if kinds := without(q.Kinds, "constraint", "handoff", "fact"); len(kinds) > 0 && q.TaskID != "" {
		if err := collect(`
			WITH RECURSIVE anc(key) AS (
			    SELECT key FROM tasks WHERE id = $2      -- the task itself: a retry
			  UNION                                       -- must see why the last attempt failed
			    SELECT unnest(t.depends_on) FROM tasks t JOIN anc ON t.key = anc.key
			    WHERE t.run_id = $1
			)
			`+cols+` WHERE run_id = $1 AND kind = ANY($3) AND superseded_by IS NULL
			         AND task_id IN (SELECT id FROM tasks WHERE run_id=$1 AND key IN (SELECT key FROM anc))
			         ORDER BY confidence DESC, created_at DESC LIMIT $4`,
			q.RunID, q.TaskID, kinds, q.Limit); err != nil {
			return nil, err
		}
	}
	if len(q.Subjects) > 0 && contains(q.Kinds, "fact") {
		if err := collect(cols+` WHERE run_id=$1 AND kind='fact' AND superseded_by IS NULL
		                         AND subject LIKE ANY($2) ORDER BY confidence DESC LIMIT $3`,
			q.RunID, prefixes(q.Subjects), q.Limit); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func dedupeHash(c Claim) string {
	norm := strings.Join(strings.Fields(strings.ToLower(c.Object)), " ")
	sum := sha256.Sum256([]byte(c.Kind + "\x00" + c.Subject + "\x00" + c.Predicate + "\x00" + norm))
	return hex.EncodeToString(sum[:])
}

func prefixes(subjects []string) []string {
	out := make([]string, len(subjects))
	for i, s := range subjects {
		out[i] = strings.ReplaceAll(s, "%", `\%`) + "%"
	}
	return out
}

func without(kinds []string, drop ...string) []string {
	var out []string
	for _, k := range kinds {
		if !contains(drop, k) {
			out = append(out, k)
		}
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// nz turns "" into NULL, so empty ids do not fail uuid parsing.
func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isNoRows(err error) bool { return strings.Contains(err.Error(), "no rows in result set") }
