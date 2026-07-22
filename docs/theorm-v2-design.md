# TheORM v2: Multi-Agent Orchestrator with Shared Memory, Adaptive Concurrency, and MCP Tooling

**Version:** 2.1 (design)
**Date:** 22 July 2026
**Status:** Draft for review
**Supersedes:** TheORM v1.0 spec (planner, queue, agent tool loop, gate, reviewer, DECISIONS.md distiller)
**Changed in 2.1:** specifications authored in a cloud session become the primary input (§4). The cloud model moves from a runtime dependency to a compile-time one, and a normal run makes zero cloud calls.
**Target host:** single workstation, RTX 5060 Ti 16GB, Go orchestrator, Postgres, Next.js dashboard

---

## 0. How to read this document

v1 built a linear pipeline: Claude plans, a Postgres queue dispatches, one local agent edits code in a worktree, deterministic gates run, a local reviewer votes, the orchestrator merges. That works, but it has three structural gaps that this document closes.

1. **Agents cannot see each other's findings.** v1's only shared state is a `DECISIONS.md` file appended by the distiller. If the coder discovers that a package is vendored, the reviewer has no way to know.
2. **Concurrency is a config flag, not a decision.** v1 has `workers=1` with worktree code written but unused. There is no model of when parallel is safe.
3. **The agent has seven hand-written tools.** Nothing can drive a browser, so front-end work is unverifiable.
4. **The plan is produced under the worst possible conditions.** v1 calls a cloud model at runtime, with no human reviewing the output, inside a context window, twice per goal. The same model used at design time, with you arguing with it, produces a better plan and costs nothing at runtime. §4 makes that the primary path.

Every section below is written so that no capability is asserted without stating where its state lives, who writes it, who reads it, what happens when it conflicts, and what it costs in VRAM. Where a real choice exists I state the default, the alternative, and the reason, rather than silently picking one. Section 1.3 collects every such choice into a single register so you can flip any of them without rereading the document.

---

## 1. Requirements and assumption register

### 1.1 Functional requirements (from the brief)

| ID | Requirement |
|----|-------------|
| R1 | Multiple specialised agents cooperate on one engineering goal |
| R2 | Agents share **short-term memory** scoped to the current run |
| R3 | Agents share **long-term memory** that persists across runs |
| R4 | Execution is **sequential by default**, because accuracy matters more than wall-clock |
| R5 | The orchestrator may **choose** parallelism, with a chosen degree N, when it is safe |
| R6 | Everything runs on one RTX 5060 Ti 16GB |
| R7 | A UI shows live progress of the agentic task |
| R8 | The UI can **manage** the task, not only display it |
| R9 | Agents can call external tools over MCP, specifically Chrome DevTools MCP for web work |
| R10 | A design document authored externally, in a cloud chat session, can be downloaded, committed, and executed by the orchestrator without a runtime planner call |

### 1.2 Non-functional requirements

| ID | Requirement | Measurable form |
|----|-------------|-----------------|
| N1 | Local-first | Zero cloud calls during a run. In compiled mode (§4.9) a run starts and finishes with the network unplugged. |
| N2 | Crash-recoverable | `kill -9` on the orchestrator at any point leaves no task permanently stuck. |
| N3 | Auditable | Every file byte written by an agent traces back to a task, attempt, prompt, and model response. |
| N4 | Bounded cost | Cloud calls per goal are capped by an atomic counter. Zero in compiled mode, at most 2 in goal mode. |
| N7 | Reproducible | The same spec compiles to a byte-identical task DAG. Execution varies, planning does not. |
| N8 | Reviewable before execution | Every command, path, and memory write is shown to a human before the first agent step (§4.4). |
| N5 | Bounded context | No prompt exceeds the executor model's window. Assembly is deterministic, not best-effort. |
| N6 | Safe by default | No agent can write outside its worktree, run a non-allowlisted command, or reach the network except through declared MCP servers. |

### 1.3 Assumption register

These are decisions I made because the brief did not specify them. Each is reversible. Flip any one and tell me, and the affected sections are noted.

| # | Assumption | Default chosen | Alternative | Why the default | Affects |
|---|-----------|----------------|-------------|-----------------|---------|
| A1 | Where the cloud model is used | **Design time only.** You author a spec in a chat session, commit it, and the orchestrator compiles it with zero model calls | Runtime planner, as in v1 and v2.0, retained as `--goal` mode | Same model, better conditions: unbounded context, a human arguing with it, and the plan reviewed before any code is written | §4, §7 |
| A2 | Long-term memory is per-repository | Yes, keyed by repo identity | Global across all repos | Cross-repo transfer of "lessons" is usually wrong. Conventions are project-local | §5 |
| A3 | Vector store | pgvector inside the same Postgres | Qdrant as a separate service | One process to back up, one to recover, one transaction boundary. You already know Qdrant, so this is an operational choice, not a capability one | §5.6 |
| A4 | Sparse retrieval | Postgres `tsvector` + GIN | FastEmbed BM25 sparse vectors | Keeps the whole memory system inside one transaction. Same RRF fusion pattern as your existing work | §5.6 |
| A5 | Embedding and reranking run on CPU | Yes, ONNX runtime | GPU | The GPU is a single indivisible resource held by the generator. See §2.4 | §2.4, §5.6 |
| A6 | Parallel tasks get isolated git worktrees | Yes | Shared checkout with file locks | Locks on a shared tree deadlock and the failure mode is corrupted source. Worktrees fail cleanly at merge | §6.5 |
| A7 | Default execution mode | Sequential, one model resident, one slot | Parallel by default | R4, and §2.3 shows the hardware agrees | §2.3, §6.4 |
| A8 | Browser access is a leased exclusive resource | Yes, one holder at a time | One Chrome MCP server per worker | Chrome DevTools MCP has global selected-page state. See §8.4 | §8.4 |
| A9 | Vision (screenshots interpreted by a model) | Off by default | Swap in a VLM at designated phases | A 7B VLM cannot be co-resident with a 14B coder. See §8.6 | §8.6 |
| A10 | Human approval gates | On for merge to the default branch only | On for every task, or off entirely | Enough control to trust it overnight, not enough to make it useless | §9.4 |
| A11 | Memory writes require verification | Yes, unverified claims are stored but down-weighted | Trust all agent writes | Unverified writes are how long-term memory poisons itself. See §5.8 | §5.8 |
| A12 | Event transport to the UI | SSE from Go, commands over REST | WebSocket both ways | Traffic is 99% one-directional. SSE reconnects for free | §9.5 |
| A13 | Where specs live | `.theorm/specs/`, committed to the repository | A database table, or an uploads directory | Plans version with the code they describe, and get diff, blame, and pull request review for free | §4.7 |
| A14 | Trust level of an incoming spec | **Untrusted.** Schema-validated, capability-clamped, human-approved at compile time | Trusted, since you wrote it | You will eventually paste one you did not write, and its knowledge blocks persist in L3 indefinitely | §4.6 |

---

## 2. The hardware budget, because it determines the architecture

Everything about the concurrency design falls out of one number. Work this section first.

### 2.1 What 16GB actually gives you

The RTX 5060 Ti 16GB reports roughly 15.9 GiB total. Subtract the display framebuffer, the CUDA context, and cuBLAS workspace and the honest working figure is **14.0 to 14.5 GiB**. Design against 14.0 GiB and you will not be debugging OOM at 2am.

The card's relevant strength is memory bandwidth. <cite index="22-1">It has 448 GB/s of GDDR7, which is 56 percent more than the 288 GB/s of the RTX 4060 Ti 16GB it replaces</cite>, and for inference <cite index="22-1">memory bandwidth is the single number that determines tokens per second</cite>. Measured throughput for a 14B class model at Q4 lands around <cite index="21-1">51 t/s</cite>, with more conservative estimates near 32 t/s. Call it 30 to 50 tokens per second and plan agent step latency accordingly.

### 2.2 The VRAM equation

```
VRAM_total = weights + KV_cache + compute_buffers

KV_bytes_per_token = 2 × n_layers × n_kv_heads × head_dim × bytes_per_element
                     ^                                      ^
                     K and V                                2 for fp16, 1 for q8_0
```

Compute the number for your chosen model before you choose a context length. Worked example, Qwen2.5-Coder-14B (48 layers, 8 KV heads via GQA, head_dim 128):

```
2 × 48 × 8 × 128 × 2 bytes = 196,608 bytes = 192 KiB per token
```

| Context | KV at fp16 | KV at q8_0 |
|---------|-----------|-----------|
| 16k | 3.0 GiB | 1.5 GiB |
| 32k | 6.0 GiB | 3.0 GiB |
| 64k | 12.0 GiB | 6.0 GiB |

With Q4_K_M weights at about 9.0 GiB:

- 32k context, fp16 KV: 9.0 + 6.0 + 0.6 = **15.6 GiB. Does not fit.**
- 32k context, q8_0 KV: 9.0 + 3.0 + 0.6 = **12.6 GiB. Fits, with 1.4 GiB spare.**

This is not a micro-optimisation, it is the difference between the system working and not working. <cite index="25-1">Q8 KV cache quantization has become standard practice on this card, set in llama.cpp with `--cache-type-k q8_0 --cache-type-v q8_0`, halving KV memory with no measurable quality loss, and on a 16GB card it is the difference between fitting a 14B model at 32K context and running out of VRAM at 16K.</cite>

**Make q8_0 KV a hard requirement in config, not an option.**

### 2.3 The central consequence: parallelism does not duplicate weights

The naive assumption is that running two agents at once needs two model instances, so 18 GiB, so it is impossible. That is wrong, and the correction is the load-bearing idea of this design.

A single inference server holds **one copy of the weights** and *n* independent KV cache slots. Two agents running concurrently against the same server cost you one set of weights plus two KV allocations. In llama.cpp's server, `--parallel N` creates N slots and the `-c` total context is divided among them.

So the real trade is not *sequential or parallel*. It is:

> **Total KV budget is fixed. You spend it either on one deep context or on several shallow ones.**

With 14.0 GiB usable and a 14B model at Q4:

| Mode | Model | Slots | Context per slot | Weights | KV (q8) | Total | Use for |
|------|-------|-------|-----------------|---------|---------|-------|---------|
| **Quality** | 14B Q4_K_M | 1 | 32k | 9.0 | 3.0 | 12.6 | Coding, review, anything needing repo context |
| **Balanced** | 14B Q4_K_M | 2 | 16k | 9.0 | 3.0 | 12.6 | Two small independent tasks |
| **Throughput** | 7B Q5_K_M | 4 | 16k | 5.4 | 1.75 | 7.8 | Summarising, classifying, doc writing, test scaffolding |

The 7B row uses different architecture numbers (28 layers, 4 KV heads, head_dim 128 gives 56 KiB per token) and leaves over 6 GiB spare, which is why it is the honest parallel mode.

**Conclusion:** sequential-by-default is not merely your accuracy preference, the hardware independently arrives at the same answer for the tasks that matter. Parallelism becomes correct only when tasks are (a) shallow enough for a 16k window, or (b) not GPU-bound at all. Both conditions are detectable at plan time, which is what makes §6.4 implementable.

### 2.4 Partition the machine, do not share it

The single most effective resource decision available to you:

| Workload | Device | Reason |
|----------|--------|--------|
| Token generation (all agents) | **GPU, exclusively** | Indivisible, latency-critical, the bottleneck |
| Embedding generation | **CPU**, ONNX, bge-m3 or nomic-embed-text | Runs during agent thinking time at zero VRAM cost |
| Cross-encoder reranking | **CPU**, ONNX, bge-reranker-base | Reranking 50 candidates is ~200ms on CPU, and it is off the critical path |
| BM25 / full-text search | **CPU**, Postgres GIN | Free |
| Gates (build, vet, test, lint) | **CPU** | Already true in v1 |
| Chrome | **CPU + integrated display** | Real memory pressure, zero VRAM if you do not screenshot into a VLM |

You already built a CPU-only hybrid retrieval stack for the recommendation engine. That experience transfers directly, and here the CPU-only constraint is a feature rather than a limitation, because it keeps the GPU pinned to generation.

### 2.5 Model selection for the executor role

Verify all of these on your own card before committing. Vendor and blog VRAM figures routinely omit KV cache.

| Model | Weights (Q4_K_M) | Realistic max context on 16GB | Verdict |
|-------|-----------------|-------------------------------|---------|
| **Qwen2.5-Coder 14B** | ~9.0 GiB | 32k with q8 KV | **Recommended default.** Known-good, already in your v1 spec, leaves headroom for Chrome and Postgres |
| Qwen2.5-Coder 7B | ~4.7 GiB (Q5: 5.4) | 64k, or 4×16k slots | The throughput-mode model |
| Devstral Small 2 24B | ~14.3 GiB | Roughly 4k after KV. **Does not fit usefully** | Marketed as a 16GB model. The KV math says otherwise unless you drop to IQ3, which costs code quality. Measure, do not trust the label |
| Qwen3-Coder 30B-A3B (MoE) | ~19 GiB | Requires CPU offload | <cite index="20-1">MoE rows scale by active parameters because decode reads only the active experts, so a 35B MoE runs far faster than a dense 32B, and models exceeding 16GB degrade less under CPU offload because hot experts stay GPU-resident.</cite> Highest ceiling, most tuning risk. Treat as an experiment, not the default |

Run the selection as a measurement, not a preference. §10.3 defines the harness that decides it.

---

## 3. System architecture

### 3.1 Process topology

```
┌──────────────────────────────────────────────────────────────────────┐
│  Next.js dashboard  :3000                                            │
│  Run DAG · live logs · diff viewer · memory browser · controls       │
└───────────────┬──────────────────────────────────┬───────────────────┘
                │ SSE /api/events                  │ REST commands
┌───────────────▼──────────────────────────────────▼───────────────────┐
│  theorm (Go, single binary)  :8080                                   │
│                                                                      │
│  ┌────────────────┐  ┌──────────────┐  ┌────────────────────────┐ │
│  │ Spec Compiler  │  │  Scheduler   │  │  Resource Manager      │ │
│  │ .theorm/specs/ │→ │  wavefronts  │→ │  GPU admission · slots │ │
│  │ 0 model calls  │  │  conflicts   │  │  browser lease · swap  │ │
│  ├────────────────┤  └──────┬───────┘  └────────────┬───────────┘ │
│  │ Planner (opt.) │         │                       │             │
│  │ --goal mode    │         │                       │             │
│  └────────────────┘         │                       │             │
│                         │                        │                   │
│  ┌──────────────────────▼────────────────────────▼───────────────┐   │
│  │  Agent Runtime  (1..N concurrent, N from Resource Manager)    │   │
│  │  context assembler → LLM step loop → tool dispatch → verify   │   │
│  └────┬──────────────────────────────────┬───────────────────────┘   │
│       │                                  │                           │
│  ┌────▼─────────────┐            ┌───────▼──────────┐                │
│  │  Memory Service  │            │  Tool Layer      │                │
│  │  STM · LTM · art │            │  native + MCP    │                │
│  └────┬─────────────┘            └───────┬──────────┘                │
└───────┼──────────────────────────────────┼──────────────────────────┘
        │                                  │
┌───────▼────────┐  ┌──────────────┐  ┌────▼──────────┐  ┌───────────┐
│  Postgres 16   │  │  llama-server│  │ Chrome DevTools│  │ Artifact  │
│  + pgvector    │  │  GPU, slots  │  │ MCP (stdio)    │  │ store     │
│  + tsvector    │  │              │  │                │  │ (disk)    │
└────────────────┘  └──────────────┘  └────────────────┘  └───────────┘
        │
┌───────▼────────────────────┐
│  CPU sidecar (Python)      │
│  embeddings + reranker     │
│  ONNX, no GPU              │
└────────────────────────────┘
```

### 3.2 Why llama-server rather than Ollama

v1 used Ollama, which is the right call for getting started. For v2 the deciding factor is explicit slot control.

| Concern | Ollama | llama-server |
|---------|--------|--------------|
| Parallel slots | `OLLAMA_NUM_PARALLEL`, allocates `num_ctx × num_parallel` KV, easy to OOM by accident | `--parallel N -c TOTAL`, total is divided across slots, explicit |
| KV quantization | Supported but awkward to pin per-model | `--cache-type-k q8_0 --cache-type-v q8_0`, a required flag |
| Model residency | `OLLAMA_MAX_LOADED_MODELS`, may evict under memory pressure without telling you | You control load and unload |
| Slot introspection | Not exposed | `/slots` endpoint reports which slots are busy |

That last row is what the Resource Manager needs. **Recommendation:** keep the Ollama client interface in Go (`internal/inference`) with two implementations, ship llama-server as the default, keep Ollama as the fallback for when you want to swap models by name quickly. The interface is small: `Chat(ctx, req) (stream, error)` plus `Capabilities() (slots int, ctxPerSlot int, model string)`.

### 3.3 Request path for one agent step

```
Scheduler picks task
  → Resource Manager grants a GPU slot (blocking semaphore, capacity = N)
    → Context Assembler builds the prompt under a hard token budget (§5.7)
        · system prompt for role
        · goal + this task's contract
        · STM blackboard slice, filtered to this task's read-set
        · LTM retrieval results (hybrid, CPU, §5.6)
        · last K tool results, summarised
      → llama-server /v1/chat/completions, temperature per role, tools attached
        → model emits tool_call
          → Tool Layer validates against role capability set
            → executes (native or MCP)
              → result stored as artifact, summary returned to context
        → loop until task_complete or step limit
    → release GPU slot
  → Gate pipeline (CPU, no slot held)
  → Reviewer (needs a slot again)
  → Memory Curator promotes verified findings to LTM
  → Integrator merges worktree
```

Note where the slot is held and released. A task blocked on `go test` must not hold a GPU slot. This single rule roughly doubles utilisation in sequential mode, because gates are often the longest step.

---

## 4. Specification ingestion: the cloud-to-local handoff

### 4.1 The idea

The most valuable thing about a cloud model in this system is not that it can write code. It is that it has an enormous context window, no VRAM budget, and a human sitting next to it who can argue with it. That is a *design-time* asset, and v2's original design wasted it by calling the cloud model at runtime, under time pressure, with no human in the loop, twice per goal.

The correction: **author the plan in a cloud session, review it, commit it to the repository, and let the orchestrator compile it.** The cloud model becomes a compile-time dependency instead of a runtime one.

| Property | Runtime planner (v2.0) | Authored spec (v2.1) |
|----------|----------------------|---------------------|
| Cloud calls during a run | 2 | **0** |
| Human review of the plan | None | Before any code is written, which is the highest-leverage review point in the system |
| Reproducibility | Same goal, different plan (F16) | Same spec, byte-identical DAG |
| Plan size ceiling | What fits in one planner response | Unbounded. A 40-task epic is fine |
| Versioning | Ephemeral, lives in a database row | A file in git, with diff, blame, and pull request review |
| What the cloud model sees | Repository context you send it | Only what you paste, which is a real privacy property |
| N1 local-first | True after planning | Literally true |

The cost is that you have to author specs. That is real work, but it is work you are already doing when you sit in a chat session reasoning about a change. This just makes the output of that session executable.

### 4.2 Three roles a document can play, which must not be conflated

A markdown file arriving at the orchestrator can be any of three things, and they run through different pipelines:

| Role | Becomes | Pipeline | Model call |
|------|---------|----------|-----------|
| **Executable plan** | A task DAG | Deterministic compiler | None |
| **Project knowledge** | L3 semantic memory | Chunk, embed on CPU, store | None |
| **Goal description** | Input to a planner | Planner, local or cloud | Yes |

One file can serve all three at once, which is the normal case. The design document you are reading is mostly *knowledge*, its section 12 build plan is nearly an *executable plan*, and its section 13 is a *goal description*. The format in §4.3 makes the distinction explicit rather than inferring it, because inferring it is exactly the kind of guess that produces a run doing something you did not ask for.

### 4.3 TheORM Spec Format

A markdown superset. Prose is for humans and is ignored by the compiler unless explicitly tagged. Fenced blocks carry the contracts.

````markdown
---
theorm: v1
kind: mixed                       # plan | knowledge | mixed
repo: github.com/rasatria01/myapp
base: 8f3c2a1b                    # commit this spec was authored against
title: "Dark mode with persistence"
authored_by: "rafiq + claude-opus-4.8"
authored_at: 2026-07-22
default_gates: [lint, build, test]
policy:                           # may only NARROW defaults, never widen (§4.6)
  max_attempts: 3
  allow_tools: [read_file, write_file, str_replace, run_cmd, git_commit]
  deny_paths: ["internal/legacy/**", ".github/**"]
---

## Context

The settings page lives at `app/settings` and has no persistence layer today.
Global client state uses React context rather than a store library, following
the pattern in `app/lib/*/provider.tsx`.

```theorm:knowledge
mem_type: semantic
subject: "convention:state-management"
content: "Global client state uses React context, not Zustand or Redux.
          Providers live at app/lib/<domain>/provider.tsx and are composed
          in app/layout.tsx."
confidence: 0.95
```

## Plan

The provider has to exist before anything can consume it, so T1 is a hard
dependency. T2 and T3 touch disjoint trees and can run together.

```theorm:task
id: T1
title: "Add theme context provider with localStorage persistence"
role: coder
depends_on: []
write_set: ["app/lib/theme/**"]
read_set:
  subjects: ["file:app/lib/", "convention:state-management"]
  claim_kinds: [constraint, decision, failure]
resource_class: gpu_deep
est_context_tokens: 22000
acceptance:
  - kind: cmd
    run: "npm run build"
    expect_exit: 0
  - kind: file_contains
    path: "app/lib/theme/provider.tsx"
    pattern: "createContext"
gates: [lint, build]
```

```theorm:task
id: T4
title: "Verify toggle persists across reload"
role: web_verifier
depends_on: [T1, T2, T3]
write_set: []
resource_class: browser
acceptance:
  - kind: browser
    url: "http://localhost:{{dev_port}}/settings"
    steps:
      - snapshot
      - click: {text: "Dark mode"}
      - eval: "localStorage.getItem('theme')"
        expect: "dark"
      - reload
      - snapshot_assert: {role: switch, name: "Dark mode", checked: true}
      - console_errors: 0
```
````

Design choices worth stating:

- **One validator, two producers.** The `theorm:task` schema is byte-identical to what the planner emits in §6.1. The compiler and the planner share the same Go struct and the same validation function. This is what keeps the two paths from diverging.
- **Prose is not a fallback input.** Untagged prose under a heading is ingested as `semantic` memory at reduced confidence (0.6) and is never parsed for tasks. If you want something executed, you write a block. Silence is not consent.
- **Rationale and contract live in the same file** on purpose. Split them and they drift within two edits. Keeping them together means a pull request reviewing the plan shows both why and what.
- **`{{dev_port}}` and a small set of substitutions** are resolved by the orchestrator at compile time, because hardcoding port 3000 in a spec guarantees F15.

### 4.4 The compiler

`theorm compile <spec.md>` produces a validated run, with zero model calls.

```
1. Parse front matter          → schema validation, format version check
2. Extract theorm:task blocks  → per-block schema validation, line numbers on failure
3. Build the DAG               → acyclic, all depends_on resolve, no orphan tasks
4. Write-set analysis          → pairwise glob intersection, precomputed
                                 (this makes §6.4 Rule 2 a table lookup, not a computation)
5. Capability clamping         → §4.6
6. Base commit resolution      → drift check, §4.5
7. Context feasibility         → any task whose est_context_tokens exceeds the
                                 largest available slot fails compilation NOW,
                                 not 20 minutes into a run
8. Extract theorm:knowledge    → L3 records with spec provenance
9. Chunk untagged prose        → L3 semantic records at confidence 0.6
10. Emit                       → runs row, tasks rows in `pending`, compile report
```

Compilation fails loudly and completely. A partially-valid spec produces a report with line numbers and no run, because a half-executed plan is worse than no plan.

```
$ theorm compile .theorm/specs/2026-07-22-dark-mode.md --explain

  ✓ format v1, 4 tasks, 2 knowledge records, 3 prose chunks
  ✓ DAG acyclic, single entry (T1), single exit (T4)
  ✓ base 8f3c2a1b resolves, HEAD is 8f3c2a1b, no drift
  ✓ capability clamp: coder 11 tools → 6 (spec policy narrows)
  ✓ context feasibility: max 22000 est vs 32000 available in quality mode

  Projected execution:
    wave 1  [T1]        sequential   gpu_deep      quality mode   ~22k ctx
    wave 2  [T2, T3]    parallel×2   gpu_shallow   throughput     ~11k ctx each
    wave 3  [T4]        sequential   browser       lease required

  Commands that will run:  npm run build, npm run lint, npm test
  Paths that will be written:  app/lib/theme/**, app/settings/**, app/api/settings/**
  Memory records that will be written:  2 (human_confirmed), 3 (spec_prose, 0.6)

  Estimated: 4 tasks, 3 waves, ~18 min. No cloud calls.

  Run with: theorm run --compiled <run_id>
```

That `--explain` output is the single most useful command in the system. It answers "will this do what I think" before you spend 18 minutes finding out.

### 4.5 Drift

A spec is authored against a repository state and executed later. Pin it:

```
front matter:  base: 8f3c2a1b
```

At compile time:

| Condition | Verdict | Action |
|-----------|---------|--------|
| `HEAD == base` | Clean | Proceed |
| `HEAD != base`, changed files do not intersect any task's `write_set` or `read_set` | Clean drift | Proceed, note in report |
| `HEAD != base`, changed files intersect | **Conflicting drift** | Report which tasks and which files. Require `--accept-drift` or a UI confirmation |
| `base` does not resolve (rebased, squashed, force-pushed) | Hard fail | Re-anchor the spec, because its assumptions are unverifiable |

Drift interacts well with §5.8's mechanism 4: files that changed since `base` already have stale entity memories, so retrieval degrades honestly rather than confidently serving outdated file summaries.

### 4.6 A spec is untrusted input

This is the part that needs care, and it is not paranoia. You are building a machine that reads a file and then runs commands on your workstation. That file needs a schema and a review step, the same as any other input that crosses a trust boundary. The reasons it genuinely is a boundary:

- You will eventually paste a spec from a colleague, a GitHub issue, or a repository you did not write.
- A cloud model co-authored parts of it, and that model may have read content that influenced it.
- Its knowledge blocks go into L3, where they persist across every future run. A bad spec is not a bad run, it is a permanently degraded memory.

Six controls, all enforced in Go, none of them in a prompt:

1. **Capability clamping is intersection, never union.** `effective_tools = role_capabilities ∩ spec.policy.allow_tools`. A spec can narrow a role's tools and add path denials. It can never grant a tool the role does not already have, and it can never remove a deny rule from config.

2. **The command allowlist is not negotiable by the spec.** An acceptance criterion of `kind: cmd` is checked against the §8.7 allowlist at *compile* time. A spec containing `curl … | sh` fails to compile. It does not fail at runtime, by which point it is a question of whether the sandbox held.

3. **Write-set ceiling.** Reject a bare `**`, any path resolving outside the repository root, and more than 20 globs per task. A task that wants to write everywhere is a task that has not been thought through.

4. **Prose enters agent context as data, never as instruction.** Knowledge chunks and prose chunks are injected in the L3 retrieval section, inside an explicit delimiter, and every role's system prompt states that retrieved memory is reference material while instructions come only from the task contract. This is the same rule that already applies to tool output, extended to a new input surface.

5. **Compile-time human review.** The `--explain` report in §4.4 is the review artifact: every command that will run, every path that will be written, every memory record that will be created, before anything executes. One approval click. Twenty seconds. This is where a bad spec gets caught.

6. **Provenance for revocation.** Records written from a spec carry `verified_by = 'spec:<sha256 of the file>'`. If a spec turns out to be wrong, `UPDATE ltm_records SET status='retired' WHERE verified_by = 'spec:abc123…'` removes everything it contributed, in one statement.

### 4.7 Where specs live

In the repository, at `.theorm/specs/`, committed.

```
.theorm/
  specs/
    2026-07-22-dark-mode.md
    2026-07-24-auth-refresh.md
  reports/
    run-8f2a-dark-mode.md          # emitted by §4.8, also committed
  config.yaml
```

The argument for in-repo rather than in a database: the spec and the code it describes version together, `git blame` works on plans, and a pull request can review a plan before a line of code exists. It also gives the orchestrator a free source of episodic memory, because prior specs touching the same paths are exactly the right thing to retrieve when planning a related change.

### 4.8 The round trip

The loop closes if the orchestrator can hand you back something worth taking to the next design session.

```
theorm report <run_id> --format spec-delta > .theorm/reports/run-8f2a.md
```

The report is markdown, written for a model to read, containing:

- **Planned versus actual.** Which tasks needed retries, how many, and the `failure` claims that explain why.
- **Write-set accuracy.** Files actually touched versus declared. Systematic under-declaration means your specs are too optimistic and the scheduler is being denied parallelism it could have had.
- **Ambiguous acceptance criteria.** Detected mechanically: a task that passed its gates but failed review, or that needed a human hint, had a criterion that did not mean what it appeared to mean.
- **Memory promoted.** What the run learned, so the next spec does not restate it.
- **Context pressure.** Which tasks came within 10 percent of their budget, which is the leading indicator that a task should have been two tasks.

You paste that report into the next cloud session. The model never sees your codebase, only a structured account of what happened, and the next spec is measurably better. **Design in the cloud, execute locally, report back, refine.**

### 4.9 Three run modes, not two

Spec and planner are not mutually exclusive.

| Mode | Command | Cloud calls | Use when |
|------|---------|-------------|----------|
| **Compiled** | `theorm run --spec f.md` | 0 | You know the decomposition. Fully deterministic and offline |
| **Hybrid** | `theorm run --spec f.md --expand` | 0 to 1 | You know the shape but not the leaves. Tasks marked `expand: true` are decomposed further by the **local** 14B, with the repository map from L3 in front of it |
| **Goal** | `theorm run --goal "…"` | 1 to 2 | Exploratory work, small changes, or you want to see what the planner proposes |

Hybrid is likely your daily mode. You author the phase structure and the hard dependencies, where human judgement is worth the most, and let a local model fill in leaf tasks, where it is competent because the surrounding structure is already fixed.

### 4.10 Bootstrapping

Once Phase A of §12 works, Phases B through E of this document can be rewritten as specs in `.theorm/specs/` and executed by TheORM itself. That is a real milestone with a real demo, and it is the honest test of whether the format is expressive enough: if you cannot express your own build plan in it, it is not ready for anyone else's.

The document you are reading now is deliberately *not* in this format, because it is a design document rather than a plan. Say the word and I can emit the Phase A and Phase B milestones as compilable `.theorm/specs/` files.

---

## 5. Memory architecture

This is the heart of the request, so it gets the most detail. The design goal is that an agent starting a task inherits everything relevant that any other agent has learned, without inheriting the noise, and without any agent being able to write a falsehood that outlives the run.

### 5.1 Four tiers, with strict roles

| Tier | Name | Scope | Lifetime | Storage | Written by |
|------|------|-------|----------|---------|-----------|
| **L0** | Context window | One model call | Milliseconds | The prompt itself | Context Assembler only |
| **L1** | Short-term memory (blackboard) | One run | Hours, then archived | Postgres `stm_claims` | Any agent, via typed tool |
| **L2** | Artifact store | One run, referenced later | Retained per policy | Disk, content-addressed | Tool Layer, automatically |
| **L3** | Long-term memory | The repository | Indefinite, with decay | Postgres + pgvector + tsvector | Curator only |

The strictness matters. Three rules that are easy to state and prevent most failure modes:

1. **L0 is never a source of truth.** It is rebuilt from L1, L2, and L3 on every single call. There is no "conversation history" that agents append to and carry forward. If a fact is not in L1 or L3, it did not happen.
2. **Large content never enters L0 by value.** It goes to L2 and enters as a handle plus a summary. A 4,000-line test log becomes `artifact://a1b2c3 (test failure, 3 failing cases in pkg/auth, first error: nil pointer at auth.go:88)`.
3. **Only the Curator writes L3.** Agents propose. Nothing an agent says becomes durable knowledge without passing §5.8.

### 5.2 Why a blackboard and not a shared chat log

The obvious design for "shared short-term memory" is one message list all agents append to. Do not do this. On a 16k to 32k window it fails within about four agent turns, and it fails in the worst way: silently, by truncating the oldest content, which is usually the goal statement.

A blackboard is a set of **typed, addressable claims**. Agents read a filtered slice of it, not the whole thing. The Context Assembler can then make a principled decision about what to include, because claims have types and relevance scores, whereas chat messages have only recency.

```go
type Claim struct {
    ID          string       // uuid
    RunID       string
    TaskID      string       // which task produced it
    AttemptID   string
    Kind        ClaimKind    // see table below
    Subject     string       // canonical key, e.g. "file:internal/auth/token.go"
    Predicate   string       // e.g. "depends_on", "fails_test", "convention"
    Object      string       // the value, <= 500 chars, else an artifact ref
    Confidence  float32      // 0.0 to 1.0, agent-asserted
    Verified    Verification // see §5.8
    Provenance  Provenance   // agent role, model, tool that produced it
    SupersededBy *string     // append-only correction chain
    CreatedAt   time.Time
}
```

| ClaimKind | Meaning | Example |
|-----------|---------|---------|
| `fact` | Observed state of the world | `file:go.mod` `declares_module` `github.com/x/y` |
| `decision` | A choice made, with reason | `task:T3` `chose` `sqlc over gorm; repo already vendors sqlc` |
| `constraint` | A rule later tasks must obey | `run` `must_not_modify` `internal/legacy/**` |
| `finding` | Something discovered while working | `pkg/auth` `has_no_tests` `coverage 0%` |
| `failure` | A thing that was tried and did not work | `attempt:A7` `failed_because` `import cycle auth -> config -> auth` |
| `question` | An unresolved ambiguity | `task:T5` `unclear` `which env var holds the signing key` |
| `handoff` | Explicit message to a named downstream role | `role:reviewer` `note` `intentionally left TODO at line 42, see task T9` |

The `failure` kind is the highest-value one and the one most systems omit. A parallel agent repeating a failure a sibling already hit is the single largest waste of GPU time in a multi-agent system.

### 5.3 The write path

Agents write to L1 through one tool, not through prose.

```json
{
  "name": "memory_write",
  "description": "Record a durable finding other agents should know. Use for facts you verified, decisions you made and why, constraints you discovered, and approaches that failed. Do not use for narration of what you are about to do.",
  "input_schema": {
    "kind":       {"enum": ["fact","decision","constraint","finding","failure","question","handoff"]},
    "subject":    {"type": "string", "maxLength": 200},
    "predicate":  {"type": "string", "maxLength": 60},
    "object":     {"type": "string", "maxLength": 500},
    "confidence": {"type": "number", "minimum": 0, "maximum": 1},
    "evidence":   {"type": "string", "description": "artifact id or tool call id that supports this"}
  },
  "annotations": {"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true}
}
```

Enforcement points, all in Go, none in the prompt:

- **Rate limit:** maximum 12 claims per attempt. A model that writes 40 claims is narrating, not observing. Exceeding the limit returns an error the model can see, which usually corrects the behaviour within one step.
- **Deduplication:** claims are hashed on `(kind, subject, predicate, normalise(object))`. A duplicate is a no-op that returns "already known", which is itself useful signal to the model.
- **Contradiction detection:** a new claim with the same `(subject, predicate)` but different `object` does not overwrite. It creates a new claim and sets `SupersededBy` on the old one, and emits a `memory.contradiction` event that the UI surfaces. Both remain readable, which matters when the new one turns out to be the wrong one.
- **Evidence requirement:** `kind` in (`fact`, `failure`) requires a non-empty `evidence` field referencing a real tool call in this attempt. This is the cheapest anti-hallucination control in the whole system.

### 5.4 The read path

An agent does not read the whole blackboard. Each task declares a **read-set** at plan time, and the Context Assembler resolves it.

```yaml
task:
  id: T4
  role: coder
  read_set:
    - kind: constraint          # always, all of them, they are short and binding
      scope: run
    - kind: handoff
      scope: role:coder
    - kind: [decision, finding, failure]
      scope: task_ancestors     # only tasks this one depends on, transitively
      limit: 20
      order: confidence desc, created_at desc
    - kind: fact
      scope: subject_prefix
      match: ["file:internal/auth/", "pkg:auth"]
```

`task_ancestors` is the important scope. In sequential mode it is "everything before me". In parallel mode it deliberately excludes sibling tasks running concurrently, because their claims are unverified and mid-flight. Siblings become visible after they complete and their claims are verified, at the next wavefront boundary. This is what makes parallel execution memory-safe rather than a race.

### 5.5 Artifact store (L2)

Content-addressed, on disk, referenced everywhere, inlined nowhere.

```
/var/lib/theorm/artifacts/
  <run_id>/
    sha256-a1b2c3....txt      # test output
    sha256-d4e5f6....diff     # a patch
    sha256-789abc....json     # network request log from Chrome
    sha256-def012....png      # screenshot
```

```sql
CREATE TABLE artifacts (
  id           TEXT PRIMARY KEY,         -- sha256
  run_id       UUID NOT NULL REFERENCES runs(id),
  task_id      UUID REFERENCES tasks(id),
  kind         TEXT NOT NULL,            -- test_output|diff|screenshot|a11y_snapshot|network_log|console_log|file_content
  media_type   TEXT NOT NULL,
  size_bytes   BIGINT NOT NULL,
  path         TEXT NOT NULL,
  summary      TEXT NOT NULL,            -- <= 300 chars, generated at write time
  created_at   TIMESTAMPTZ DEFAULT now()
);
```

The `summary` column is generated **at write time**, not at read time. Two strategies depending on kind:

- **Deterministic** (preferred): test output gets a parsed summary via regex over the framework's output format, diffs get files-changed plus line counts, console logs get error counts by level. No model call, no cost, no hallucination.
- **Model-generated** (fallback): for unstructured content, one call to the 7B in throughput mode. Cheap, and it happens off the critical path.

An agent that needs the full content calls `artifact_read(id, offset, limit)` and pays for it out of its own context budget, having made an informed choice. That is the correct place for the decision.

### 5.6 Long-term memory (L3)

Four memory types, distinct because they are retrieved differently.

| Type | Content | Retrieval trigger | Example |
|------|---------|-------------------|---------|
| **Semantic** | How this repository works | Every task, always | "HTTP handlers live in internal/api and are registered in router.go" |
| **Episodic** | What happened in past runs | Similar goal detected | "Run 41 added a /metrics endpoint; the plan was 3 tasks; it took 2 retries because the Prometheus registry is a package-level singleton" |
| **Procedural** | Distilled how-to lessons | Task kind matches | "When adding a route, also update the OpenAPI spec in api/openapi.yaml or the contract test fails" |
| **Entity** | Per-file and per-symbol summaries | Read-set matches | "internal/auth/token.go: issues and validates JWTs, depends on config.Signing, 210 lines, 4 exported symbols" |

#### Schema

```sql
CREATE TABLE ltm_records (
  id           UUID PRIMARY KEY,
  repo_id      TEXT NOT NULL,                    -- A2: scoped per repository
  mem_type     TEXT NOT NULL,                    -- semantic|episodic|procedural|entity
  subject      TEXT NOT NULL,                    -- canonical key, same namespace as L1 subjects
  content      TEXT NOT NULL,                    -- the retrievable text, <= 2000 chars
  metadata     JSONB NOT NULL DEFAULT '{}',      -- {files:[], symbols:[], task_kinds:[]}

  -- provenance and trust
  source_run   UUID REFERENCES runs(id),
  source_claim UUID,                             -- the L1 claim it was promoted from
  verified_by  TEXT NOT NULL,                    -- gate_pass|review_pass|human|tool_observation
  confidence   REAL NOT NULL CHECK (confidence BETWEEN 0 AND 1),

  -- lifecycle
  hit_count    INT NOT NULL DEFAULT 0,
  last_hit_at  TIMESTAMPTZ,
  contradicted_count INT NOT NULL DEFAULT 0,
  status       TEXT NOT NULL DEFAULT 'active',   -- active|stale|retired
  valid_until  TIMESTAMPTZ,                      -- NULL means no expiry

  -- retrieval
  embedding    VECTOR(768),                      -- bge-m3 or nomic-embed-text, CPU-generated
  content_tsv  TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,

  created_at   TIMESTAMPTZ DEFAULT now(),
  updated_at   TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX ltm_embedding_idx ON ltm_records USING hnsw (embedding vector_cosine_ops);
CREATE INDEX ltm_tsv_idx       ON ltm_records USING gin (content_tsv);
CREATE INDEX ltm_lookup_idx    ON ltm_records (repo_id, mem_type, status);
CREATE INDEX ltm_subject_idx   ON ltm_records (repo_id, subject);
```

#### Retrieval: hybrid, on CPU

This is a direct port of the architecture you already built, which is a good reason to use it: you know its failure modes.

```
query (task title + description + read-set subjects)
   │
   ├──► dense: pgvector cosine, top 30           ─┐
   │                                              ├──► RRF fusion (k=60) ──► top 20
   ├──► sparse: tsvector ts_rank_cd, top 30      ─┘                            │
   │                                                                           ▼
   └──► exact: subject prefix match, always included    CPU cross-encoder rerank ──► top 6
                                                        (bge-reranker-base, ONNX)      │
                                                                                       ▼
                                                                            fits the L0 budget
```

The exact-match lane matters and is often omitted. If the task's read-set names `internal/auth/token.go`, the entity memory for that exact file must appear regardless of what the embeddings think. Vector search is for the things you did not know to ask for.

Reranking 60 candidates on CPU costs roughly 200 to 400ms with a base-size cross-encoder. That is free, because it happens while the GPU is finishing the previous task's generation.

```sql
-- RRF fusion, single query
WITH dense AS (
  SELECT id, ROW_NUMBER() OVER (ORDER BY embedding <=> $1) AS rank
  FROM ltm_records
  WHERE repo_id = $2 AND status = 'active'
  ORDER BY embedding <=> $1 LIMIT 30
),
sparse AS (
  SELECT id, ROW_NUMBER() OVER (ORDER BY ts_rank_cd(content_tsv, query) DESC) AS rank
  FROM ltm_records, plainto_tsquery('english', $3) query
  WHERE repo_id = $2 AND status = 'active' AND content_tsv @@ query
  ORDER BY ts_rank_cd(content_tsv, query) DESC LIMIT 30
)
SELECT COALESCE(d.id, s.id) AS id,
       COALESCE(1.0/(60 + d.rank), 0) + COALESCE(1.0/(60 + s.rank), 0) AS rrf
FROM dense d FULL OUTER JOIN sparse s USING (id)
ORDER BY rrf DESC LIMIT 20;
```

### 5.7 Context assembly: a fixed budget, spent deliberately

This is where most agent systems quietly break. The prompt must be built to a **hard token budget** with per-section allocations, and overflow must be handled by a stated policy rather than by truncation at the end.

Budget for the 14B in quality mode, 32k window:

| Section | Budget | Overflow policy |
|---------|--------|-----------------|
| System prompt (role, rules, output contract) | 800 | Never truncated. If it does not fit, that is a bug |
| Goal + task contract + acceptance criteria | 700 | Never truncated |
| Constraints from L1 | 500 | Never truncated. Drop a lower-priority section instead |
| L1 claims (read-set resolved) | 2,500 | Drop lowest confidence first, then oldest |
| L3 retrieval (top 6 reranked) | 3,000 | Drop lowest rerank score first |
| Repository context (file outlines, not bodies) | 6,000 | Degrade from body to outline to path-only |
| Tool results this attempt (summarised) | 8,000 | Sliding window, oldest replaced by its own summary |
| Scratchpad / prior turns this attempt | 4,000 | Compress turns older than the last 3 into a single summary |
| **Reserve for output** | 4,000 | Hard reserve, never allocated to input |
| Safety margin | 2,500 | Absorbs tokenizer variance between estimate and actual |
| **Total** | **32,000** | |

Two implementation notes that save real pain:

- **Estimate high.** Use `len(text)/3.2` as the token estimate for code rather than the usual /4. Code tokenizes worse than prose. Over-estimating costs you a little unused window, under-estimating costs you a mid-generation truncation.
- **Reference over content.** For repository context, send symbol outlines by default (`func NewTokenService(cfg Config) *TokenService` with a one-line doc), and let the agent call `read_file` when it needs a body. This roughly triples the number of files an agent can be aware of within the same budget.

### 5.8 Promotion, verification, and not poisoning yourself

The failure mode that kills long-term memory in agentic systems: an agent asserts something false, it gets written, and it is retrieved into every future run as established fact. Confidence in the store does not decay just because the claim was wrong. Guard it with three mechanisms.

**Mechanism 1: verification levels.** Every claim carries how it came to be believed.

| Level | Meaning | Retrieval weight | Eligible for L3 |
|-------|---------|-----------------|-----------------|
| `tool_observed` | Directly follows from a tool result (file exists, test failed, HTTP 500 returned) | 1.0 | Yes |
| `gate_passed` | The change containing this claim compiled and passed tests | 1.0 | Yes |
| `review_passed` | The reviewer agent independently agreed | 0.8 | Yes |
| `human_confirmed` | You clicked approve in the UI | 1.0 | Yes, and immune to decay |
| `asserted` | The model said so | 0.3 | **No** |
| `contradicted` | A later claim disagreed | 0.1 | No, and existing L3 record is marked `stale` |

**Only `tool_observed`, `gate_passed`, `review_passed`, and `human_confirmed` are promotable.** An agent's unsupported assertion can inform the current run at low weight and then dies with it. This is assumption A11 and it is the one I would push back hardest on changing.

**Mechanism 2: the Curator.** A distinct role that runs once at end of run, not during it.

```
Input:  all L1 claims for the run, the final diff, gate results, review verdict
Output: 0 to 10 proposed L3 records

Rules enforced in Go, not in the prompt:
  1. Only promotable verification levels are eligible
  2. Reject if a near-duplicate exists (cosine > 0.92 against same repo_id and mem_type)
     → instead increment hit_count and refresh updated_at on the existing record
  3. Reject if it contradicts an active record with higher confidence
     → emit memory.conflict event, park it for human review in the UI
  4. Cap at 10 records per run. If the model proposes more, take the top 10 by confidence
  5. Episodic records are always written (one per run, automatically, not model-decided)
```

The cap is deliberate. A system that writes 50 memories per run has a retrieval problem within two weeks. Ten good records per run is 500 records after 50 runs, which is a well-behaved corpus for hybrid retrieval.

**Mechanism 3: decay and retirement.** A nightly job:

```sql
-- Records nothing has retrieved in 60 days lose confidence
UPDATE ltm_records
SET confidence = confidence * 0.9
WHERE status = 'active'
  AND (last_hit_at IS NULL OR last_hit_at < now() - INTERVAL '60 days')
  AND verified_by <> 'human';

-- Records that fall below the floor retire
UPDATE ltm_records SET status = 'retired'
WHERE status = 'active' AND confidence < 0.25;

-- Entity memories whose file no longer exists go stale immediately (checked against git)
UPDATE ltm_records SET status = 'stale'
WHERE mem_type = 'entity' AND subject = ANY($1);  -- deleted paths from git diff
```

Retired records are kept, not deleted, and excluded from retrieval. When something goes wrong you will want to see what the system used to believe.

**Mechanism 4: staleness by code change.** The highest-value invalidation signal is free. Every merged commit gives you a list of changed files. Any `entity` memory whose subject is a changed file is marked stale and queued for re-derivation. Any `procedural` memory whose metadata references a changed file drops confidence by 0.2. Code changes, memory follows, without anyone asking a model.

### 5.9 Bootstrapping cold memory

A fresh repository has empty L3, which makes the first several runs poor. Fix this with a one-time indexing pass that is deterministic and needs no model:

1. Walk the repo, respecting `.gitignore`.
2. For each source file, parse with the language's own tooling (`go/ast` for Go, `tree-sitter` otherwise) and emit an `entity` record: path, package, exported symbols with signatures, imports, line count.
3. Read `README.md`, `CONTRIBUTING.md`, `docs/**`, and any `AGENTS.md` or `CLAUDE.md`, chunk them, and emit `semantic` records.
4. Parse `go.mod`, `package.json`, `Makefile`, CI config, and emit `semantic` records for build and test commands. These are the highest-value memories in the entire system and they are pure parsing.
5. Embed everything on CPU. A 500-file repository takes a few minutes and blocks nothing.

Add `theorm index <repo>` as a command, run it on first use and on demand, and make the dashboard show record counts by type so you can tell at a glance whether memory is healthy.

---

## 6. Orchestration and the sequential-versus-parallel decision

### 6.1 The task contract

Tasks are contracts the scheduler can reason about mechanically, never prose steps. There are two producers of this structure, the spec compiler (§4.4) and the planner, and **they share one Go struct and one validation function**. That shared validator is what keeps the compiled path and the planned path from drifting apart. Every field below exists because the scheduler or the context assembler needs it.

```yaml
- id: T4
  title: "Add JWT refresh endpoint"
  role: coder                      # selects prompt, model, tools, temperature
  depends_on: [T2, T3]

  # --- what it may touch: this is what makes parallelism decidable ---
  write_set:                       # globs the task is permitted to modify
    - "internal/auth/**"
    - "internal/api/routes.go"
  read_set:                        # memory + repo scope (see §5.4)
    subjects: ["file:internal/auth/", "pkg:auth"]
    claim_kinds: [constraint, decision, finding, failure, handoff]

  # --- what it costs ---
  resource_class: gpu_deep         # gpu_deep | gpu_shallow | cpu_only | browser
  est_context_tokens: 24000
  est_steps: 12

  # --- what "done" means ---
  acceptance:
    - "POST /auth/refresh returns 200 with a new token for a valid refresh token"
    - "returns 401 for an expired refresh token"
    - "go test ./internal/auth/... passes"
  gates: [fmt, vet, build, test]

  # --- failure handling ---
  max_attempts: 3
  on_exhausted: escalate_to_planner   # escalate_to_planner | escalate_to_human | fail_run
```

`write_set` is mandatory and the planner prompt must be explicit that a task with no declared write set will be rejected. It is the single field that turns "should these run in parallel" from a judgement call into a set intersection.

### 6.2 State machine

```
pending ──claim──► running ──agent ok──► gating ──pass──► reviewing ──pass──► merging ──► done
   ▲                  │                     │                  │                 │
   │                  │ agent error         │ fail             │ fail            │ conflict
   │                  ▼                     ▼                  ▼                 ▼
   └──backoff──── retrying ◄────────────────┴──────────────────┘            conflict_hold
                      │                                                          │
                      │ attempts exhausted                                       ▼
                      ▼                                                    (human resolves)
                  escalated ──► (replan | human) ──► pending | failed
```

Two operational requirements on this machine:

- **Claiming is atomic.** `UPDATE tasks SET state='running', worker_id=$1, lease_until=now()+interval '15 min' WHERE id = (SELECT id FROM tasks WHERE state='pending' AND ... FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING *`. `SKIP LOCKED` is what makes multiple workers safe without a separate lock service.
- **Leases, not liveness checks.** A worker renews its lease every 60 seconds. A crashed worker's task returns to `retrying` when the lease expires. This is how N2 is satisfied and it is about 30 lines of code.

### 6.3 Wavefront scheduling

The scheduler runs a loop, not a queue drain:

```
loop:
  ready := tasks where state=pending and all depends_on are done
  if ready is empty and nothing is running: run is complete (or deadlocked, check for cycles)
  wave := decide_parallelism(ready)      // §6.4, returns an ordered list of batches
  for batch in wave:
      execute batch concurrently, degree = len(batch)
      wait for all to reach a terminal state
      // barrier: sibling claims become visible to the next batch here
      curator.verify_and_publish(batch claims)
  goto loop
```

The barrier between batches is not incidental. It is what makes §5.4's `task_ancestors` scope correct: within a batch, siblings cannot see each other's mid-flight claims, and at the barrier their verified claims become available to everyone downstream. Concurrency without this barrier gives you agents reading each other's half-formed conclusions.

### 6.4 The parallelism decision function

This is R5 made concrete. The scheduler answers "sequential or parallel, and at what degree" with an ordered set of rules, not a heuristic score.

```go
func (s *Scheduler) decideParallelism(ready []Task) [][]Task {
    // Rule 0: global override
    if s.cfg.Mode == ModeSequential { return oneEach(ready) }

    // Rule 1: only one candidate, nothing to decide
    if len(ready) == 1 { return [][]Task{ready} }

    // Rule 2: write-set conflict forces serialization.
    // Two tasks whose write globs intersect must never run together.
    groups := partitionByWriteSetOverlap(ready)   // union-find over glob intersection
    // tasks within a group are mutually conflicting -> sequential within the group

    // Rule 3: classify by resource class
    //   cpu_only  : unlimited concurrency (bounded by GOMAXPROCS)
    //   browser   : degree 1, requires the browser lease (A8, §8.4)
    //   gpu_deep  : requires a full 32k slot -> degree 1 in quality mode
    //   gpu_shallow: fits 16k -> may share slots

    // Rule 4: GPU admission. Ask the Resource Manager what is available NOW.
    //   available = mgr.FreeSlots()   // from llama-server /slots
    //   never oversubscribe: queueing at the inference server destroys latency
    //   and makes step timeouts fire spuriously

    // Rule 5: accuracy guard. Never parallelize a task whose
    //   est_context_tokens > ctxPerSlot in the current mode.
    //   Better to run it alone with a full window than in parallel with a
    //   truncated one. This is R4 encoded as a rule rather than a preference.

    // Rule 6: mode switch. If every ready task is gpu_shallow and there are
    //   >= 3 of them, propose switching the resident model to throughput mode
    //   (7B, 4 slots). Model swap costs ~10s of load time, so require the
    //   batch to be big enough to amortise it: len(batch) * est_steps >= 20.
    //   Swaps happen ONLY at wavefront barriers, never mid-batch.

    // Rule 7: cap. degree = min(conflictFreeCount, freeSlots, cfg.MaxDegree)
}
```

Rules 5 and 6 are the ones that satisfy the brief's actual intent. The system is sequential because deep tasks need the full window, and it becomes parallel precisely when the work stops needing a deep window. That is a defensible engineering position rather than a knob.

**Worked example.** Goal: "Add a settings page with a dark mode toggle that persists to localStorage."

| Wave | Ready tasks | Decision | Why |
|------|------------|----------|-----|
| 1 | T1 explore repo structure | Sequential, degree 1 | Single task |
| 2 | T2 build settings API, T3 build settings UI component | **Sequential** | Write sets do not overlap, but both are `gpu_deep` at 22k and 24k estimated. Rule 5 fires |
| 3 | T4 write API tests, T5 write component tests, T6 update README | **Parallel, degree 3, throughput mode** | All `gpu_shallow` (under 12k), disjoint write sets, 3 tasks over the amortisation threshold. Rule 6 fires, swap to 7B |
| 4 | T7 verify in browser | Sequential, degree 1, browser lease | `resource_class: browser` |

### 6.5 Isolation and integration

Each task in a parallel batch runs in its own git worktree.

```
theorm-work/
  <run_id>/
    T4/   git worktree add ../theorm-work/<run>/T4 -b theorm/<run>/T4 <base_sha>
    T5/
    T6/
```

Integration order and rules:

1. Tasks merge in **dependency order**, never in completion order. A task that finishes first still waits for its ancestors.
2. Merge is `git merge --no-ff` onto the run's integration branch, not onto your default branch. The default branch is touched once, at the end, behind the approval gate (A10).
3. A merge conflict moves the task to `conflict_hold` and emits an event. Do not have a model resolve merge conflicts. It is the operation with the worst ratio of confidence to correctness in the entire system. Surface it in the UI with both sides and let a human decide, or re-run the task rebased onto the new base.
4. After each merge, gates run **again** on the integration branch. Two changes that each pass in isolation can fail together, and this is common enough that skipping it will burn you.
5. Worktree cleanup is idempotent and runs on startup, because crashed runs leave them behind.

### 6.6 Retry and escalation ladder

Every rung is cheaper than the one above it. Never skip a rung.

| Attempt | Action | Context change |
|---------|--------|---------------|
| 1 | Normal execution | Fresh context |
| 2 | Retry with condensed failure feedback | Fresh context plus a `failure` claim from attempt 1 plus the gate output tail. **Do not resume the old conversation**, a 16k window cannot afford it |
| 3 | Retry with reduced scope | The planner is not called. The orchestrator narrows the acceptance criteria to the first failing one and tells the agent to fix only that |
| 4 | `escalate_to_planner` | One cloud call, the second and last of the budget. Input: the goal, the task, all three failures, the current diff. Output: a revised sub-DAG replacing this task |
| 5 | `escalate_to_human` | Task moves to `blocked`, UI shows a card with the diff, all failures, and buttons: retry with hint, edit acceptance criteria, skip, abort run |

The "reduced scope" rung at attempt 3 is worth building. A surprising share of failures are a model doing three things right and one thing wrong, and telling it to only do the wrong one succeeds without spending a cloud call.

---

## 7. Agent roles

### 7.1 Role table

| Role | Model / mode | Temp | Context | Tools | Writes code | Writes L1 |
|------|-------------|------|---------|-------|-------------|-----------|
| **Planner** | Claude, cloud | 0.2 | n/a | none | No | Constraints only, at plan time |
| **Explorer** | 14B quality | 0.1 | 32k | read-only: `read_file`, `grep`, `list_dir`, `memory_read`, `memory_write` | **No** | Yes, heavily |
| **Coder** | 14B quality | 0.1 | 32k | full: edit, run_cmd, git, memory | Yes | Yes |
| **Web Verifier** | 14B quality | 0.1 | 32k | Chrome MCP subset + `read_file` + memory | No | Yes |
| **Reviewer** | 14B quality | 0.1 | 32k | read-only + `memory_read` | No | Findings only |
| **Test Writer** | 7B throughput | 0.2 | 16k | edit within `**/*_test.go`, run_cmd (test only) | Tests only | Yes |
| **Summariser** | 7B throughput | 0.3 | 16k | none | No | No |
| **Curator** | 7B throughput | 0.1 | 16k | `memory_read`, `ltm_propose` | No | Proposes L3 |

The **Explorer** is a v2 addition and the highest-return one. It is a read-only agent that runs first, cannot break anything, and populates L1 with facts the coder would otherwise spend half its context window discovering. On a constrained window, separating "find out" from "change things" is worth more than any prompt tuning.

### 7.2 The step loop

```
for step := 0; step < role.MaxSteps; step++ {
    prompt := assembler.Build(task, role, memory, artifacts)   // §5.7, hard budget
    resp   := inference.Chat(prompt, role.Tools, role.Temp)

    if resp.HasToolCalls() {
        for _, call := range resp.ToolCalls {
            if !role.Capabilities.Allows(call.Name) {
                record ToolResult{Error: "tool not available to this role"}
                continue                       // let the model correct itself
            }
            if err := validateArgs(call); err != nil {
                record ToolResult{Error: actionableMessage(err)}
                continue
            }
            result := tools.Execute(ctx, call)  // §8
            artifact := artifacts.Store(result) // §5.5, always
            record ToolResult{Summary: artifact.Summary, Ref: artifact.ID}
        }
        continue
    }

    if resp.CallsTaskComplete() { break }

    // no tool call and no completion: the model is narrating.
    // one nudge, then fail the attempt.
    if narrationStrikes++; narrationStrikes > 1 {
        return ErrAgentStalled
    }
}
```

Three deliberate choices in that loop:

- **Tool errors are returned to the model, not raised.** A denied tool or a bad argument becomes an observation the model can react to. This recovers roughly half of what would otherwise be failed attempts.
- **Every tool result becomes an artifact.** No exceptions, including successful ones. This is what makes N3 auditability real rather than aspirational.
- **Narration is a failure.** Small models will happily emit "Now I will read the file" without calling `read_file`, forever. One nudge, then stop, because the alternative is a task that burns 40 steps producing nothing.

### 7.3 Structured output

Do not parse prose. Use the inference server's grammar constraint (`response_format` with a JSON schema, or a GBNF grammar in llama.cpp) for every structured decision: the reviewer's verdict, the curator's proposals, the planner's DAG. A 7B model with a constrained grammar produces valid JSON essentially always. The same model asked politely for JSON produces valid JSON perhaps 85 percent of the time, and the 15 percent will cost you more debugging time than the feature saved.

---

## 8. Tool layer and MCP

### 8.1 Two kinds of tools, one interface

```go
type Tool interface {
    Name() string
    Schema() json.RawMessage
    Annotations() ToolAnnotations       // readOnly, destructive, idempotent, openWorld
    Execute(ctx context.Context, args json.RawMessage, env TaskEnv) (ToolResult, error)
}
```

**Native tools** (Go, in-process, v1 already has most of these): `read_file`, `write_file`, `str_replace`, `list_dir`, `grep`, `run_cmd`, `git_commit`, plus v2's `memory_read`, `memory_write`, `artifact_read`.

**MCP tools** (external processes over stdio or streamable HTTP): discovered at startup via `tools/list`, wrapped in the same interface, namespaced by server (`chrome.take_snapshot`).

The wrapper is thin but does three things the raw MCP client does not:

1. **Namespacing**, so two servers cannot collide on a tool name.
2. **Result interception**, so every result goes to the artifact store and only a summary reaches the context. This is mandatory for browser tools, where a single snapshot can exceed the entire context budget.
3. **Capability filtering**, so a role only sees the tools it is allowed to call. The reviewer should not be shown `write_file` at all, rather than being told not to use it. Models respect an absent tool far more reliably than a prohibited one.

### 8.2 Capability sets

```yaml
capabilities:
  explorer:  [read_file, list_dir, grep, memory_read, memory_write, artifact_read]
  coder:     [read_file, write_file, str_replace, list_dir, grep, run_cmd,
              git_commit, memory_read, memory_write, artifact_read, task_complete]
  web_verifier:
             [read_file, memory_read, memory_write, artifact_read, task_complete,
              chrome.navigate_page, chrome.wait_for, chrome.take_snapshot,
              chrome.click, chrome.fill, chrome.fill_form, chrome.press_key,
              chrome.list_console_messages, chrome.list_network_requests,
              chrome.get_network_request, chrome.evaluate_script,
              chrome.resize_page, chrome.emulate, chrome.take_screenshot]
  reviewer:  [read_file, list_dir, grep, memory_read, artifact_read, submit_verdict]
```

Note what the web verifier does **not** get: no `write_file`, no `run_cmd`. It observes and reports. If it finds a bug, it writes a `finding` claim and the orchestrator schedules a coder task. Splitting observation from mutation is what makes the browser loop debuggable.

### 8.3 Chrome DevTools MCP

The official server from the Chrome DevTools team is the right choice, and it is worth knowing its actual shape before designing around it.

Tool surface, grouped as the project groups them: <cite index="7-1">Input automation provides click, drag, fill, fill_form, handle_dialog, hover, press_key, type_text, upload_file and click_at. Navigation automation provides close_page, list_pages, navigate_page, new_page, select_page and wait_for. Emulation provides emulate and resize_page. Performance provides performance_analyze_insight, performance_start_trace and performance_stop_trace. Network provides get_network_request and list_network_requests. Debugging provides evaluate_script, get_console_message, lighthouse_audit, list_console_messages, take_screenshot and take_snapshot.</cite>

Configuration:

```json
{
  "mcpServers": {
    "chrome": {
      "command": "npx",
      "args": ["chrome-devtools-mcp@latest", "--isolated"]
    }
  }
}
```

Four operational facts that shape the design:

1. <cite index="11-1">The browser starts automatically on the first tool call that requires it, using a persistent Chrome profile.</cite> Use `--isolated` for a fresh profile per run if you do not want cookies and localStorage leaking between runs. For testing a "persists to localStorage" feature you specifically want a **fresh** profile, otherwise a previous run's state makes the test pass falsely.
2. <cite index="11-1">Tools operate on the currently selected page. Use list_pages to see available pages, then select_page to switch context.</cite> This is global mutable state in the MCP server, and it is why the browser is a leased exclusive resource (§8.4).
3. <cite index="4-1">take_snapshot returns a text snapshot of the selected page based on the accessibility tree, listing page elements with unique identifiers, and the tool documentation explicitly says to prefer taking a snapshot over taking a screenshot.</cite> On a 16GB card that guidance is not just a preference, it is what makes browser verification possible at all (§8.6).
4. <cite index="11-1">Element uids come from the snapshot, and if an element is not found you take a fresh snapshot because the element may have been removed or the page changed.</cite> Encode this in the role prompt as a rule: after any action that changes the DOM, re-snapshot before the next interaction.

<cite index="7-1">The server officially supports Google Chrome and Chrome for Testing only</cite>, so pin Chrome for Testing in the dev environment rather than relying on whatever Chrome the workstation has.

### 8.4 The browser is a leased singleton

Because the MCP server has one "selected page" and one browser process, two agents sharing it will interleave `select_page` calls and silently act on each other's tabs. The failure is not an error, it is a wrong result, which is worse.

```go
type BrowserLease struct {
    mu       sync.Mutex
    holder   string        // task id
    acquired time.Time
    ttl      time.Duration // 10 min default
}
```

Rules:

- `resource_class: browser` tasks require the lease. The scheduler treats the lease as a resource of capacity 1, so at most one browser task is ever in a wavefront batch.
- The lease has a TTL. A hung task cannot deadlock the run.
- On release, the server navigates to `about:blank` and closes extra pages, so the next task starts clean.

**Alternative if you later want parallel browser work:** run one `chrome-devtools-mcp` process per worker, each with `--isolated` and a distinct remote debugging port, and treat them as a pool. This is assumption A8's alternative. It costs roughly 400MB of system RAM per Chrome instance, not VRAM, so it is affordable. Do not build it until you have a run that is actually blocked on browser throughput.

### 8.5 The web development verification loop

This is the concrete answer to "best if the agent can have access to tooling like chrome mcp for web dev". Without it, a front-end task's definition of done is "the model believes the code is correct". With it, the definition of done is an observation.

```
Coder task T3 finishes: implements dark mode toggle
   ↓ merged to integration branch, gates pass (build + unit tests)
Orchestrator starts the dev server as a managed process
   npm run dev, wait for port 3000, capture stdout to an artifact
   ↓
Web Verifier task T7 acquires the browser lease
   1. chrome.navigate_page      url=http://localhost:3000/settings
   2. chrome.wait_for           text="Appearance"
   3. chrome.take_snapshot      → a11y tree, uids
      → stored as artifact, ~2-6k tokens of the budget, summary in context
   4. chrome.list_console_messages
      → 0 errors expected; any error is an immediate `finding` claim
   5. chrome.click              uid=<toggle from snapshot>
   6. chrome.take_snapshot      → confirm aria-checked flipped
   7. chrome.evaluate_script    "localStorage.getItem('theme')"
      → assert "dark"                    ← the acceptance criterion, verified
   8. chrome.navigate_page      type=reload
   9. chrome.take_snapshot      → confirm the toggle is still on after reload
  10. chrome.resize_page        375x667
      chrome.take_snapshot      → confirm no elements overlap or overflow
  11. chrome.list_network_requests → assert no 4xx or 5xx
   ↓ release lease
Verdict: pass → memory_write(kind=fact, verified=tool_observed)
         fail → memory_write(kind=finding) + orchestrator schedules a fix task
```

Step 7 is the whole point. `localStorage.getItem('theme') === "dark"` is a **tool-observed fact**, so under §5.8 it is promotable to long-term memory. The system's knowledge that the feature works is grounded in the browser having actually done it. Compare that to the same conclusion reached by a model reading its own diff.

Two additions once the basic loop works:

- `performance_start_trace` / `performance_stop_trace` / `performance_analyze_insight` on a page that has a performance acceptance criterion. Cheap, deterministic, and the output is structured.
- `lighthouse_audit` as a gate for accessibility regressions. Its output is a score plus a list of failures, which fits a context budget far better than a screenshot does.

### 8.6 The vision problem, stated honestly

`take_screenshot` returns an image. To interpret it you need a vision-language model. On 16GB you cannot hold a 7B VLM and a 14B coder at once (roughly 5 + 9 GiB of weights plus two KV caches exceeds the budget). So there are exactly three options and no clever fourth:

| Option | Cost | When to use |
|--------|------|-------------|
| **A. No vision (default, A9)** | Free | Almost always. The a11y snapshot carries structure, labels, roles, and state. Overflow, missing labels, wrong ARIA state, and broken interaction are all detectable from text |
| **B. Phase swap** | ~10s unload + ~8s load per swap | A dedicated "visual check" wavefront at end of run. Unload the coder, load Qwen2.5-VL 7B, review the screenshots collected during the run, unload, reload the coder. Amortised over a whole run this is acceptable |
| **C. Cloud vision** | One API call | A "does this look right" check you invoke manually from the UI. Breaks N1 (local-first) for that one call, which is a fine trade for a button a human presses |

Recommendation: build A, wire the artifact store to keep screenshots regardless (they cost nothing and you will want them in the UI for human review), add C as a UI button, and treat B as optional. Human eyes on a screenshot in the dashboard is the highest-value "vision" in the system and it costs zero VRAM.

### 8.7 Guardrails

| Surface | Control |
|---------|---------|
| Filesystem | All paths resolved and checked against the worktree root after `filepath.EvalSymlinks`. Reject `..` traversal, absolute paths, and symlinks escaping the tree |
| Write scope | Writes must match the task's declared `write_set` globs. A write outside it is a tool error returned to the model, and a `constraint_violation` event |
| Commands | Allowlist by binary and subcommand: `go build`, `go test`, `go vet`, `gofmt`, `npm run <script from package.json>`, `git` restricted to `add/commit/diff/status`. No shell interpolation, `exec.Command` with an argv slice, never `sh -c` |
| Timeouts | Per tool call (30s default, 300s for `run_cmd`, 60s for browser), per attempt (15 min), per run (configurable) |
| Network | Agents have no direct network tool. All network access is via declared MCP servers, and Chrome is restricted to `localhost` plus an allowlist |
| Secrets | Environment variables are filtered before being passed to `run_cmd`. Any tool result matching a secret pattern is redacted before it enters the artifact summary or the context |
| Destructive git | `push`, `reset --hard`, `clean -fd`, and force operations are never available to any agent. The orchestrator performs merges, not agents |
| Incoming specs | Schema-validated, capability-clamped by intersection, write-set ceiling enforced, commands checked against this allowlist at compile time, and approved by a human before the first agent step. Prose from a spec enters context as data inside a delimiter, never as instruction (§4.6) |

---

## 9. The dashboard

R7 asks to see progress. R8 asks to manage it. Those are different products and the second one is where the value is: a run you can only watch is a run you have to babysit.

### 9.1 Screens

**Spec library.** The landing page in compiled mode. Every file in `.theorm/specs/`, with its compile status (compiles clean, drift warning, fails validation), the runs it has produced, and their outcomes. Drag a downloaded `.md` file onto this screen to write it into `.theorm/specs/` and compile it.

**Compile report.** The gate between a spec and a run, rendering the §4.4 `--explain` output: the projected wavefronts, every command that will execute, every path that will be written, every memory record that will be created, and any drift against `base`. One approve button. This screen is doing the work of N8, and it is the screen that makes running someone else's spec a reasonable thing to do.

**Runs list.** One row per run: spec or goal, state, progress (tasks done over total), elapsed, tokens consumed, cloud calls used against budget, last event. Filter by state.

**Run detail**, four panes:

1. **DAG canvas.** Nodes coloured by state, edges are dependencies. Live. Clicking a node opens the task drawer. Batch groupings are drawn as boxes so the sequential-versus-parallel decision is visible rather than inferred.
2. **Timeline.** A Gantt view of attempts, one lane per worker. This is where you see GPU idle time, which is the metric that tells you whether the scheduler is working.
3. **Live log.** The event stream, filterable by task, by role, and by event type. Tool calls are collapsible and expand to full arguments and the artifact result.
4. **Resource strip.** Current mode (quality / balanced / throughput), resident model, slots busy over total, VRAM used, browser lease holder, tokens per second.

**Task drawer** (opens from the DAG): the contract, the attempt history, the prompt actually sent (this one matters more than it sounds, most debugging ends here), the model's raw response, tool calls with results, gate output, reviewer verdict, and the diff.

**Memory browser.** Two tabs.
- *Short-term*: the live blackboard for this run as a filterable table of claims, with contradiction chains rendered as threads. Sortable by confidence.
- *Long-term*: search over L3 using the same hybrid retrieval the agents use, showing which records were retrieved for which task and their rerank scores. Per record: content, provenance, verification level, hit count, and a retire button.

The long-term tab doing retrieval *the same way agents do* is deliberate. When a run goes wrong because of a bad memory, this is where you find it, and you need to see what the agent saw.

**Diff viewer.** Per task and cumulative for the run. Side by side, with the acceptance criteria pinned next to it.

### 9.2 Control surface

| Control | Scope | Effect |
|---------|-------|--------|
| Pause / Resume | Run | Scheduler stops issuing new tasks. Running tasks finish |
| Cancel | Run | Running tasks receive context cancellation, worktrees are preserved for inspection |
| Retry with hint | Task | Re-queue with a human-written `handoff` claim injected into the read-set. This is the single most useful control in the system |
| Edit acceptance criteria | Task | For when the planner misunderstood. Task returns to pending |
| Skip task | Task | Mark done without executing, dependents unblock. For work you did yourself |
| Force sequential | Run | Overrides §6.4 to degree 1 for the rest of the run |
| Switch mode | Run | Force quality or throughput mode at the next barrier |
| Approve / Reject merge | Gate | A10's approval gate, with the full diff |
| Resolve conflict | Task | Choose ours, theirs, or re-run rebased |
| Retire memory | L3 record | Marks a record retired, effective for all future runs |
| Retire spec contributions | Spec | Retires every L3 record whose `verified_by` matches that spec's hash (§4.6, control 6) |
| Export spec delta | Run | Emits the §4.8 report for the next design session |
| Re-anchor spec | Spec | Rewrites `base` to current HEAD after you have reviewed the drift |
| Promote claim | L1 claim | Manually promote to L3 with `human_confirmed`, which is immune to decay |

"Retry with hint" deserves emphasis. When a small model is stuck, one sentence from you is worth more than three automated retries, and it costs one click. Build it early.

### 9.3 Event model

Everything is an event. The UI is a projection, which means live view and historical replay are the same code path.

```sql
CREATE TABLE events (
  id          BIGSERIAL PRIMARY KEY,
  run_id      UUID NOT NULL,
  task_id     UUID,
  attempt_id  UUID,
  type        TEXT NOT NULL,
  payload     JSONB NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX events_run_idx ON events (run_id, id);
```

Event types, which double as the specification of what the UI can show:

```
run.created  run.planned  run.started  run.paused  run.resumed  run.completed  run.failed
wave.started  wave.completed              -- payload: task ids, degree, mode, reason
task.queued  task.claimed  task.started  task.completed  task.failed  task.escalated
attempt.started  attempt.finished        -- payload: prompt tokens, completion tokens, ms
llm.request  llm.response                -- payload: model, slot, temp, token counts
tool.called  tool.returned  tool.denied  -- payload: name, args, artifact id, duration
gate.started  gate.passed  gate.failed   -- payload: pipeline, tail of output
review.verdict
memory.claim_written  memory.contradiction  memory.promoted  memory.conflict
browser.lease_acquired  browser.lease_released
resource.mode_switched  resource.slot_granted  resource.slot_released
git.merged  git.conflict
human.intervened                          -- payload: control, actor, params
```

### 9.4 Approval gates

An approval is a task state, not a modal dialog, so it survives you closing the browser.

```sql
CREATE TABLE approvals (
  id          UUID PRIMARY KEY,
  run_id      UUID NOT NULL,
  task_id     UUID,
  kind        TEXT NOT NULL,        -- merge_to_default | destructive_op | memory_conflict
  context     JSONB NOT NULL,       -- diff ref, both sides of a conflict, etc.
  state       TEXT NOT NULL DEFAULT 'pending',   -- pending|approved|rejected|expired
  decided_by  TEXT,
  decided_at  TIMESTAMPTZ,
  expires_at  TIMESTAMPTZ
);
```

A run with a pending approval is `waiting_human`, not `running`, and the runs list shows a badge. If you want to run overnight, set `auto_approve_after` in config and the gate becomes a delay rather than a block.

### 9.5 Transport

```
Go:      Postgres LISTEN/NOTIFY  →  in-process event bus  →  SSE fanout per run
Next.js: EventSource('/api/runs/:id/events?after=<last_id>')
```

The `after` cursor makes reconnection free and correct: on reconnect the client replays from its last seen event id, so a dropped connection loses nothing. Commands go over plain REST:

```
POST /api/runs                          create a goal, trigger planning
POST /api/runs/:id/pause|resume|cancel
POST /api/runs/:id/mode                 {mode: quality|balanced|throughput|auto}
POST /api/tasks/:id/retry               {hint: "the config loader is in internal/cfg not internal/config"}
POST /api/tasks/:id/skip
PATCH /api/tasks/:id/acceptance
POST /api/approvals/:id                 {decision: approve|reject}
GET  /api/runs/:id/memory/stm           filterable claim list
GET  /api/memory/ltm/search             ?q=...&repo=...   same hybrid path as agents
POST /api/memory/ltm/:id/retire
GET  /api/artifacts/:id                 content, ranged
```

Keep the Next.js app a **pure client**. No business logic, no direct database access. Everything through the Go API. This keeps the orchestrator runnable headless, which matters for CI and for debugging.

---

## 10. Observability and evaluation

### 10.1 Metrics

Prometheus, since you already instrument this way.

```
# Throughput and latency
theorm_task_duration_seconds{role,outcome}                 histogram
theorm_attempt_total{role,outcome}                         counter
theorm_llm_tokens_total{model,direction}                   counter    # direction=prompt|completion
theorm_llm_tokens_per_second{model}                        gauge

# The scheduler's actual behaviour
theorm_wave_degree                                         histogram
theorm_gpu_slot_utilization                                gauge
theorm_gpu_idle_seconds_total                              counter    # the number to minimise
theorm_mode_switches_total{from,to}                        counter

# Context health, the leading indicator of quality problems
theorm_context_tokens_used{role,section}                   histogram
theorm_context_overflow_total{role,section}                counter    # should be near zero
theorm_context_truncation_total{role}                      counter

# Memory health
theorm_stm_claims_total{kind,verification}                 counter
theorm_ltm_records{repo,mem_type,status}                   gauge
theorm_ltm_retrieval_latency_seconds{stage}                histogram  # stage=dense|sparse|rerank
theorm_ltm_promotions_total{outcome}                       counter    # outcome=accepted|dup|conflict
theorm_memory_contradictions_total                         counter

# Tools
theorm_tool_calls_total{name,outcome}                      counter
theorm_tool_duration_seconds{name}                         histogram
theorm_tool_denied_total{role,name}                        counter    # rising = bad capability sets

# Cost
theorm_cloud_calls_total{purpose}                          counter
theorm_cloud_budget_remaining{run}                         gauge
```

`theorm_context_overflow_total` and `theorm_gpu_idle_seconds_total` are the two to put on the front of the dashboard. The first tells you when quality is about to degrade. The second tells you whether the scheduler is earning its complexity.

### 10.2 Tracing

OpenTelemetry spans nested as `run → wave → task → attempt → step → tool_call`, with token counts as span attributes. Even with only a local Jaeger, being able to see one attempt's full step sequence with timings answers "where did 40 seconds go" instantly, and the answer is usually a gate or a browser wait, not the model.

### 10.3 An evaluation harness for the orchestrator itself

This is the part most people skip, and given your existing work on retrieval evaluation it is the natural thing for you to build well. Without it, every change to a prompt or a scheduling rule is a vibe.

**Fixture repositories.** Three, kept in `testdata/`:
- `tiny-go`: a Go HTTP service, about 8 files. Fast, for CI.
- `mid-next`: a Next.js app with a component library and tests. For browser loop coverage.
- `messy-go`: deliberately awkward. Inconsistent conventions, a package-level singleton, one import cycle waiting to happen. This is the one that separates good scheduling from lucky scheduling.

**Task suite.** 20 to 30 goals with machine-checkable success criteria:

```yaml
- id: G07
  repo: tiny-go
  goal: "Add a /healthz endpoint returning build version as JSON, with a unit test"
  success:
    - cmd: "go test ./... -count=1"        expect: exit 0
    - cmd: "grep -r 'healthz' internal/"   expect: exit 0
    - http: {path: /healthz, status: 200, json_has: ["version"]}
  budget: {max_attempts: 6, max_wall_clock: 600s, max_cloud_calls: 2}
```

**Metrics to report**, per configuration:

| Metric | Definition | Why |
|--------|-----------|-----|
| Pass@1 | Fraction of goals where the first run succeeds | The headline number |
| Pass@3 | Fraction succeeding within three runs | Measures whether retries actually help |
| Attempts per task | Mean | Prompt quality proxy |
| Wall clock | Median and p90 | What parallelism is supposed to improve |
| Tokens per goal | Sum | Efficiency, and it correlates with cost if you ever move to an API |
| Human interventions | Count | The real usability number |
| Memory hit rate | Retrieved L3 records that appear in the final diff's touched files | Is retrieval actually useful, or decoration |
| Memory precision | Retrieved records a human judges relevant, sampled | The one worth measuring by hand, quarterly |

**Ablations to run.** These are the experiments that tell you which parts of this design are worth their complexity:

| Ablation | Question it answers |
|----------|--------------------|
| L3 memory off | Does long-term memory improve Pass@1, or is it just retrieval theatre |
| L1 blackboard off (agents isolated) | How much does sharing short-term memory actually buy |
| Explorer role removed | Is separating discovery from mutation worth an extra task |
| Forced sequential vs adaptive | Does the scheduler improve wall clock without hurting Pass@1 |
| 14B vs 7B executor | Where exactly the quality cliff is on your hardware |
| Reranker off (RRF only) | Whether the cross-encoder earns its 300ms |
| Cloud planner vs local planner | Whether A1 is still true |

Run the suite nightly against `tiny-go`, weekly against all three. A regression in Pass@1 after a prompt change is the kind of thing you otherwise discover three weeks later.

This harness also happens to be the most portfolio-legible artifact in the project. "I built an agent orchestrator" is a common claim. "I built an evaluation harness that measures whether shared memory improves task success rate, and here are the ablations" is not.

---

## 11. Failure modes

Every one of these will happen. The design is only real if each has a stated response.

| # | Failure | Detection | Response |
|---|---------|-----------|----------|
| F1 | VRAM OOM on model load | Load returns an error | Resource Manager refuses the mode switch, stays in the current mode, emits `resource.mode_switch_denied`. Never partially unload |
| F2 | KV exhaustion mid-generation | Server returns a context error | Attempt fails with `ErrContextExhausted`. Retry at attempt+1 with a 25 percent tighter context budget. If it recurs, the task is too big and escalates to replan |
| F3 | Model swap thrashing | `mode_switches_total` rising | Enforce a minimum residency of 5 minutes per model and the amortisation check in Rule 6. If it still thrashes, pin the mode |
| F4 | Context overflow at assembly | Assembler's own budget check, before the request | Assembly never sends an over-budget prompt. It drops sections by the §5.7 policy and emits `context.section_dropped`. A dropped `constraint` section is a hard error, not a degradation |
| F5 | Infinite tool loop (same call, same args) | Hash of (name, args) repeated 3 times in one attempt | Return a tool error naming the loop. Then fail the attempt |
| F6 | Agent narrates without acting | No tool call and no completion | One nudge, then `ErrAgentStalled` (§7.2) |
| F7 | Memory poisoning | Contradiction rate rising, or a human notices a bad answer | Retire the record from the UI. The `source_run` and `source_claim` columns let you trace how it got there and whether the same path produced others |
| F8 | Retrieval returns nothing useful | `memory_hit_rate` near zero | Usually cold memory. Run `theorm index`. If it persists, the embedding model is a poor fit for code, and this is exactly what embedding fine-tuning would address |
| F9 | Chrome crashes or hangs | Tool timeout, or MCP process exit | Kill the MCP server process, release the lease, restart on next use. Browser tasks are idempotent by construction, so retry is safe |
| F10 | Two agents edit the same file | Should be impossible via write-set partitioning. If it happens, the merge conflicts | `conflict_hold` plus a UI card. Then fix the planner prompt, because this means it emitted overlapping write sets |
| F11 | Merge conflict on integration | `git merge` non-zero exit | Never auto-resolve. Human decision or rebase-and-rerun (§6.5) |
| F12 | Planner budget exhausted | Atomic counter at zero | Run escalates to human. The UI offers "spend one more" as an explicit, logged override |
| F13 | Orchestrator crash mid-run | Lease expiry on restart | Tasks in `running` with expired leases go to `retrying`. Worktrees are reconciled against the database. Partial commits in a worktree are preserved and shown in the UI |
| F14 | Postgres unavailable | Connection error | The orchestrator refuses to start rather than running with no state. There is no in-memory fallback, deliberately |
| F15 | Dev server port already in use | Bind error at startup | Managed process supervisor picks the next free port and injects it into the browser task's context. Never assume 3000 |
| F16 | Non-determinism between runs | Same goal, different plan | Temperature 0.1, fixed seed, and log the seed. Accept that llama.cpp batching still introduces small variation. Report Pass@3, not just Pass@1, for this reason |
| F17 | Snapshot too large for context | Token estimate exceeds the section budget | The a11y snapshot goes to an artifact and the context receives a structural summary plus the first N interactive elements. The agent can request more with `artifact_read` |
| F18 | Secrets leak into artifacts | Redaction pattern match at write time | Redact before storing, not before displaying. An artifact on disk with a live token in it is a real incident |

---

## 12. Build plan

v1's milestones M0 to M9 are assumed complete or in progress. These build on them, and each has a demo-able definition of done.

**Phase A0: the spec compiler (about 3 days). Build this first.** It is simpler than the planner, it removes the cloud dependency from every subsequent phase's tests, and it makes every later milestone reproducible instead of stochastic.

| # | Work | DoD |
|---|------|-----|
| A0.1 | TSF parser: front matter, `theorm:task` and `theorm:knowledge` blocks, prose chunking | A malformed block fails with a file path and line number, and no run is created |
| A0.2 | Shared task validator used by both the compiler and the planner | The v1 planner's output and a hand-written spec pass through the identical code path |
| A0.3 | DAG construction, write-set intersection, context feasibility check | A spec with a cycle, an unresolvable dependency, or a task estimated above the slot size fails compilation |
| A0.4 | Capability clamping, write-set ceiling, command allowlist at compile time | A spec requesting a tool its role lacks compiles with the tool absent, not granted. A spec containing a non-allowlisted command fails |
| A0.5 | `theorm compile --explain` | The §4.4 sample output reproduces for a hand-written spec against `tiny-go` |
| A0.6 | Base pinning and drift detection | Changing a file in a task's write set after authoring produces a conflicting-drift warning naming that task |

**Phase A: memory foundations (about 1 week part-time)**

| # | Work | DoD |
|---|------|-----|
| A1 | `stm_claims` and `artifacts` tables, `memory_write` / `memory_read` / `artifact_read` tools | A coder agent writes a `finding`, a reviewer in the same run reads it back and cites it in its verdict |
| A2 | Context Assembler with the §5.7 budget and per-section overflow policy | `theorm debug-prompt --task T4` prints the exact prompt with a per-section token table. No section exceeds budget on any fixture goal |
| A3 | Artifact interception on every tool result, with deterministic summarisers for test output and diffs | A 4,000-line test log enters the context as under 100 tokens, and `artifact_read` retrieves the full text |
| A4 | Explorer role, read-only capability set | On `messy-go`, a run with an Explorer produces a coder attempt that reads 40 percent fewer files than one without |

**Phase B: long-term memory (about 1 week)**

| # | Work | DoD |
|---|------|-----|
| B1 | pgvector and tsvector schema, CPU embedding sidecar | `theorm index testdata/tiny-go` produces entity records for every source file in under 60 seconds |
| B2 | Hybrid retrieval with RRF plus CPU cross-encoder rerank | `theorm memory search "how are routes registered"` returns the router file in the top 3 on all three fixtures |
| B3 | Curator role with the §5.8 promotion rules | A run producing 30 claims promotes at most 10, rejects near-duplicates, and parks contradictions |
| B4 | Decay job and git-driven staleness | Changing a file marks its entity record stale within one run |
| B5 | Spec knowledge ingestion: `theorm:knowledge` blocks and prose chunks into L3 with spec provenance | Compiling a spec writes its knowledge records, and retiring by spec hash removes exactly those |
| B6 | Evaluation harness, ablation: L3 on versus off | A number. Whatever it is, you now know whether §5.6 was worth building |

**Phase C: adaptive concurrency (about 1 week)**

| # | Work | DoD |
|---|------|-----|
| C1 | llama-server backend behind the inference interface, with `/slots` introspection | `theorm doctor` reports slots, context per slot, resident model, and VRAM |
| C2 | Resource Manager: slot semaphore, mode definitions, swap at barriers only | Forcing throughput mode swaps the model once and reports 4 slots |
| C3 | `write_set` in the planner schema plus validation that rejects a plan without one | A plan with overlapping write sets on independent tasks is rejected with a specific error |
| C4 | Wavefront scheduler with `decideParallelism` | The §6.4 worked example reproduces: waves 1 and 2 sequential, wave 3 parallel degree 3 in throughput mode |
| C5 | Worktree pool, dependency-ordered integration, gates re-run after each merge | Three parallel tasks merge cleanly, and a deliberately conflicting pair lands in `conflict_hold` rather than corrupting the tree |

**Phase D: MCP and the browser (about 1 week)**

| # | Work | DoD |
|---|------|-----|
| D1 | MCP client in Go: stdio transport, `tools/list`, `tools/call`, namespacing, result interception | Chrome's tools appear in the registry namespaced and capability-filtered |
| D2 | Browser lease with TTL and clean release | Two browser tasks in one run serialise, and a killed task releases the lease within its TTL |
| D3 | Web Verifier role and the §8.5 loop | On `mid-next`, the dark mode goal is verified end to end including the `localStorage` assertion, and the resulting fact reaches L3 |
| D4 | Managed dev server supervisor with dynamic port injection | Two runs on the same machine do not collide on port 3000 |

**Phase E: the dashboard (about 1.5 weeks)**

| # | Work | DoD |
|---|------|-----|
| E1 | Event table, SSE with `after` cursor, reconnect replay | Killing the browser tab and reopening it loses no events |
| E2 | Runs list and run detail with the live DAG | The DAG updates within 500ms of a state change, and batch groupings are visible |
| E3 | Task drawer showing the exact prompt sent | You can diagnose a bad attempt without reading a log file |
| E4 | Memory browser, both tabs, LTM search using the agent retrieval path | You can find and retire a bad memory in under 30 seconds |
| E5 | Control surface: pause, cancel, retry-with-hint, approve, skip, mode override | A run can be steered to completion by a human who never touches a terminal |

Roughly six weeks part-time. Phase A0 comes first because everything after it becomes easier to test once plans are deterministic. Phases A and B change the system's character. C, D, and E are additive and can be reordered.

If you have to cut something, cut Phase C. Sequential execution with excellent memory beats parallel execution with poor memory, and §2.3 shows the hardware agrees. Do not cut A0: without it, every test of every later phase depends on a cloud call producing the same plan twice, which it will not.

Once A0 and Phase A are working, write Phases B through E as `.theorm/specs/` files and have TheORM build itself. That is the honest test of whether the format is expressive enough, and it is a considerably better demo than a healthz endpoint.

---

## 13. Decisions I need from you

These are the places where I made a call that a different reasonable person would make differently, and where your answer changes the design rather than just a config value.

1. **A3, pgvector versus Qdrant.** I chose pgvector for operational simplicity: one database, one backup, one transaction. You know Qdrant well and it is genuinely better at scale. At 500 to 5,000 records per repository, scale is not the deciding factor, so this is a preference question. Which do you want to maintain?

2. **Answered by §4.** You asked whether a design authored in a cloud session can be downloaded and fed to the orchestrator, and that is now the primary path. What is left to decide is how much of the leaf-level decomposition you want to author yourself. Compiled mode (§4.9) means you write every task block, which is the most control and the most typing. Hybrid mode means you write the phase structure and hard dependencies and let the local 14B expand tasks marked `expand: true`. I recommend hybrid, but the answer determines how much structure the TSF schema needs to support, so it is worth deciding before A0.2.

3. **Executor model.** I recommend Qwen2.5-Coder 14B as the default because the numbers are known and it is already in your v1 spec. Devstral Small 2 24B is agent-trained and would likely behave better in a tool loop, but the KV math says it does not fit on 16GB with a useful context window. Do you want Phase C1 to include a measurement pass across 14B, a 24B at IQ3, and a 30B-A3B MoE with offload, before committing?

4. **Scope of the first browser loop.** §8.5 assumes a local dev server on localhost. Do you also want to point it at deployed staging URLs, which adds authentication handling, network policy, and a much larger blast radius for a mistake?

5. **How much autonomy overnight?** A10 puts an approval gate only on merging to the default branch. The alternative is `auto_approve_after: 30m`, which turns the gate into a delay and lets a run finish while you sleep. This is a trust decision, not a technical one, and it is easy to change later.

6. **Is this a portfolio artifact or a tool you use?** It changes what to build first. As a tool, Phase E's controls matter most, because they are what make it usable daily. As a portfolio piece, Phase B5's evaluation harness matters most, because measured ablations on a system you designed is the strongest possible evidence of the architectural judgment you have been positioning around.

If you answer 1, 3, and 6, I can turn this into the Go package layout and the migration files, which is the natural next step after the v1 skeleton.
