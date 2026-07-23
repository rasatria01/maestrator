package memory

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rasatria01/theorm/internal/store"
)

const (
	maxIndexBytes = 512 << 10 // above this a file is generated or minified; skip it
	maxExports    = 40
	maxContent    = 1200
	embedBatch    = 256
)

type Indexer struct {
	Store *store.Store
	Embed *Embedder
}

// Index walks dir, derives one entity record per source file with the language's
// own tooling, embeds them on CPU, and writes them to L3 under repoID. It is
// deterministic and calls no model — the §5.9 cold-start pass.
func (ix *Indexer) Index(ctx context.Context, repoID, dir string) (int, error) {
	var recs []store.LTMEntity
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case d.IsDir():
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		case !indexable(d.Name()):
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > maxIndexBytes {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil || isBinary(src) {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		recs = append(recs, store.LTMEntity{Subject: "file:" + rel, Content: entity(rel, src)})
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(recs) == 0 {
		return 0, nil
	}
	// Embed in batches so a large repo does not build one enormous request.
	for i := 0; i < len(recs); i += embedBatch {
		j := min(i+embedBatch, len(recs))
		texts := make([]string, j-i)
		for k := range texts {
			texts[k] = recs[i+k].Content
		}
		vecs, err := ix.Embed.Embed(ctx, texts)
		if err != nil {
			return 0, err
		}
		for k, v := range vecs {
			recs[i+k].Embedding = v
		}
	}
	if err := ix.Store.ReplaceEntities(ctx, repoID, recs); err != nil {
		return 0, err
	}
	return len(recs), nil
}

// entity is the summary that goes into the record and gets embedded. Go files
// get real structure from go/ast; everything else gets a deterministic fallback
// until per-language tooling (tree-sitter) is worth it.
func entity(rel string, src []byte) string {
	if strings.HasSuffix(rel, ".go") {
		if s, ok := goEntity(rel, src); ok {
			return clip(s, maxContent)
		}
	}
	return clip(fallback(rel, src), maxContent)
}

func goEntity(rel string, src []byte) (string, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return "", false // unparseable: fall back rather than lose the file
	}
	var exports []string
	for _, decl := range f.Decls {
		if len(exports) >= maxExports {
			break
		}
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name.IsExported() {
				exports = append(exports, funcSig(fset, d))
			}
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				switch s := sp.(type) {
				case *ast.TypeSpec:
					if s.Name.IsExported() {
						exports = append(exports, "type "+s.Name.Name)
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.IsExported() {
							exports = append(exports, n.Name)
						}
					}
				}
			}
		}
	}
	imports := make([]string, 0, len(f.Imports))
	for _, imp := range f.Imports {
		imports = append(imports, strings.Trim(imp.Path.Value, `"`))
	}
	lines := bytes.Count(src, []byte{'\n'}) + 1
	var b strings.Builder
	fmt.Fprintf(&b, "%s — package %s, %d lines, %d exported.", rel, f.Name.Name, lines, len(exports))
	if len(exports) > 0 {
		fmt.Fprintf(&b, " exports: %s.", strings.Join(exports, "; "))
	}
	if len(imports) > 0 {
		fmt.Fprintf(&b, " imports: %s.", strings.Join(imports, ", "))
	}
	return b.String(), true
}

// funcSig prints a function's signature without its body or doc comment, on one
// line: "func (s *Store) Task(ctx context.Context, key string) (TaskRow, error)".
func funcSig(fset *token.FileSet, d *ast.FuncDecl) string {
	body, doc := d.Body, d.Doc
	d.Body, d.Doc = nil, nil
	var b strings.Builder
	err := printer.Fprint(&b, fset, d)
	d.Body, d.Doc = body, doc
	if err != nil {
		return "func " + d.Name.Name
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// fallback is what a non-Go (or unparseable) file gets: its size and first
// meaningful line, which catches a markdown title or a shebang for free.
func fallback(rel string, src []byte) string {
	lines := bytes.Count(src, []byte{'\n'}) + 1
	first := ""
	for _, ln := range strings.SplitN(string(src), "\n", 40) {
		if t := strings.TrimSpace(ln); t != "" {
			first = t
			break
		}
	}
	if len(first) > 200 {
		first = first[:200]
	}
	if first != "" {
		return fmt.Sprintf("%s — %d lines. %s", rel, lines, first)
	}
	return fmt.Sprintf("%s — %d lines", rel, lines)
}

var sourceExt = map[string]bool{
	".go": true, ".py": true, ".js": true, ".jsx": true, ".mjs": true,
	".ts": true, ".tsx": true, ".rs": true, ".java": true, ".rb": true,
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true, ".sh": true,
	".sql": true, ".md": true, ".yaml": true, ".yml": true, ".toml": true, ".json": true,
}

var sourceName = map[string]bool{"Makefile": true, "Dockerfile": true, "go.mod": true}

func indexable(name string) bool {
	return sourceExt[strings.ToLower(filepath.Ext(name))] || sourceName[name]
}

// skipDir mirrors the tools package: the same directories are noise to a grep
// and noise to the index.
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".next", "vendor", "__pycache__", ".venv":
		return true
	}
	return false
}

func isBinary(b []byte) bool {
	if len(b) > 8000 {
		b = b[:8000]
	}
	return bytes.IndexByte(b, 0) >= 0
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
