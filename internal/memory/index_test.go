package memory

import (
	"strings"
	"testing"
)

// The entity summary is what gets embedded and retrieved, so it must carry the
// structure a coder would want: package, exported signatures, imports — and
// never an unexported symbol, which would just be noise.
func TestGoEntity(t *testing.T) {
	src := []byte(`// Package auth mints tokens.
package auth

import (
	"crypto/subtle"
	"fmt"
)

type Config struct{ Secret string }

// NewService builds one.
func NewService(cfg Config) *Service { return nil }

func (s *Service) Validate(token string) error { return fmt.Errorf("no") }

func helper() {}
`)
	got := entity("internal/auth/token.go", src)
	for _, want := range []string{
		"internal/auth/token.go", "package auth",
		"func NewService(cfg Config) *Service",
		"func (s *Service) Validate(token string) error",
		"type Config", "crypto/subtle",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("entity missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "helper") {
		t.Errorf("entity leaked an unexported symbol:\n%s", got)
	}
}

// A file go/ast cannot parse must still produce a record, not vanish.
func TestFallbackEntity(t *testing.T) {
	md := entity("README.md", []byte("\n\n# TheORM\n\nMulti-agent orchestrator.\n"))
	if !strings.Contains(md, "README.md") || !strings.Contains(md, "# TheORM") {
		t.Errorf("markdown fallback: %q", md)
	}
	broken := entity("bad.go", []byte("package \n func ("))
	if !strings.Contains(broken, "bad.go") {
		t.Errorf("unparseable Go should fall back, got: %q", broken)
	}
}

func TestIndexable(t *testing.T) {
	for _, y := range []string{"main.go", "app.tsx", "schema.sql", "go.mod", "Makefile"} {
		if !indexable(y) {
			t.Errorf("%q should be indexable", y)
		}
	}
	for _, n := range []string{"logo.png", "a.exe", "notes.txt"} {
		if indexable(n) {
			t.Errorf("%q should not be indexable", n)
		}
	}
}
