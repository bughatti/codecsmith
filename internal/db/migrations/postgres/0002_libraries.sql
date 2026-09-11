-- v2: libraries replace fixed media types; add live encode telemetry,
-- cancellation, and the 'cancelled' status.
BEGIN;

ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_media_type_check;
ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_status_check;
ALTER TABLE jobs RENAME COLUMN media_type TO library;
ALTER TABLE jobs
    ADD CONSTRAINT jobs_status_check CHECK (status IN
        ('queued','processing','running','completed','failed','interrupted','skipped','cancelled')),
    ADD COLUMN IF NOT EXISTS profile          TEXT,
    ADD COLUMN IF NOT EXISTS updated_at       TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS speed            REAL,
    ADD COLUMN IF NOT EXISTS fps              REAL,
    ADD COLUMN IF NOT EXISTS eta_seconds      INTEGER,
    ADD COLUMN IF NOT EXISTS cancel_requested BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE jobs DROP COLUMN IF EXISTS pid;

CREATE INDEX IF NOT EXISTS jobs_library_status_idx ON jobs (library, status);
CREATE INDEX IF NOT EXISTS jobs_completed_idx ON jobs (completed_at DESC);

-- Trigger payload is now the library name. A new function name is used so
-- this works when the original was created by a different role.
CREATE OR REPLACE FUNCTION transcoder_notify_job() RETURNS trigger AS $$
BEGIN
    IF NEW.status IN ('queued','interrupted') THEN
        PERFORM pg_notify('transcoder_jobs', NEW.library);
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

-- media_files was written only by webhooks and never read by the new UI.
DROP TABLE IF EXISTS media_files;
DROP TABLE IF EXISTS stats;

COMMIT;
