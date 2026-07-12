-- 0001_init.up.sql
-- Initial schema for the onboarding-kyc service.

BEGIN;

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- KYC applications (aggregate root).
CREATE TABLE IF NOT EXISTS kyc_applications (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id              text NOT NULL,
    vendor               text,
    vendor_application_id text,
    state                text NOT NULL DEFAULT 'started'
                          CHECK (state IN ('started','documents_uploaded','liveness_passed','screening','vendor_decision','pass','fail','manual_review')),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    expires_at           timestamptz,
    re_kyc_due_at        timestamptz,
    decided_at           timestamptz,
    version              integer NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_kyc_apps_user_active ON kyc_applications(user_id) WHERE state NOT IN ('pass','fail');
CREATE INDEX IF NOT EXISTS idx_kyc_apps_re_kyc ON kyc_applications(re_kyc_due_at) WHERE re_kyc_due_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_kyc_apps_state ON kyc_applications(state);

-- Uploaded documents.
CREATE TABLE IF NOT EXISTS documents (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id     uuid NOT NULL REFERENCES kyc_applications(id) ON DELETE CASCADE,
    type               text NOT NULL CHECK (type IN ('id_front','id_back','selfie','poa')),
    object_key         text,
    vendor_document_id text,
    uploaded_at        timestamptz NOT NULL DEFAULT now(),
    retention_until     timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_documents_app ON documents(application_id);

-- Liveness sessions.
CREATE TABLE IF NOT EXISTS liveness_sessions (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id     uuid NOT NULL REFERENCES kyc_applications(id) ON DELETE CASCADE,
    vendor_session_id  text,
    status             text NOT NULL DEFAULT 'started',
    started_at         timestamptz NOT NULL DEFAULT now(),
    completed_at       timestamptz,
    result             jsonb
);
CREATE INDEX IF NOT EXISTS idx_liveness_app ON liveness_sessions(application_id);

-- Sanctions / PEP screening hits.
CREATE TABLE IF NOT EXISTS sanctions_hits (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid NOT NULL REFERENCES kyc_applications(id) ON DELETE CASCADE,
    list           text NOT NULL,
    matched_name   text NOT NULL,
    score          double precision NOT NULL,
    raw_payload    jsonb,
    reviewed_by    text,
    reviewed_at    timestamptz,
    disposition    text CHECK (disposition IN ('clear','block'))
);
CREATE INDEX IF NOT EXISTS idx_sanctions_app ON sanctions_hits(application_id);

-- KYC decisions.
CREATE TABLE IF NOT EXISTS kyc_decisions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid NOT NULL REFERENCES kyc_applications(id) ON DELETE CASCADE,
    outcome       text NOT NULL CHECK (outcome IN ('pass','fail','manual_review')),
    reason        text,
    decided_by    text NOT NULL,
    decided_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_decisions_app ON kyc_decisions(application_id);

-- Webhook events (idempotent dedup).
CREATE TABLE IF NOT EXISTS webhook_events (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    vendor      text NOT NULL,
    event_id    text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    raw_payload jsonb,
    UNIQUE (vendor, event_id)
);

-- Audit events outbox.
CREATE TABLE IF NOT EXISTS audit_events (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate   text NOT NULL,
    action      text NOT NULL,
    actor       text,
    payload     jsonb NOT NULL DEFAULT '{}',
    occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_audit_events_aggregate ON audit_events(aggregate);

COMMIT;