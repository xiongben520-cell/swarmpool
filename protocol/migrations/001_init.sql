-- SwarmPool 数据库 schema v0.1（对齐 protocol/spec.md）
-- 应用方式: docker compose exec -T postgres psql -U swarmpool -d swarmpool < migrations/001_init.sql

CREATE TABLE IF NOT EXISTS nodes (
  id          TEXT PRIMARY KEY,              -- node_<32hex>（公钥哈希）
  pubkey      TEXT NOT NULL UNIQUE,          -- ed25519 base64
  hw          JSONB NOT NULL DEFAULT '{}',   -- 硬件白名单遥测（见 spec 第8节）
  benchmark   JSONB NOT NULL DEFAULT '{}',   -- 基准测试结果
  gpu_tier    INT  NOT NULL DEFAULT 0,
  models      JSONB NOT NULL DEFAULT '[]',   -- 声明的模型清单
  status      TEXT NOT NULL DEFAULT 'offline', -- online|offline|suspended
  reputation  INT  NOT NULL DEFAULT 50,      -- 0-100，初始 50
  region      TEXT,
  first_seen  TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen   TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS api_keys (
  id         BIGSERIAL PRIMARY KEY,
  key_hash   TEXT NOT NULL UNIQUE,           -- sha256(sk-sp-...)，明文不落库
  label      TEXT,
  credits    BIGINT NOT NULL DEFAULT 500,    -- 新需方赠送 500
  status     TEXT NOT NULL DEFAULT 'active', -- active|revoked
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS jobs (
  id          TEXT PRIMARY KEY,              -- job_<ulid>
  api_key_id  BIGINT REFERENCES api_keys(id),
  type        TEXT NOT NULL,                 -- image.gen | video.i2v | ...
  model       TEXT,
  payload     JSONB NOT NULL,                -- 任务输入（对齐 spec 第6节）
  status      TEXT NOT NULL DEFAULT 'queued',-- queued|leased|running|verifying|done|failed
  node_id     TEXT REFERENCES nodes(id),
  attempts    INT  NOT NULL DEFAULT 0,
  batch_id    TEXT,
  credits_cost BIGINT NOT NULL DEFAULT 0,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS artifacts (
  id          BIGSERIAL PRIMARY KEY,
  job_id      TEXT NOT NULL REFERENCES jobs(id),
  storage_key TEXT NOT NULL,                 -- R2 对象 key
  sha256      TEXT,
  size_bytes  BIGINT,
  meta        JSONB NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS ledger (
  seq        BIGSERIAL PRIMARY KEY,
  ts         TIMESTAMPTZ NOT NULL DEFAULT now(),
  kind       TEXT NOT NULL,                  -- mint|burn|grant|slash
  node_id    TEXT,
  api_key_id BIGINT,
  job_id     TEXT,
  amount     BIGINT NOT NULL,                -- 恒为正，方向由 kind 决定
  memo       TEXT
);

CREATE INDEX IF NOT EXISTS idx_jobs_status  ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_batch   ON jobs(batch_id);
CREATE INDEX IF NOT EXISTS idx_nodes_status ON nodes(status);
CREATE INDEX IF NOT EXISTS idx_ledger_node  ON ledger(node_id, seq DESC);
CREATE INDEX IF NOT EXISTS idx_ledger_key   ON ledger(api_key_id, seq DESC);

-- 任务类型注册表（spec 第7节：新增负载=插一行，平台代码零改动）
CREATE TABLE IF NOT EXISTS task_types (
  type            TEXT PRIMARY KEY,          -- image.gen
  runtime_image   TEXT NOT NULL,             -- ghcr.io/swarmpool/runtime-comfy:1.0
  input_schema    JSONB NOT NULL DEFAULT '{}',
  pricing         JSONB NOT NULL DEFAULT '{}', -- {gpu_tier_factor, per_second, per_gb_artifact}
  verifier        TEXT NOT NULL DEFAULT 'quick-checks',
  sandbox_profile TEXT NOT NULL DEFAULT 'default',
  enabled         BOOLEAN NOT NULL DEFAULT true
);

INSERT INTO task_types (type, runtime_image, pricing, verifier, sandbox_profile)
VALUES ('image.gen', 'ghcr.io/swarmpool/runtime-comfy:1.0',
        '{"gpu_tier_factor":6,"per_second":0.01,"per_gb_artifact":20}',
        'quick-checks', 'comfy')
ON CONFLICT (type) DO NOTHING;
