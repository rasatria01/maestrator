// Package spec parses and compiles TheORM Spec Format files (design §4).
// Zero model calls: the same spec always produces the same task DAG (N7).
package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Spec struct {
	Path      string
	SHA256    string // provenance for revocation (§4.6 control 6)
	Meta      Meta
	Tasks     []Task
	Knowledge []Knowledge
	Prose     []Chunk
}

type Meta struct {
	Theorm       string   `yaml:"theorm"`
	Kind         string   `yaml:"kind"` // plan|knowledge|mixed
	Repo         string   `yaml:"repo"`
	Base         string   `yaml:"base"`
	Title        string   `yaml:"title"`
	AuthoredBy   string   `yaml:"authored_by"`
	AuthoredAt   string   `yaml:"authored_at"`
	DefaultGates []string `yaml:"default_gates"`
	Policy       Policy   `yaml:"policy"`
}

// Policy may only narrow role defaults, never widen them (§4.6 control 1).
type Policy struct {
	MaxAttempts int      `yaml:"max_attempts"`
	AllowTools  []string `yaml:"allow_tools"`
	DenyPaths   []string `yaml:"deny_paths"`
}

type Task struct {
	ID               string       `yaml:"id"`
	Title            string       `yaml:"title"`
	Role             string       `yaml:"role"`
	DependsOn        []string     `yaml:"depends_on"`
	WriteSet         []string     `yaml:"write_set"`
	ReadSet          ReadSet      `yaml:"read_set"`
	ResourceClass    string       `yaml:"resource_class"`
	EstContextTokens int          `yaml:"est_context_tokens"`
	EstSteps         int          `yaml:"est_steps"`
	Acceptance       []Acceptance `yaml:"acceptance"`
	Gates            []string     `yaml:"gates"`
	MaxAttempts      int          `yaml:"max_attempts"`
	Expand           bool         `yaml:"expand"` // §4.9 hybrid mode

	Line  int      `yaml:"-"`
	Tools []string `yaml:"-"` // effective set, filled by the capability clamp
}

type ReadSet struct {
	Subjects   []string `yaml:"subjects"`
	ClaimKinds []string `yaml:"claim_kinds"`
	Limit      int      `yaml:"limit"` // 0 uses the store default
}

// Acceptance is either a prose criterion (§6.1) or a typed one (§4.3).
type Acceptance struct {
	Kind       string `yaml:"kind"` // cmd|file_contains|browser|prose
	Run        string `yaml:"run"`
	ExpectExit *int   `yaml:"expect_exit"`
	Path       string `yaml:"path"`
	Pattern    string `yaml:"pattern"`
	URL        string `yaml:"url"`
	Steps      []any  `yaml:"steps"`
	Text       string `yaml:"-"`
}

func (a *Acceptance) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		a.Kind, a.Text = "prose", n.Value
		return nil
	}
	type plain Acceptance // avoid recursing into this method
	return n.Decode((*plain)(a))
}

type Knowledge struct {
	MemType    string  `yaml:"mem_type"`
	Subject    string  `yaml:"subject"`
	Content    string  `yaml:"content"`
	Confidence float32 `yaml:"confidence"`
	Line       int     `yaml:"-"`
}

// Chunk is untagged prose: L3 semantic memory at reduced confidence, never
// parsed for tasks. Silence is not consent (§4.3).
type Chunk struct {
	Heading string
	Text    string
	Line    int
}

// Diag is a compile error or warning, anchored to a line in the spec.
type Diag struct {
	Line int
	Msg  string
}

func (d Diag) String() string { return fmt.Sprintf("line %d: %s", d.Line, d.Msg) }

func ParseFile(path string) (*Spec, []Diag) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, []Diag{{0, err.Error()}}
	}
	sum := sha256.Sum256(raw)
	s, diags := Parse(string(raw))
	if s != nil {
		s.Path = path
		s.SHA256 = hex.EncodeToString(sum[:])
	}
	return s, diags
}

func Parse(src string) (*Spec, []Diag) {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	s := &Spec{}
	var diags []Diag

	i := 0
	if i < len(lines) && strings.TrimSpace(lines[i]) == "---" {
		start := i + 1
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "---"; i++ {
		}
		if i >= len(lines) {
			return nil, []Diag{{start, "front matter is not terminated by ---"}}
		}
		if err := yaml.Unmarshal([]byte(strings.Join(lines[start:i], "\n")), &s.Meta); err != nil {
			return nil, []Diag{{start, "front matter: " + err.Error()}}
		}
		i++
	} else {
		return nil, []Diag{{1, "spec must begin with YAML front matter (---)"}}
	}

	heading, prose, proseLine := "", []string{}, i+1
	flush := func() {
		text := strings.TrimSpace(strings.Join(prose, "\n"))
		if text != "" {
			s.Prose = append(s.Prose, Chunk{Heading: heading, Text: text, Line: proseLine})
		}
		prose, proseLine = nil, 0
	}

	for ; i < len(lines); i++ {
		line := lines[i]
		if fence, info, ok := openFence(line); ok {
			body, end := fenceBody(lines, i+1, fence)
			switch info {
			case "theorm:task":
				var t Task
				if err := yaml.Unmarshal([]byte(body), &t); err != nil {
					diags = append(diags, Diag{i + 1, "theorm:task: " + err.Error()})
				} else {
					t.Line = i + 1
					s.Tasks = append(s.Tasks, t)
				}
			case "theorm:knowledge":
				var k Knowledge
				if err := yaml.Unmarshal([]byte(body), &k); err != nil {
					diags = append(diags, Diag{i + 1, "theorm:knowledge: " + err.Error()})
				} else {
					k.Line = i + 1
					s.Knowledge = append(s.Knowledge, k)
				}
			case "":
			default:
				if strings.HasPrefix(info, "theorm:") {
					diags = append(diags, Diag{i + 1, "unknown block type " + info})
				}
			}
			i = end
			continue
		}
		if h, ok := strings.CutPrefix(line, "## "); ok {
			flush()
			heading, proseLine = strings.TrimSpace(h), i+2
			continue
		}
		if proseLine == 0 {
			proseLine = i + 1
		}
		prose = append(prose, line)
	}
	flush()
	return s, diags
}

// openFence reports the fence marker and info string of a code fence opener.
func openFence(line string) (fence, info string, ok bool) {
	t := strings.TrimLeft(line, " ")
	n := 0
	for n < len(t) && t[n] == '`' {
		n++
	}
	if n < 3 {
		return "", "", false
	}
	return t[:n], strings.TrimSpace(t[n:]), true
}

func fenceBody(lines []string, from int, fence string) (body string, end int) {
	var out []string
	for i := from; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimLeft(lines[i], " "), fence) {
			return strings.Join(out, "\n"), i
		}
		out = append(out, lines[i])
	}
	return strings.Join(out, "\n"), len(lines) - 1 // unterminated: treat rest as body
}
