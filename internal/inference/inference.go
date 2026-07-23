// Package inference talks to the local model server. llama-server and Ollama
// both speak the OpenAI chat completions API, so one client covers both and the
// only difference is a base URL (§3.2).
package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type Message struct {
	Role       string     `json:"role"` // system|user|assistant|tool
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Servers disagree on whether arguments are a JSON string or an
		// object, so it is decoded loosely and normalised by Args.
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// Args returns the call's arguments as a JSON object regardless of how the
// server encoded them.
func (c ToolCall) Args() json.RawMessage {
	raw := c.Function.Arguments
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return json.RawMessage(s)
		}
	}
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

type Request struct {
	Messages    []Message
	Tools       []ToolSpec
	Temperature float32
	MaxTokens   int
}

type Response struct {
	Content          string
	ToolCalls        []ToolCall
	PromptTokens     int
	CompletionTokens int
	Model            string
}

// Model is the whole surface the agent runtime needs. Anything larger is
// speculation about a server we have not run yet.
type Model interface {
	Chat(ctx context.Context, req Request) (Response, error)
	Name() string
}

type Client struct {
	BaseURL string // e.g. http://127.0.0.1:11434/v1
	ModelID string
	HTTP    *http.Client
}

// New reads the endpoint from the environment: THEORM_LLAMA_URL points at
// llama-server, THEORM_MODEL names the model (Ollama needs it, llama-server
// ignores it).
func New() *Client {
	base := os.Getenv("THEORM_LLAMA_URL")
	if base == "" {
		base = "http://127.0.0.1:8081"
	}
	model := os.Getenv("THEORM_MODEL")
	if model == "" {
		model = "local"
	}
	return &Client{
		BaseURL: base + "/v1",
		ModelID: model,
		HTTP:    &http.Client{Timeout: 10 * time.Minute},
	}
}

func (c *Client) Name() string { return c.ModelID }

func (c *Client) Chat(ctx context.Context, req Request) (Response, error) {
	body := map[string]any{
		"model":       c.ModelID,
		"messages":    req.Messages,
		"temperature": req.Temperature,
		"stream":      false,
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": t.Name, "description": t.Description, "parameters": t.Schema,
				},
			})
		}
		body["tools"] = tools
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("model server returned %d: %s", resp.StatusCode, snippet(raw))
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("decoding response: %w: %s", err, snippet(raw))
	}
	if len(out.Choices) == 0 {
		return Response{}, fmt.Errorf("model returned no choices: %s", snippet(raw))
	}
	m := out.Choices[0].Message
	return Response{
		Content: m.Content, ToolCalls: m.ToolCalls, Model: out.Model,
		PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens,
	}, nil
}

func snippet(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return string(b)
}
