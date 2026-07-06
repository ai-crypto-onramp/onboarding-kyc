CREATE TABLE IF NOT EXISTS liveness_sessions (
    id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id   UUID         NOT NULL,
    vendor_session_id TEXT,
    status           TEXT         NOT NULL DEFAULT 'started',
    started_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ,
    result           JSONB,
    CONSTRAINT liveness_sessions_status_check
        CHECK (status IN ('started', 'passed', 'failed', 'error')),
    CONSTRAINT liveness_sessions_application_fk
        FOREIGN KEY (application_id) REFERENCES kyc_applications (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_liveness_sessions_application_id
    ON liveness_sessions (application_id);