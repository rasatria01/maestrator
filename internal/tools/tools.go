// Package tools is the agent's hands (§8.1). Native tools and MCP tools sit
// behind one interface, and every call is capability-checked in Go.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/rasatria01/theorm/internal/store"
)

type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage
	Annotations() Annotations
	Execute(ctx context.Context, args json.RawMessage, env Env) (Output, error)
}

// Output is what a tool produces: the whole thing, however large. Tools never
// decide what reaches the model; the registry does that (§8.1).
type Output struct {
	Content    string
	Kind       string // artifact kind: test_output|diff|command_output|...
	MediaType  string
	Summary    string // set only when the tool knows better than the summariser
	ArtifactID string // set when the content is already stored, e.g. artifact_read
}

// Annotations mirror the MCP tool annotations, so native and MCP tools present
// the same shape to the model.
type Annotations struct {
	ReadOnly    bool `json:"readOnlyHint"`
	Destructive bool `json:"destructiveHint"`
	Idempotent  bool `json:"idempotentHint"`
	OpenWorld   bool `json:"openWorldHint"`
}

// Env is what a tool is allowed to know about the task calling it.
type Env struct {
	Store     *store.Store
	RunID     string
	TaskID    string
	AttemptID string
	Role      string
	Allowed   []string // the role's effective capability set, post-clamp
	Dir       string   // worktree root; nothing may be written outside it
	ReadSet   store.ClaimQuery
}

// Result is what re-enters the model's context: a summary, never the payload.
type Result struct {
	Summary    string `json:"summary"`
	ArtifactID string `json:"artifact_id,omitempty"`
	Error      string `json:"error,omitempty"`
}

// InlineLimit is the largest output that enters context verbatim. Above it,
// the model gets a summary and an artifact id and decides for itself whether
// the full text is worth its budget.
const InlineLimit = 500

type Registry struct{ byName map[string]Tool }

func New(ts ...Tool) *Registry {
	r := &Registry{byName: map[string]Tool{}}
	for _, t := range ts {
		r.byName[t.Name()] = t
	}
	return r
}

// ForRole returns only the tools a role may call. A model respects an absent
// tool far more reliably than a prohibited one, so filtering beats instructing.
func (r *Registry) ForRole(allowed []string) []Tool {
	var out []Tool
	for _, name := range allowed {
		if t, ok := r.byName[name]; ok {
			out = append(out, t)
		}
	}
	return out
}

// Execute runs a tool and intercepts its result. Every result becomes an
// artifact, including successful ones: that is what makes N3 auditability real
// rather than aspirational, and it is why a 4,000 line test log costs the model
// a handle and a sentence instead of its whole window.
//
// It never returns an error to the caller. A denied tool or a bad argument is
// an observation the model can correct (§7.2).
func (r *Registry) Execute(ctx context.Context, env Env, name string, args json.RawMessage) Result {
	started := time.Now()
	emit(ctx, env, "tool.called", map[string]any{"name": name, "args": string(args)})

	t, ok := r.byName[name]
	if !ok {
		return deny(ctx, env, name, started, fmt.Sprintf("no tool named %q", name))
	}
	if !allows(env.Allowed, name) {
		return deny(ctx, env, name, started, fmt.Sprintf("tool %q is not available to role %s", name, env.Role))
	}
	out, err := t.Execute(ctx, args, env)
	if err != nil {
		// It ran and failed, which is not the same as being refused: the audit
		// trail should not call a missing file a denial.
		emit(ctx, env, "tool.returned", map[string]any{
			"name": name, "error": err.Error(), "duration_ms": time.Since(started).Milliseconds(),
		})
		return Result{Error: err.Error()}
	}

	res := Result{Summary: out.Summary, ArtifactID: out.ArtifactID}
	if out.ArtifactID == "" && env.Store != nil && env.RunID != "" {
		a, err := env.Store.PutArtifact(ctx, store.ArtifactInput{
			RunID: env.RunID, TaskID: env.TaskID, Kind: kindOr(out.Kind),
			MediaType: mediaOr(out.MediaType), Content: []byte(out.Content),
		})
		if err != nil {
			return Result{Error: "storing tool result: " + err.Error()}
		}
		res.ArtifactID = a.ID
		if res.Summary == "" {
			res.Summary = a.Summary
		}
	}
	if res.Summary == "" || (out.Summary == "" && len(out.Content) <= InlineLimit) {
		res.Summary = out.Content
	}
	emit(ctx, env, "tool.returned", map[string]any{
		"name": name, "artifact_id": res.ArtifactID,
		"bytes": len(out.Content), "duration_ms": time.Since(started).Milliseconds(),
	})
	return res
}

func deny(ctx context.Context, env Env, name string, started time.Time, msg string) Result {
	emit(ctx, env, "tool.denied", map[string]any{
		"name": name, "error": msg, "duration_ms": time.Since(started).Milliseconds(),
	})
	return Result{Error: msg}
}

// emit is best-effort: a failed event insert must not fail a tool call.
func emit(ctx context.Context, env Env, typ string, payload map[string]any) {
	if env.Store == nil || env.RunID == "" {
		return
	}
	_ = env.Store.Event(ctx, store.Event{
		RunID: env.RunID, TaskID: env.TaskID, AttemptID: env.AttemptID,
		Type: typ, Payload: payload,
	})
}

func kindOr(kind string) string {
	if kind == "" {
		return "tool_result"
	}
	return kind
}

func mediaOr(m string) string {
	if m == "" {
		return "text/plain"
	}
	return m
}

func allows(allowed []string, name string) bool { return slices.Contains(allowed, name) }

func decode(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return fmt.Errorf("missing arguments")
	}
	err := json.Unmarshal(args, v)
	if err == nil {
		return nil
	}
	// Small models routinely emit every argument as a string, including numbers
	// and arrays: {"limit":"20","kinds":"[\"fact\"]"}. Retry once with those
	// unwrapped rather than spending a step telling the model about JSON.
	if relaxed, ok := unstringify(args); ok && json.Unmarshal(relaxed, v) == nil {
		return nil
	}
	return fmt.Errorf("invalid arguments: %v", err)
}

// unstringify replaces string values that themselves contain JSON with that
// JSON. It runs only after a strict decode has already failed, so a genuine
// string like "123" is never coerced on the happy path.
func unstringify(args json.RawMessage) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(args, &obj) != nil {
		return nil, false
	}
	changed := false
	for k, raw := range obj {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			continue // not a string
		}
		var inner json.RawMessage
		if json.Unmarshal([]byte(s), &inner) == nil && len(s) > 0 && s[0] != '"' {
			obj[k], changed = inner, true
		}
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(obj)
	return out, err == nil
}
