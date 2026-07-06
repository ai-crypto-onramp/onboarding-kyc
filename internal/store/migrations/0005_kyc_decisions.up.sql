CREATE TABLE IF NOT EXISTS kyc_decisions (
    id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID         NOT NULL,
    outcome        TEXT         NOT NULL,
    reason         TEXT,
    decided_by     TEXT         NOT NULL,
    decided_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT kyc_decisions_outcome_check
        CHECK (outcome IN ('pass', 'fail', 'manual_review')),
    CONSTRAINT kyc_decisions_decided_by_check
        CHECK (decided_by IN ('vendor', 'analyst', 'system')),
    CONSTRAINT kyc_decisions_application_fk
        FOREIGN KEY (application_id) REFERENCES kyc_applications (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_kyc_decisions_application_id
    ON kyc_decisions (application_id);

CREATE INDEX IF NOT EXISTS idx_kyc_decisions_decided_at
    ON kyc_decisions (decided_at);