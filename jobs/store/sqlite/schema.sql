-- Schema for the SQLite-backed jobs store.
--
-- Conventions:
--   * IDs are TEXT (UUIDv4 strings).
--   * Time columns are INTEGER unix nanoseconds; 0 means "no value".
--   * JSON payloads are BLOB.
--   * Bool columns are INTEGER 0/1.

CREATE TABLE IF NOT EXISTS jobs (
    id               TEXT PRIMARY KEY,
    kind             TEXT NOT NULL,
    payload          BLOB NOT NULL,
    queue            TEXT NOT NULL,
    priority         INTEGER NOT NULL DEFAULT 0,
    state            TEXT NOT NULL,
    attempt          INTEGER NOT NULL DEFAULT 0,
    max_attempts     INTEGER NOT NULL,
    available_at     INTEGER NOT NULL,
    timeout_ms       INTEGER NOT NULL DEFAULT 0,
    on_timeout       INTEGER NOT NULL DEFAULT 0,
    backoff_spec     BLOB,
    unique_key       TEXT NOT NULL DEFAULT '',
    progress_done    INTEGER NOT NULL DEFAULT 0,
    progress_total   INTEGER NOT NULL DEFAULT 0,
    progress_msg     TEXT NOT NULL DEFAULT '',
    error            TEXT NOT NULL DEFAULT '',
    locked_by        TEXT NOT NULL DEFAULT '',
    locked_until     INTEGER NOT NULL DEFAULT 0,
    heartbeat_at     INTEGER NOT NULL DEFAULT 0,
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

-- Partial unique index for unique_key (only one non-terminal job
-- may hold a given (kind, unique_key) pair).
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
    result       BLOB,
    error        TEXT NOT NULL DEFAULT '',
    started_at   INTEGER NOT NULL DEFAULT 0,
    finished_at  INTEGER NOT NULL DEFAULT 0,
    UNIQUE(job_id, name)
);

CREATE TABLE IF NOT EXISTS job_attempts (
    id           TEXT PRIMARY KEY,
    job_id       TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    attempt      INTEGER NOT NULL,
    worker_id    TEXT NOT NULL DEFAULT '',
    started_at   INTEGER NOT NULL,
    finished_at  INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL,
    error        TEXT NOT NULL DEFAULT '',
    UNIQUE(job_id, attempt)
);

CREATE TABLE IF NOT EXISTS workers (
    id           TEXT PRIMARY KEY,
    hostname     TEXT NOT NULL,
    queues       TEXT NOT NULL,
    started_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS schedules (
    name         TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    cron         TEXT NOT NULL,
    payload      BLOB NOT NULL,
    options      BLOB NOT NULL,
    next_run_at  INTEGER NOT NULL DEFAULT 0,
    last_run_at  INTEGER NOT NULL DEFAULT 0,
    updated_at   INTEGER NOT NULL
);
