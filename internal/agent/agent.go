// Package agent runs one task: the step loop of §7.2. Three rules shape it.
// Tool errors are returned to the model rather than raised, because a denied
// tool is an observation it can react to. Every tool result becomes an artifact.
// And narration is a failure: a model that says it will read a file without
// calling read_file gets one nudge, then the attempt ends.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rasatria01/theorm/internal/inference"
	"github.com/rasatria01/theorm/internal/prompt"
	"github.com/rasatria01/theorm/internal/role"
	"github.com/rasatria01/theorm/internal/store"
	"github.com/rasatria01/theorm/internal/tools"
)

// repeatLimit is F5: the same call with the same arguments three times is a
// loop, not persistence.
const repeatLimit = 3

type Runner struct {
	Store *store.Store
	Tools *tools.Registry
	Model inference.Model
	Dir   string // worktree root
	// ponytail: Dir is the repository itself until the worktree pool lands in
	// C5. Safe for read-only roles, and the write-set check is what protects
	// the others.
}

type Outcome struct {
	AttemptID string
	Steps     int
	Completed bool
	Summary   string
	Reason    string // why it stopped, when it did not complete
}

func (r *Runner) RunTask(ctx context.Context, runID, key string) (Outcome, error) {
	task, err := r.Store.Task(ctx, runID, key)
	if err != nil {
		return Outcome{}, err
	}
	cfg, ok := role.Get(task.Role)
	if !ok {
		return Outcome{}, fmt.Errorf("unknown role %q", task.Role)
	}
	attemptID, err := r.Store.StartAttempt(ctx, task.ID)
	if err != nil {
		return Outcome{}, err
	}
	env := tools.Env{
		Store: r.Store, RunID: runID, TaskID: task.ID, AttemptID: attemptID,
		Role: task.Role, Allowed: task.Contract.Tools, Dir: r.Dir,
		ReadSet: store.ClaimQuery{
			Kinds:    task.Contract.ReadSet.ClaimKinds,
			Subjects: task.Contract.ReadSet.Subjects,
			Limit:    task.Contract.ReadSet.Limit,
		},
	}
	if len(env.Allowed) == 0 {
		env.Allowed = cfg.Tools
	}
	specs := toolSpecs(r.Tools.ForRole(env.Allowed))

	r.event(ctx, env, "attempt.started", map[string]any{"role": task.Role, "task": key})
	out := Outcome{AttemptID: attemptID}
	var turns []string
	seen := map[string]int{}
	narration := 0

	for out.Steps = 1; out.Steps <= cfg.MaxSteps; out.Steps++ {
		// L0 is rebuilt every step from L1, L2 and the turn log. There is no
		// conversation history the agent appends to and carries forward.
		p, err := prompt.Assemble(ctx, r.Store, runID, key, turns)
		if err != nil {
			return out, err
		}
		resp, err := r.Model.Chat(ctx, inference.Request{
			Messages: []inference.Message{
				{Role: "system", Content: p.System},
				{Role: "user", Content: p.User},
			},
			Tools: specs, Temperature: cfg.Temperature, MaxTokens: 4000,
		})
		if err != nil {
			out.Reason = "model call failed: " + err.Error()
			r.event(ctx, env, "attempt.finished", map[string]any{"outcome": "model_error", "error": err.Error()})
			return out, err
		}
		r.event(ctx, env, "llm.response", map[string]any{
			"model": resp.Model, "step": out.Steps, "prompt_tokens": resp.PromptTokens,
			"completion_tokens": resp.CompletionTokens, "tool_calls": len(resp.ToolCalls),
		})

		if len(resp.ToolCalls) == 0 {
			// One nudge, then stop: the alternative is forty steps of prose.
			narration++
			if narration > 1 {
				out.Reason = "agent stalled: two steps with no tool call"
				break
			}
			turns = append(turns, "step "+fmt.Sprint(out.Steps)+
				": you replied with text and called no tool. Call a tool or call task_complete.")
			continue
		}
		narration = 0

		for _, call := range resp.ToolCalls {
			name, args := call.Function.Name, call.Args()
			fp := fingerprint(name, args)
			if seen[fp]++; seen[fp] >= repeatLimit {
				turns = append(turns, fmt.Sprintf("step %d: %s with these arguments has already been "+
					"called %d times and returned the same thing. Do something different.",
					out.Steps, name, seen[fp]))
				out.Reason = fmt.Sprintf("tool loop: %s called %d times with identical arguments", name, seen[fp])
				return r.finish(ctx, env, out, "tool_loop")
			}
			res := r.Tools.Execute(ctx, env, name, args)
			turns = append(turns, turnLine(out.Steps, name, args, res))

			if name == tools.TaskComplete && res.Error == "" {
				out.Completed, out.Summary = true, res.Summary
				return r.finish(ctx, env, out, "ok")
			}
		}
	}
	if out.Steps > cfg.MaxSteps {
		out.Steps = cfg.MaxSteps
		if out.Reason == "" {
			out.Reason = fmt.Sprintf("step limit of %d reached without task_complete", cfg.MaxSteps)
		}
	}
	return r.finish(ctx, env, out, "agent_error")
}

func (r *Runner) finish(ctx context.Context, env tools.Env, out Outcome, outcome string) (Outcome, error) {
	detail := out.Summary
	if !out.Completed {
		detail = out.Reason
	}
	_, err := r.Store.Pool().Exec(ctx,
		`UPDATE attempts SET outcome=$2, detail=$3, ended_at=now() WHERE id=$1`,
		env.AttemptID, outcome, detail)
	r.event(ctx, env, "attempt.finished", map[string]any{
		"outcome": outcome, "steps": out.Steps, "detail": detail,
	})
	return out, err
}

func (r *Runner) event(ctx context.Context, env tools.Env, typ string, payload map[string]any) {
	_ = r.Store.Event(ctx, store.Event{
		RunID: env.RunID, TaskID: env.TaskID, AttemptID: env.AttemptID,
		Type: typ, Payload: payload,
	})
}

// turnLine is one line of the scratchpad: what was called and what came back,
// never the payload itself.
func turnLine(step int, name string, args json.RawMessage, res tools.Result) string {
	body := res.Summary
	if res.Error != "" {
		body = "error: " + res.Error
	}
	if len(body) > 600 {
		body = body[:600] + "…"
	}
	line := fmt.Sprintf("step %d: %s(%s) -> %s", step, name, compact(args), body)
	if res.ArtifactID != "" && res.Error == "" {
		line += fmt.Sprintf(" [artifact %s]", res.ArtifactID[:12])
	}
	return line
}

func compact(args json.RawMessage) string {
	s := strings.Join(strings.Fields(string(args)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// fingerprint canonicalises the arguments first: JSON key order is not a
// difference between two calls, and a model that reorders keys each turn would
// otherwise loop forever undetected.
func fingerprint(name string, args json.RawMessage) string {
	canon := compact(args)
	var m map[string]any
	if json.Unmarshal(args, &m) == nil {
		if b, err := json.Marshal(m); err == nil { // Go marshals map keys sorted
			canon = string(b)
		}
	}
	sum := sha256.Sum256([]byte(name + "\x00" + canon))
	return hex.EncodeToString(sum[:8])
}

func toolSpecs(ts []tools.Tool) []inference.ToolSpec {
	out := make([]inference.ToolSpec, 0, len(ts))
	for _, t := range ts {
		out = append(out, inference.ToolSpec{
			Name: t.Name(), Description: t.Description(), Schema: t.Schema(),
		})
	}
	return out
}
