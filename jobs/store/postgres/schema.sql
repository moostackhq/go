-- Schema for the PostgreSQL-backed jobs store.
--
-- IDs are TEXT (UUIDv4 strings) for symmetry with the SQLite
-- backend; switch to UUID locally if you prefer.

CREATE TABLE IF NOT EXISTS jobs (
    id               TEXT PRIMARY KEY,
    kind             TEXT NOT NULL,
    payload          BYTEA NOT NULL,
    queue            TEXT NOT NULL,
    priority         INTEGER NOT NULL DEFAULT 0,
    state            TEXT NOT NULL,
    attempt          INTEGER NOT NULL DEFAULT 0,
    max_attempts     INTEGER NOT NULL,
    available_at     TIMESTAMPTZ NOT NULL,
    timeout_ms       BIGINT NOT NULL DEFAULT 0,
    on_timeout       INTEGER NOT NULL DEFAULT 0,
    backoff_spec     BYTEA,
    unique_key       TEXT NOT NULL DEFAULT '',
    progress_done    BIGINT NOT NULL DEFAULT 0,
    progress_total   BIGINT NOT NULL DEFAULT 0,
    progress_msg     TEXT NOT NULL DEFAULT '',
    error            TEXT NOT NULL DEFAULT '',
    locked_by        TEXT NOT NULL DEFAULT '',
    locked_until     TIMESTAMPTZ,
    heartbeat_at     TIMESTAMPTZ,
    cancel_requested BOOLEAN NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS jobs_unique_active
    ON jobs (kind, unique_key)
    WHERE unique_key != ''
      AND state NOT IN ('succeeded','failed','cancelled','discarded');

CREATE INDEX IF NOT EXISTS jobs_claim
    ON jobs (queue, priority DESC, available_at)
    WHERE state IN ('available','scheduled');

CREATE INDEX IF NOT EXISTS jobs_created_at
    ON jobs (created_at, id);

CREATE INDEX IF NOT EXISTS jobs_locked_until
    ON jobs (locked_until)
    WHERE state = 'running';

CREATE TABLE IF NOT EXISTS job_steps (
    id           TEXT PRIMARY KEY,
    job_id       TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    state        TEXT NOT NULL,
    result       BYTEA,
    error        TEXT NOT NULL DEFAULT '',
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    UNIQUE(job_id, name)
);

CREATE TABLE IF NOT EXISTS job_attempts (
    id           TEXT PRIMARY KEY,
    job_id       TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    attempt      INTEGER NOT NULL,
    worker_id    TEXT NOT NULL DEFAULT '',
    started_at   TIMESTAMPTZ NOT NULL,
    finished_at  TIMESTAMPTZ,
    state        TEXT NOT NULL,
    error        TEXT NOT NULL DEFAULT '',
    UNIQUE(job_id, attempt)
);

CREATE TABLE IF NOT EXISTS workers (
    id           TEXT PRIMARY KEY,
    hostname     TEXT NOT NULL,
    queues       TEXT NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS schedules (
    name         TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    cron         TEXT NOT NULL,
    payload      BYTEA NOT NULL,
    options      BYTEA NOT NULL,
    next_run_at  TIMESTAMPTZ,
    last_run_at  TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ NOT NULL
);
