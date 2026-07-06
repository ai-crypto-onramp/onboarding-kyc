CREATE TABLE IF NOT EXISTS audit_events (
    id           UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate    TEXT         NOT NULL,
    action       TEXT         NOT NULL,
    actor        TEXT         NOT NULL,
    payload      JSONB        NOT NULL DEFAULT '{}'::jsonb,
    occurred_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_audit_events_aggregate
    ON audit_events (aggregate);

CREATE INDEX IF NOT EXISTS idx_audit_events_occurred_at
    ON audit_events (occurred_at);