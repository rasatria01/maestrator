---
theorm: v1
kind: mixed
repo: github.com/rasatria01/theorm
base: HEAD
title: "Example spec — format fixture for the A0 compiler"
authored_by: "rafiq"
authored_at: 2026-07-22
default_gates: [build]
policy:
  max_attempts: 2
  allow_tools: [read_file, write_file, str_replace, run_cmd]
  deny_paths: ["migrations/**", "docs/**"]
---

## Context

The orchestrator is a single Go binary in `cmd/theorm`. Out-of-process
dependencies are reachable over TCP/HTTP only; nothing is linked in.

```theorm:knowledge
mem_type: semantic
subject: "convention:go-layout"
content: "One binary at cmd/theorm. Packages live under internal/. External
          deps (postgres, llama-server, embed sidecar) are reached over the
          network and configured by THEORM_* environment variables."
confidence: 0.95
```

## Plan

```theorm:task
id: T1
title: "Report llama-server slot count and resident model in `theorm doctor`"
role: coder
depends_on: []
write_set: ["cmd/theorm/**", "internal/inference/**"]
read_set:
  subjects: ["file:cmd/theorm/", "convention:go-layout"]
  claim_kinds: [constraint, decision, failure]
resource_class: gpu_deep
est_context_tokens: 12000
acceptance:
  - kind: cmd
    run: "go build ./..."
    expect_exit: 0
  - kind: file_contains
    path: "cmd/theorm/main.go"
    pattern: "slots"
gates: [build]
```
