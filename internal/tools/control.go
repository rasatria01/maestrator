package tools

import (
	"context"
	"encoding/json"
)

// TaskComplete is the name of the tool an agent calls to end its own loop. The
// runtime checks for it by name, which is why it is exported.
const TaskComplete = "task_complete"

type taskComplete struct{}

func (taskComplete) Name() string { return TaskComplete }
func (taskComplete) Description() string {
	return "Call this when every acceptance criterion is satisfied, saying briefly what you did " +
		"and how you verified it. Do not call it hoping the criteria pass."
}
func (taskComplete) Annotations() Annotations { return Annotations{ReadOnly: true} }
func (taskComplete) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {"summary": {"type": "string", "maxLength": 1000}},
	  "required": ["summary"]
	}`)
}

func (taskComplete) Execute(ctx context.Context, args json.RawMessage, env Env) (Output, error) {
	var in struct {
		Summary string `json:"summary"`
	}
	if err := decode(args, &in); err != nil {
		return Output{}, err
	}
	return Output{Content: in.Summary, Kind: "task_result", MediaType: "text/plain"}, nil
}
