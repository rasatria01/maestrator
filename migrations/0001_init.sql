-- TheORM v2 base schema. See docs/theorm-v2-design.md §5, §6.2, §9.3.
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE runs (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  repo_id     TEXT NOT NULL,
  title       TEXT NOT NULL,
  mode        TEXT NOT NULL,                     -- compiled|hybrid|goal
  spec_path   TEXT,
  spec_sha256 TEXT,
  base_commit TEXT,
  state       TEXT NOT NULL DEFAULT 'pending',   -- pending|running|paused|done|failed
  cloud_budget INT NOT NULL DEFAULT 0,           -- N4: atomic counter, 0 in compiled mode
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tasks (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id      UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  key         TEXT NOT NULL,                     -- T1, T2, ... from the spec
  title       TEXT NOT NULL,
  role        TEXT NOT NULL,
  depends_on  TEXT[] NOT NULL DEFAULT '{}',      -- task keys, within this run
  write_set   TEXT[] NOT NULL,                   -- §6.1: mandatory, drives parallelism
  contract    JSONB NOT NULL,                    -- read_set, acceptance, gates, estimates
  resource_class TEXT NOT NULL,                  -- gpu_deep|gpu_shallow|cpu_only|browser
  state       TEXT NOT NULL DEFAULT 'pending',   -- §6.2
  attempts    INT NOT NULL DEFAULT 0,
  max_attempts INT NOT NULL DEFAULT 3,
  worker_id   TEXT,
  lease_until TIMESTAMPTZ,                       -- §6.2: leases, not liveness checks
  worktree    TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (run_id, key)
);
CREATE INDEX tasks_claim_idx ON tasks (run_id, state);

CREATE TABLE attempts (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  task_id    UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  n          INT NOT NULL,
  outcome    TEXT,                               -- ok|agent_error|gate_failed|review_failed
  detail     TEXT,
  started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  ended_at   TIMESTAMPTZ,
  UNIQUE (task_id, n)
);

-- L1: the blackboard (§5.2). Append-only; corrections chain via superseded_by.
CREATE TABLE stm_claims (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id       UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  task_id      UUID REFERENCES tasks(id) ON DELETE CASCADE,
  attempt_id   UUID REFERENCES attempts(id) ON DELETE CASCADE,
  kind         TEXT NOT NULL,                    -- fact|decision|constraint|finding|failure|question|handoff
  subject      TEXT NOT NULL,
  predicate    TEXT NOT NULL,
  object       TEXT NOT NULL,
  confidence   REAL NOT NULL CHECK (confidence BETWEEN 0 AND 1),
  verified     TEXT NOT NULL DEFAULT 'asserted', -- §5.8 verification levels
  evidence     TEXT,                             -- artifact id or tool call id
  provenance   JSONB NOT NULL DEFAULT '{}',
  dedupe_hash  TEXT NOT NULL,                    -- (kind,subject,predicate,normalised object)
  superseded_by UUID REFERENCES stm_claims(id),
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX stm_dedupe_idx  ON stm_claims (run_id, dedupe_hash);
CREATE INDEX stm_read_idx           ON stm_claims (run_id, kind, subject);

-- L2: artifacts (§5.5). Content on disk, summary in the row.
CREATE TABLE artifacts (
  id         TEXT PRIMARY KEY,                   -- sha256
  run_id     UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  task_id    UUID REFERENCES tasks(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL,
  media_type TEXT NOT NULL,
  size_bytes BIGINT NOT NULL,
  path       TEXT NOT NULL,
  summary    TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- L3: long-term memory (§5.6). Per-repo (A2), hybrid retrieval, decay.
CREATE TABLE ltm_records (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  repo_id      TEXT NOT NULL,
  mem_type     TEXT NOT NULL,                    -- semantic|episodic|procedural|entity
  subject      TEXT NOT NULL,
  content      TEXT NOT NULL,
  metadata     JSONB NOT NULL DEFAULT '{}',
  source_run   UUID REFERENCES runs(id) ON DELETE SET NULL,
  source_claim UUID,
  verified_by  TEXT NOT NULL,                    -- gate_pass|review_pass|human|tool_observation|spec:<sha>
  confidence   REAL NOT NULL CHECK (confidence BETWEEN 0 AND 1),
  hit_count    INT NOT NULL DEFAULT 0,
  last_hit_at  TIMESTAMPTZ,
  contradicted_count INT NOT NULL DEFAULT 0,
  status       TEXT NOT NULL DEFAULT 'active',   -- active|stale|retired
  valid_until  TIMESTAMPTZ,
  embedding    VECTOR(768),                      -- nomic-embed-text-v1.5, CPU (A5)
  content_tsv  TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ltm_embedding_idx ON ltm_records USING hnsw (embedding vector_cosine_ops);
CREATE INDEX ltm_tsv_idx       ON ltm_records USING gin (content_tsv);
CREATE INDEX ltm_lookup_idx    ON ltm_records (repo_id, mem_type, status);
CREATE INDEX ltm_subject_idx   ON ltm_records (repo_id, subject);

-- §9.3: the UI is a projection of this table.
CREATE TABLE events (
  id         BIGSERIAL PRIMARY KEY,
  run_id     UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  task_id    UUID,
  attempt_id UUID,
  type       TEXT NOT NULL,
  payload    JSONB NOT NULL DEFAULT '{}',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX events_run_idx ON events (run_id, id);
