# TheORM

Multi-agent orchestrator with shared memory, adaptive concurrency and MCP tooling.
Design of record: [`docs/theorm-v2-design.md`](docs/theorm-v2-design.md).

## Layout

| Path | What |
|------|------|
| `cmd/theorm` | The orchestrator binary (§3.1) |
| `internal/spec` | Spec parser, validator and compiler — zero model calls |
| `internal/store` | Postgres: runs and tasks, L1 claims, L2 artifacts |
| `internal/tools` | Tool interface, capability filtering, result interception, `run_cmd` and the memory tools |
| `internal/prompt` | Context assembler: the §5.7 token budget and its overflow policies |
| `internal/role` | The §7.1 role table: capabilities, system prompt, temperature, step limit |
| `internal/inference` | Chat client for any OpenAI-compatible server (llama-server, Ollama) |
| `internal/agent` | The §7.2 step loop: one task, one attempt |
| `migrations/` | Postgres schema — runs, tasks, L1 claims, artifacts, L3 records, events |
| `services/embed/` | CPU-only embedding + rerank sidecar (§2.4) |
| `apps/dashboard/` | Next.js UI (§9) |
| `.theorm/specs/` | Executable specs, committed alongside the code they describe (§4.7) |
| `docs/` | Design documents |
| `testdata/` | Fixture repos for the eval harness (§10.3): `tiny-go`, `messy-go`, `mid-next` |

Three toolchains, three native package managers, no meta-build tool. `go build`
builds Go, `npm` builds the dashboard, `pip` installs the sidecar.

## Run it

```sh
docker compose up -d db                          # postgres 16 + pgvector, migrations on first boot
go run ./cmd/theorm doctor                       # checks postgres, llama-server, sidecar
conda run -n orchest python services/embed/main.py   # :8090, CPU only (fastembed needs py<=3.12)
corepack pnpm -C apps/dashboard dev               # :3000
```

Compile a spec — zero model calls. Prints the review report and emits a run of
pending tasks; `--check` validates without touching the database:

```sh
go run ./cmd/theorm compile --explain .theorm/specs/0000-example.md
```

llama-server is started outside this repo, with q8 KV as a hard requirement (§2.2):

```sh
llama-server -m qwen2.5-coder-14b-instruct-q4_k_m.gguf \
  -c 32768 --parallel 1 --cache-type-k q8_0 --cache-type-v q8_0 --port 8081
```

Config is environment variables: `THEORM_DATABASE_URL`, `THEORM_LLAMA_URL`, `THEORM_EMBED_URL`.
Postgres is published on **55432** — 5432 and 5433 are taken by native servers on this box.

## Status

Phase A0 (§12) done: the compiler parses and validates a spec, clamps capabilities by
intersection, enforces the write-set ceiling and command allowlist, detects drift,
projects waves, and emits a run of pending tasks in one transaction.

Phase A1 done: L1 the blackboard and L2 the artifact store, behind the three memory
tools. Enforcement lives in Go, never in a prompt — 12 claims per attempt, dedupe on
normalised content, contradictions supersede instead of overwrite, `fact` and `failure`
require evidence, secrets are redacted before content reaches disk.

Phase A2 done: the context assembler builds every prompt to a hard per-section budget
and degrades by a stated policy. Inspect any task's exact prompt with
`theorm debug-prompt --run <id> --task T1`.

Phase A3 done: every tool result is intercepted into the artifact store and reaches the
model as a summary plus a handle. 295 KB of `go test` output costs 21 tokens; the full
text stays one `artifact_read` away. `run_cmd` runs allowlisted commands only, argv-split
with no shell, with secret-shaped environment variables stripped.

Phase A4 done: the agent step loop and the read-only Explorer role. Tool errors go back to
the model as observations, narration gets one nudge then fails, and three identical calls
are a loop, not persistence. `read_file`, `list_dir` and `grep` resolve every agent-supplied
path against the worktree root and refuse anything that leaves it.

Phase A5 done: the coder's mutating tools. `write_file` and `str_replace` bound every edit
twice — the path resolves inside the worktree, and it must fall inside the task's declared
write set, so a model cannot widen its own reach by asking. `str_replace` demands a unique
match. `git_commit` stages only the write-set globs (`:(glob)` pathspecs) and refuses when
nothing in the set changed, so a commit never carries a file the task had no leave to touch.
`run_cmd`'s git is read-only (`diff`, `status`); `add` and `commit` are off the allowlist so
they cannot stage around the write set — mutation has one door, and it checks the write set.

Phase B1 done: cold-start L3 indexing. `theorm index <dir>` walks the repo and emits one
`entity` record per source file — Go files parsed with `go/ast` into package, exported
signatures, imports and line count; everything else a deterministic size-plus-first-line
fallback. The CPU sidecar embeds them (768-dim, zero VRAM) and they land in `ltm_records`
keyed by the repo's module path. Re-indexing is idempotent: delete-by-subject then insert,
in one transaction. No model is called — the whole pass is parsing.

Not yet: hybrid retrieval reads these back (B2), the Curator promotes L1 into L3 (B3), decay
and git-driven staleness (B4), and `theorm:knowledge` ingestion (B5). `theorm run --compiled`
needs the scheduler (Phase C). The `memory` and `repository` prompt sections are wired into
the budget but stay empty until B2 gives them a retrieval path.

The A4 measurement — does an Explorer make the coder read fewer files — needs a competent
executor model and the `messy-go` fixture. On llama3.2:3b the loop runs correctly and the
model is the limit: it never gets past `list_dir`, which is why §2.5 specifies a 14B coder.

Tests that need Postgres skip themselves when it is not up; `docker compose up -d db` first.
