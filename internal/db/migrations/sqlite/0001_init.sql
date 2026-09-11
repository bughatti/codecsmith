-- Transcoder SQLite schema (fresh installs). Kept column-compatible with
-- the Postgres schema after migration 0002.
CREATE TABLE IF NOT EXISTS jobs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    file_path        TEXT UNIQUE NOT NULL,
    library          TEXT NOT NULL,
    profile          TEXT,
    status           TEXT NOT NULL CHECK (status IN
        ('queued','processing','running','completed','failed','interrupted','skipped','cancelled')),
    priority         INTEGER NOT NULL DEFAULT 5,
    original_size    INTEGER,
    new_size         INTEGER,
    original_codec   TEXT,
    target_codec     TEXT,
    created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at       TIMESTAMP,
    completed_at     TIMESTAMP,
    updated_at       TIMESTAMP,
    error_message    TEXT,
    progress         REAL NOT NULL DEFAULT 0,
    speed            REAL,
    fps              REAL,
    eta_seconds      INTEGER,
    worker_id        TEXT,
    cancel_requested INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS jobs_status_idx ON jobs (status);
CREATE INDEX IF NOT EXISTS jobs_queue_idx ON jobs (status, priority DESC, created_at ASC);
CREATE INDEX IF NOT EXISTS jobs_library_status_idx ON jobs (library, status);
CREATE INDEX IF NOT EXISTS jobs_completed_idx ON jobs (completed_at DESC);

CREATE TABLE IF NOT EXISTS system_state (
    key        TEXT PRIMARY KEY,
    value      TEXT,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS system_metrics (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    cpu_percent        REAL,
    memory_percent     REAL,
    disk_usage_percent REAL,
    active_jobs        INTEGER,
    queue_size         INTEGER
);
CREATE INDEX IF NOT EXISTS system_metrics_timestamp_idx ON system_metrics (timestamp DESC);

CREATE TABLE IF NOT EXISTS downloads (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    source          TEXT NOT NULL,
    external_id     TEXT NOT NULL,
    title           TEXT NOT NULL,
    status          TEXT NOT NULL,
    progress        REAL NOT NULL DEFAULT 0,
    size_total      INTEGER NOT NULL DEFAULT 0,
    size_downloaded INTEGER NOT NULL DEFAULT 0,
    download_rate   REAL NOT NULL DEFAULT 0,
    eta_seconds     INTEGER,
    category        TEXT,
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (source, external_id)
);
CREATE INDEX IF NOT EXISTS downloads_status_updated_idx ON downloads (status, updated_at);

CREATE TABLE IF NOT EXISTS storage_stats (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    path        TEXT NOT NULL,
    total_bytes INTEGER NOT NULL,
    used_bytes  INTEGER NOT NULL,
    free_bytes  INTEGER NOT NULL,
    file_count  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS storage_stats_path_ts_idx ON storage_stats (path, timestamp DESC);
