-- 0001_init.down.sql
-- Reverse of 0001_init.up.sql.

BEGIN;

DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS webhook_events;
DROP TABLE IF EXISTS kyc_decisions;
DROP TABLE IF EXISTS sanctions_hits;
DROP TABLE IF EXISTS liveness_sessions;
DROP TABLE IF EXISTS documents;
DROP TABLE IF EXISTS kyc_applications;

COMMIT;