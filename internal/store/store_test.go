package store

import (
	"context"
	"testing"

	"github.com/rasatria01/theorm/internal/spec"
)

// Needs the compose database: docker compose up -d db
func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), DSN())
	if err != nil {
		t.Skipf("no database at %s: %v", DSN(), err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestCreateRun(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	sp, diags := spec.ParseFile("../../.theorm/specs/0000-example.md")
	if sp == nil {
		t.Fatalf("fixture: %v", diags)
	}
	if d := spec.Validate(sp); len(d) != 0 {
		t.Fatalf("fixture does not validate: %v", d)
	}

	runID, err := s.CreateRun(ctx, sp, "compiled", "8ff80eb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.pool.Exec(ctx, `DELETE FROM runs WHERE id=$1`, runID) })

	var tasks int
	var state, role string
	var writeSet []string
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) OVER (), state, role, write_set FROM tasks WHERE run_id=$1 LIMIT 1`,
		runID).Scan(&tasks, &state, &role, &writeSet); err != nil {
		t.Fatal(err)
	}
	if tasks != len(sp.Tasks) {
		t.Errorf("wrote %d tasks, spec has %d", tasks, len(sp.Tasks))
	}
	if state != "pending" || role != "coder" || len(writeSet) == 0 {
		t.Errorf("task row wrong: state=%s role=%s write_set=%v", state, role, writeSet)
	}

	var events int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE run_id=$1 AND type='run.created'`, runID).Scan(&events)
	if events != 1 {
		t.Errorf("want 1 run.created event, got %d", events)
	}

	// The clamped tool set must survive the round trip: it is what the agent
	// runtime will actually offer the model.
	var tools []string
	if err := s.pool.QueryRow(ctx,
		`SELECT array(SELECT jsonb_array_elements_text(contract->'tools')) FROM tasks WHERE run_id=$1 LIMIT 1`,
		runID).Scan(&tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != len(sp.Tasks[0].Tools) {
		t.Errorf("stored %d tools, clamp produced %d", len(tools), len(sp.Tasks[0].Tools))
	}
}

// A failed insert must leave nothing behind: a half-created run is worse than none.
func TestCreateRunIsAtomic(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	sp := &spec.Spec{
		Path: "test", Meta: spec.Meta{Repo: "github.com/x/y", Title: "atomic"},
		Tasks: []spec.Task{
			{ID: "T1", Title: "ok", Role: "coder", ResourceClass: "cpu_only"},
			{ID: "T1", Title: "duplicate key", Role: "coder", ResourceClass: "cpu_only"},
		},
	}
	if _, err := s.CreateRun(ctx, sp, "compiled", "deadbeef"); err == nil {
		t.Fatal("duplicate task key should fail the transaction")
	}
	var runs int
	s.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE title='atomic'`).Scan(&runs)
	if runs != 0 {
		t.Errorf("rolled-back run left %d rows behind", runs)
	}
}
