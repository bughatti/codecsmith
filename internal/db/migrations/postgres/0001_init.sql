-- Transcoder Postgres schema.
-- Ported from /opt/transcoder_v2/src/database.py (SQLite).
-- Designed for Postgres 18+. Uses SELECT FOR UPDATE SKIP LOCKED for queue claiming
-- and LISTEN/NOTIFY ("transcoder_jobs") so workers wake on new jobs instead of
-- polling every 10s.

BEGIN;

CREATE TABLE IF NOT EXISTS jobs (
    id              BIGSERIAL PRIMARY KEY,
    file_path       TEXT UNIQUE NOT NULL,
    media_type      TEXT NOT NULL CHECK (media_type IN ('movies','shows','anime','downloads')),
    status          TEXT NOT NULL CHECK (status IN ('queued','processing','running','completed','failed','interrupted','skipped')),
    priority        INTEGER NOT NULL DEFAULT 5,
    original_size   BIGINT,
    new_size        BIGINT,
    original_codec  TEXT,
    target_codec    TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    error_message   TEXT,
    progress        REAL NOT NULL DEFAULT 0,
    pid             INTEGER,
    worker_id       TEXT
);

CREATE INDEX IF NOT EXISTS jobs_status_idx   ON jobs (status);
CREATE INDEX IF NOT EXISTS jobs_priority_idx ON jobs (priority DESC, created_at ASC);

-- Hot path for queue claim: workers always look for queued/interrupted jobs.
-- Partial index keeps it tiny and order-aware so SELECT FOR UPDATE SKIP LOCKED
-- doesn't need a full scan.
CREATE INDEX IF NOT EXISTS jobs_queue_idx
    ON jobs ((CASE WHEN status='interrupted' THEN 1 ELSE 2 END), priority DESC, created_at ASC)
    WHERE status IN ('queued','interrupted');

CREATE TABLE IF NOT EXISTS system_state (
    key         TEXT PRIMARY KEY,
    value       TEXT,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS stats (
    id                  BIGSERIAL PRIMARY KEY,
    date                DATE NOT NULL,
    media_type          TEXT NOT NULL,
    total_processed     INTEGER NOT NULL DEFAULT 0,
    total_saved_bytes   BIGINT  NOT NULL DEFAULT 0,
    total_time_seconds  INTEGER NOT NULL DEFAULT 0,
    errors              INTEGER NOT NULL DEFAULT 0,
    UNIQUE (date, media_type)
);

CREATE TABLE IF NOT EXISTS system_metrics (
    id                  BIGSERIAL PRIMARY KEY,
    timestamp           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    cpu_percent         REAL,
    memory_percent      REAL,
    disk_usage_percent  REAL,
    active_jobs         INTEGER,
    queue_size          INTEGER
);

CREATE INDEX IF NOT EXISTS system_metrics_timestamp_idx ON system_metrics (timestamp DESC);

CREATE TABLE IF NOT EXISTS downloads (
    id                BIGSERIAL PRIMARY KEY,
    source            TEXT NOT NULL CHECK (source IN ('sabnzbd','sonarr','radarr')),
    external_id       TEXT NOT NULL,
    title             TEXT NOT NULL,
    status            TEXT NOT NULL,
    progress          REAL NOT NULL DEFAULT 0,
    size_total        BIGINT NOT NULL DEFAULT 0,
    size_downloaded   BIGINT NOT NULL DEFAULT 0,
    download_rate     REAL NOT NULL DEFAULT 0,
    eta_seconds       INTEGER,
    category          TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (source, external_id)
);

CREATE INDEX IF NOT EXISTS downloads_status_updated_idx ON downloads (status, updated_at);

CREATE TABLE IF NOT EXISTS media_files (
    id                  BIGSERIAL PRIMARY KEY,
    file_path           TEXT UNIQUE NOT NULL,
    media_type          TEXT NOT NULL,
    title               TEXT,
    series_title        TEXT,
    season              INTEGER,
    episode             INTEGER,
    year                INTEGER,
    quality             TEXT,
    source              TEXT,
    external_id         TEXT,
    file_size           BIGINT,
    date_added          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    transcoded          BOOLEAN NOT NULL DEFAULT FALSE,
    transcode_job_id    BIGINT REFERENCES jobs (id)
);

CREATE INDEX IF NOT EXISTS media_files_type_date_idx ON media_files (media_type, date_added DESC);

CREATE TABLE IF NOT EXISTS storage_stats (
    id          BIGSERIAL PRIMARY KEY,
    timestamp   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    path        TEXT NOT NULL,
    total_bytes BIGINT NOT NULL,
    used_bytes  BIGINT NOT NULL,
    free_bytes  BIGINT NOT NULL,
    file_count  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS storage_stats_path_ts_idx ON storage_stats (path, timestamp DESC);

-- NOTIFY trigger: workers LISTEN on "transcoder_jobs" to wake on new queued jobs
-- and avoid polling. Payload is the media_type so a worker can filter cheaply.
CREATE OR REPLACE FUNCTION transcoder_notify_job() RETURNS trigger AS $$
BEGIN
    IF NEW.status IN ('queued','interrupted') THEN
        PERFORM pg_notify('transcoder_jobs', NEW.media_type);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS jobs_notify_insert ON jobs;
CREATE TRIGGER jobs_notify_insert
    AFTER INSERT ON jobs
    FOR EACH ROW EXECUTE FUNCTION transcoder_notify_job();

DROP TRIGGER IF EXISTS jobs_notify_update ON jobs;
CREATE TRIGGER jobs_notify_update
    AFTER UPDATE OF status ON jobs
    FOR EACH ROW
    WHEN (NEW.status IN ('queued','interrupted') AND OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION transcoder_notify_job();

-- Migration bookkeeping
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO schema_migrations (version) VALUES (1) ON CONFLICT (version) DO NOTHING;

COMMIT;
