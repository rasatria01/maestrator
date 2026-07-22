# TheORM

Multi-agent orchestrator with shared memory, adaptive concurrency and MCP tooling.
Design of record: [`docs/theorm-v2-design.md`](docs/theorm-v2-design.md).

## Layout

| Path | What |
|------|------|
| `cmd/theorm` | The orchestrator binary (§3.1) |
| `internal/` | Go packages: compiler, scheduler, memory, tools, inference (not written yet) |
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

Not yet: `theorm run --compiled` (needs the scheduler, Phase C), `--expand` hybrid mode,
and `theorm:knowledge` ingestion into L3 (Phase B5). Next up is Phase A1 — `memory_write`
and the context assembler.
