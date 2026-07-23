package store

import "testing"

// Summaries are deterministic: no model call, so nothing to hallucinate.
func TestSummariseIsDeterministicAndBounded(t *testing.T) {
	failing := `=== RUN   TestFoo
--- FAIL: TestFoo (0.01s)
    auth_test.go:88: nil pointer dereference
--- FAIL: TestBar (0.00s)
FAIL	github.com/x/y/internal/auth	0.12s
`
	s := Summarise("test_output", []byte(failing))
	for _, want := range []string{"2 failing", "TestFoo", "auth_test.go:88"} {
		if !contains([]string{s}, s) || !containsSub(s, want) {
			t.Errorf("summary %q missing %q", s, want)
		}
	}
	if Summarise("test_output", []byte(failing)) != s {
		t.Error("summary is not deterministic")
	}

	diff := `--- a/internal/auth/token.go
+++ b/internal/auth/token.go
@@
-old line
+new line
+another
`
	if got := Summarise("diff", []byte(diff)); !containsSub(got, "1 file(s) changed, +2 -1") {
		t.Errorf("diff summary wrong: %q", got)
	}

	long := make([]byte, 10000)
	for i := range long {
		long[i] = 'x'
	}
	if got := Summarise("file_content", long); len(got) > 300 {
		t.Errorf("summary is %d chars, must be <= 300", len(got))
	}
}

// F18: an artifact on disk with a live token in it is a real incident.
func TestRedactBeforeStoring(t *testing.T) {
	cases := []string{
		"aws key AKIAIOSFODNN7EXAMPLE here",
		"token ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"OPENAI_API_KEY=sk-abcdefghijklmnopqrstuvwxyz012345",
		`{"password": "hunter2hunter2"}`,
	}
	for _, c := range cases {
		if got := string(Redact([]byte(c))); !containsSub(got, "[REDACTED]") {
			t.Errorf("secret survived redaction: %q -> %q", c, got)
		}
	}
	clean := "func NewTokenService(cfg Config) *TokenService {"
	if got := string(Redact([]byte(clean))); got != clean {
		t.Errorf("redaction damaged clean content: %q", got)
	}
}

func containsSub(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
