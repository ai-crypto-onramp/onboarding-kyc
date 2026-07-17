-- 0002_retention.up.sql
-- Add retention_until to liveness_sessions so the retention sweeper can prune
-- expired sessions alongside documents.

BEGIN;

ALTER TABLE liveness_sessions
    ADD COLUMN IF NOT EXISTS retention_until TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_liveness_retention
    ON liveness_sessions(retention_until)
    WHERE retention_until IS NOT NULL;

COMMIT;