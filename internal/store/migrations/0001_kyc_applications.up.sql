CREATE TABLE IF NOT EXISTS kyc_applications (
    id                    UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id               TEXT         NOT NULL,
    vendor                TEXT         NOT NULL,
    vendor_application_id TEXT,
    state                 TEXT         NOT NULL DEFAULT 'started',
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at            TIMESTAMPTZ,
    re_kyc_due_at         TIMESTAMPTZ,
    CONSTRAINT kyc_applications_vendor_check
        CHECK (vendor IN ('onfido', 'sumsub')),
    CONSTRAINT kyc_applications_state_check
        CHECK (state IN ('started', 'documents_uploaded', 'liveness_passed',
                         'screening', 'vendor_decision', 'pass', 'fail',
                         'manual_review'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_kyc_applications_user_id
    ON kyc_applications (user_id);

CREATE INDEX IF NOT EXISTS idx_kyc_applications_re_kyc_due_at
    ON kyc_applications (re_kyc_due_at);

CREATE INDEX IF NOT EXISTS idx_kyc_applications_state
    ON kyc_applications (state);