-- 0001_init.up.sql
-- Initial schema for the onboarding-kyc service.
-- Conventions: UUID PKs (app-generated UUIDv7, no DB default), UPPER_CASE enum
-- TEXT (no CHECK), created_at + updated_at on every table, no DB triggers.

BEGIN;

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- KYC applications (aggregate root).
CREATE TABLE IF NOT EXISTS kyc_applications (
    id                    UUID PRIMARY KEY,
    user_id               TEXT NOT NULL,
    vendor                TEXT,
    vendor_application_id TEXT,
    state                 TEXT NOT NULL DEFAULT 'STARTED',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at            TIMESTAMPTZ,
    re_kyc_due_at         TIMESTAMPTZ,
    decided_at            TIMESTAMPTZ,
    version               INTEGER NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_kyc_apps_user_active ON kyc_applications(user_id) WHERE state NOT IN ('PASS','FAIL');
CREATE INDEX IF NOT EXISTS idx_kyc_apps_re_kyc ON kyc_applications(re_kyc_due_at) WHERE re_kyc_due_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_kyc_apps_state ON kyc_applications(state);

-- Uploaded documents.
CREATE TABLE IF NOT EXISTS documents (
    id                 UUID PRIMARY KEY,
    application_id     UUID NOT NULL REFERENCES kyc_applications(id) ON DELETE CASCADE,
    type               TEXT NOT NULL,
    object_key         TEXT,
    vendor_document_id TEXT,
    uploaded_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    retention_until     TIMESTAMPTZ NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_documents_app ON documents(application_id);

-- Liveness sessions.
CREATE TABLE IF NOT EXISTS liveness_sessions (
    id                UUID PRIMARY KEY,
    application_id    UUID NOT NULL REFERENCES kyc_applications(id) ON DELETE CASCADE,
    vendor_session_id TEXT,
    status            TEXT NOT NULL DEFAULT 'STARTED',
    started_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ,
    result            JSONB,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_liveness_app ON liveness_sessions(application_id);

-- Sanctions / PEP screening hits.
CREATE TABLE IF NOT EXISTS sanctions_hits (
    id             UUID PRIMARY KEY,
    application_id UUID NOT NULL REFERENCES kyc_applications(id) ON DELETE CASCADE,
    list           TEXT NOT NULL,
    matched_name   TEXT NOT NULL,
    score          DOUBLE PRECISION NOT NULL,
    raw_payload    JSONB,
    reviewed_by    TEXT,
    reviewed_at    TIMESTAMPTZ,
    disposition    TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_sanctions_app ON sanctions_hits(application_id);

-- KYC decisions.
CREATE TABLE IF NOT EXISTS kyc_decisions (
    id            UUID PRIMARY KEY,
    application_id UUID NOT NULL REFERENCES kyc_applications(id) ON DELETE CASCADE,
    outcome       TEXT NOT NULL,
    reason        TEXT,
    decided_by    TEXT NOT NULL,
    decided_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_decisions_app ON kyc_decisions(application_id);

-- Webhook events (idempotent dedup).
CREATE TABLE IF NOT EXISTS webhook_events (
    id          UUID PRIMARY KEY,
    vendor      TEXT NOT NULL,
    event_id    TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    raw_payload JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (vendor, event_id)
);

-- Audit events outbox.
CREATE TABLE IF NOT EXISTS audit_events (
    id          UUID PRIMARY KEY,
    aggregate   TEXT NOT NULL,
    action      TEXT NOT NULL,
    actor       TEXT,
    payload     JSONB NOT NULL DEFAULT '{}',
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_audit_events_aggregate ON audit_events(aggregate);

COMMIT;