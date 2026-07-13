-- 0002_retention.down.sql
-- Reverse of 0002_retention.up.sql.

BEGIN;

DROP INDEX IF EXISTS idx_liveness_retention;
ALTER TABLE liveness_sessions DROP COLUMN IF EXISTS retention_until;

COMMIT;