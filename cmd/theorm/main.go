// theorm — multi-agent orchestrator. Design: docs/theorm-v2-design.md
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

var version = "0.0.0-dev"

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "version":
		fmt.Println("theorm", version)
	case "doctor":
		os.Exit(doctor())
	default:
		fmt.Fprintln(os.Stderr, "usage: theorm <doctor|version>")
		os.Exit(2)
	}
}

// doctor reports whether the three out-of-process dependencies are reachable.
// ponytail: reachability only. Slot count, context per slot and VRAM (C1) land
// with the llama-server backend in internal/inference.
func doctor() int {
	checks := []struct{ name, kind, addr string }{
		{"postgres", "tcp", env("THEORM_DB_ADDR", "127.0.0.1:5432")},
		{"llama-server", "http", env("THEORM_LLAMA_URL", "http://127.0.0.1:8081") + "/health"},
		{"embed sidecar", "http", env("THEORM_EMBED_URL", "http://127.0.0.1:8090") + "/health"},
	}
	bad := 0
	for _, c := range checks {
		err := probe(c.kind, c.addr)
		mark := "ok"
		if err != nil {
			mark, bad = "FAIL: "+err.Error(), bad+1
		}
		fmt.Printf("%-14s %-40s %s\n", c.name, c.addr, mark)
	}
	return bad
}

func probe(kind, addr string) error {
	if kind == "tcp" {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(addr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
