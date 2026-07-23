package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/rasatria01/theorm/internal/spec"
)

// Native returns the in-process tools (§8.1). MCP tools join the same registry
// at startup, namespaced by server.
func Native() []Tool {
	return append(Memory(), runCmd{}, readFile{}, listDir{}, grepTool{},
		writeFile{}, strReplace{}, gitCommit{}, taskComplete{})
}

const (
	cmdTimeout  = 300 * time.Second // §8.7
	maxCmdBytes = 2 << 20           // a runaway command should not fill the disk
)

// secretEnv names whose values never reach a subprocess. A denylist rather than
// an allowlist because an allowlist breaks every toolchain that reads an env
// var nobody predicted.
// ponytail: names only. Values that leak anyway are caught by the artifact
// redactor on the way to disk, which is the second layer.
var secretEnv = regexp.MustCompile(`(?i)(KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|AUTH|SESSION)`)

type runCmd struct{}

func (runCmd) Name() string { return "run_cmd" }
func (runCmd) Description() string {
	return "Run one allowlisted command in the task's working tree and return its output. " +
		"No shell: the command is split into arguments, so pipes, redirection and && do not work. " +
		"Allowed: go build/test/vet, gofmt, npm|pnpm run <script>, git add/commit/diff/status."
}
func (runCmd) Annotations() Annotations { return Annotations{Destructive: true} }
func (runCmd) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "cmd":             {"type": "string", "description": "e.g. go test ./internal/auth/..."},
	    "timeout_seconds": {"type": "integer", "minimum": 1, "maximum": 300}
	  },
	  "required": ["cmd"]
	}`)
}

func (runCmd) Execute(ctx context.Context, args json.RawMessage, env Env) (Output, error) {
	var in struct {
		Cmd     string `json:"cmd"`
		Timeout int    `json:"timeout_seconds"`
	}
	if err := decode(args, &in); err != nil {
		return Output{}, err
	}
	// The same allowlist the compiler checks a spec's acceptance commands
	// against, so what compiles is what may run.
	if err := spec.AllowedCmd(in.Cmd); err != nil {
		return Output{}, err
	}
	if env.Dir == "" {
		return Output{}, fmt.Errorf("no working directory for this task")
	}
	timeout := cmdTimeout
	if in.Timeout > 0 {
		timeout = time.Duration(in.Timeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv := strings.Fields(in.Cmd)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = env.Dir
	cmd.Env = filterEnv(os.Environ())

	out, err := cmd.CombinedOutput()
	if len(out) > maxCmdBytes {
		out = append(out[:maxCmdBytes], []byte("\n[truncated at 2 MiB]")...)
	}
	body := fmt.Sprintf("$ %s\n%s", in.Cmd, out)
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		body += fmt.Sprintf("\n[timed out after %s]", timeout)
	case err != nil:
		body += fmt.Sprintf("\n[%v]", err)
	default:
		body += "\n[exit status 0]"
	}
	return Output{Content: body, Kind: cmdKind(argv), MediaType: "text/plain"}, nil
}

// cmdKind picks the summariser that knows how to read this output (§5.5).
func cmdKind(argv []string) string {
	switch {
	case argv[0] == "go" && argv[1] == "test":
		return "test_output"
	case argv[0] == "git" && argv[1] == "diff":
		return "diff"
	}
	return "command_output"
}

func filterEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if name, _, ok := strings.Cut(kv, "="); ok && secretEnv.MatchString(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
