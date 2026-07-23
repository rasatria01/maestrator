// Package store is the Postgres side of the orchestrator. There is no
// in-memory fallback: with no database there is no state, and running without
// state is the failure mode F14 exists to prevent.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rasatria01/theorm/internal/spec"
)

const DefaultDSN = "postgres://theorm:theorm@127.0.0.1:55432/theorm"

func DSN() string {
	if v := os.Getenv("THEORM_DATABASE_URL"); v != "" {
		return v
	}
	return DefaultDSN
}

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Pool is the escape hatch for queries that do not deserve a method yet.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Contract carries everything the scheduler and the context assembler need
// that does not deserve its own column.
type Contract struct {
	ReadSet          spec.ReadSet      `json:"read_set"`
	Acceptance       []spec.Acceptance `json:"acceptance"`
	Gates            []string          `json:"gates"`
	Tools            []string          `json:"tools"` // effective set, post-clamp
	EstContextTokens int               `json:"est_context_tokens"`
	EstSteps         int               `json:"est_steps"`
	Expand           bool              `json:"expand"`
	SpecLine         int               `json:"spec_line"`
}

// CreateRun emits step 10 of the compiler (§4.4): a runs row and one pending
// task row per spec task, in a single transaction. Compilation either produces
// a whole run or none of it.
func (s *Store) CreateRun(ctx context.Context, sp *spec.Spec, mode, baseCommit string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var runID string
	err = tx.QueryRow(ctx, `
		INSERT INTO runs (repo_id, title, mode, spec_path, spec_sha256, base_commit, cloud_budget)
		VALUES ($1, $2, $3, $4, $5, $6, 0) RETURNING id`,
		sp.Meta.Repo, sp.Meta.Title, mode, sp.Path, sp.SHA256, baseCommit).Scan(&runID)
	if err != nil {
		return "", fmt.Errorf("insert run: %w", err)
	}

	for _, t := range sp.Tasks {
		c, err := json.Marshal(Contract{
			ReadSet: t.ReadSet, Acceptance: t.Acceptance, Gates: gates(t, sp),
			Tools: t.Tools, EstContextTokens: t.EstContextTokens, EstSteps: t.EstSteps,
			Expand: t.Expand, SpecLine: t.Line,
		})
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO tasks (run_id, key, title, role, depends_on, write_set,
			                   contract, resource_class, max_attempts)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			runID, t.ID, t.Title, t.Role, orEmpty(t.DependsOn), orEmpty(t.WriteSet),
			c, t.ResourceClass, maxAttempts(t, sp)); err != nil {
			return "", fmt.Errorf("insert task %s: %w", t.ID, err)
		}
	}

	if _, err := tx.Exec(ctx, `INSERT INTO events (run_id, type, payload) VALUES ($1,'run.created',$2)`,
		runID, map[string]any{"spec": sp.Path, "sha256": sp.SHA256, "tasks": len(sp.Tasks), "mode": mode},
	); err != nil {
		return "", err
	}
	return runID, tx.Commit(ctx)
}

// Event is the unit the dashboard projects from (§9.3). Live view and
// historical replay are the same code path because both read this table.
type Event struct {
	RunID     string
	TaskID    string
	AttemptID string
	Type      string
	Payload   map[string]any
}

func (s *Store) Event(ctx context.Context, e Event) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO events (run_id, task_id, attempt_id, type, payload) VALUES ($1,$2,$3,$4,$5)`,
		e.RunID, nz(e.TaskID), nz(e.AttemptID), e.Type, e.Payload)
	return err
}

// TaskRow is a task as the assembler and the scheduler see it.
type TaskRow struct {
	ID            string
	RunID         string
	Key           string
	Title         string
	Role          string
	State         string
	ResourceClass string
	DependsOn     []string
	WriteSet      []string
	Contract      Contract
	RunTitle      string
	RepoID        string
}

func (s *Store) Task(ctx context.Context, runID, key string) (TaskRow, error) {
	var t TaskRow
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT t.id, t.run_id, t.key, t.title, t.role, t.state, t.resource_class,
		       t.depends_on, t.write_set, t.contract, r.title, r.repo_id
		FROM tasks t JOIN runs r ON r.id = t.run_id
		WHERE t.run_id = $1 AND t.key = $2`, runID, key).
		Scan(&t.ID, &t.RunID, &t.Key, &t.Title, &t.Role, &t.State, &t.ResourceClass,
			&t.DependsOn, &t.WriteSet, &raw, &t.RunTitle, &t.RepoID)
	if err != nil {
		return t, err
	}
	return t, json.Unmarshal(raw, &t.Contract)
}

// TaskArtifacts returns this task's tool results, newest first.
// ponytail: scoped by task, not attempt, because artifacts carry no attempt_id.
// Add the column when a retry needs to hide the previous attempt's results.
func (s *Store) TaskArtifacts(ctx context.Context, taskID string) ([]Artifact, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, size_bytes, summary, path FROM artifacts
		WHERE task_id = $1 ORDER BY created_at DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.Kind, &a.Size, &a.Summary, &a.Path); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TaskID resolves a spec task key (T1) to its row id within a run.
func (s *Store) TaskID(ctx context.Context, runID, key string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM tasks WHERE run_id=$1 AND key=$2`, runID, key).Scan(&id)
	return id, err
}

// StartAttempt opens the next attempt for a task. Claims and artifacts are
// scoped to it, which is what makes N3 auditability work.
func (s *Store) StartAttempt(ctx context.Context, taskID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO attempts (task_id, n)
		VALUES ($1, (SELECT coalesce(max(n),0)+1 FROM attempts WHERE task_id=$1))
		RETURNING id`, taskID).Scan(&id)
	return id, err
}

func gates(t spec.Task, sp *spec.Spec) []string {
	if len(t.Gates) > 0 {
		return t.Gates
	}
	return sp.Meta.DefaultGates
}

func maxAttempts(t spec.Task, sp *spec.Spec) int {
	for _, n := range []int{t.MaxAttempts, sp.Meta.Policy.MaxAttempts} {
		if n > 0 {
			return n
		}
	}
	return 3
}

// orEmpty keeps a nil slice out of a NOT NULL text[] column.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
