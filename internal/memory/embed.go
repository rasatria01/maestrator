// Package memory is L3, long-term memory (§5.6): the cold-start index (B1) and,
// next, hybrid retrieval (B2). It leans on the CPU sidecar for embeddings so
// nothing here costs VRAM (§2.4).
package memory

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

// Embedder calls the CPU sidecar's /embed. One 768-float vector per text, in
// order — the same dimensionality as ltm_records.embedding.
type Embedder struct {
	URL  string
	HTTP *http.Client
}

func NewEmbedder() *Embedder {
	base := os.Getenv("THEORM_EMBED_URL")
	if base == "" {
		base = "http://127.0.0.1:8090"
	}
	return &Embedder{URL: base, HTTP: &http.Client{Timeout: 2 * time.Minute}}
}

func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	buf, err := json.Marshal(map[string]any{"texts": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL+"/embed", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed sidecar unreachable at %s: %w", e.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed sidecar returned %d: %s", resp.StatusCode, snippet(body))
	}
	var out struct {
		Vectors [][]float32 `json:"vectors"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decoding embed response: %w", err)
	}
	if len(out.Vectors) != len(texts) {
		return nil, fmt.Errorf("embed returned %d vectors for %d texts", len(out.Vectors), len(texts))
	}
	return out.Vectors, nil
}

func snippet(b []byte) string {
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}
