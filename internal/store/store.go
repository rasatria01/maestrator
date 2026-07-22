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

// contract carries everything the scheduler and the context assembler need
// that does not deserve its own column.
type contract struct {
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
		c, err := json.Marshal(contract{
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
