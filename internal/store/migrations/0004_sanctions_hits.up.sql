CREATE TABLE IF NOT EXISTS sanctions_hits (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id  UUID         NOT NULL,
    list            TEXT         NOT NULL,
    matched_name    TEXT         NOT NULL,
    score           NUMERIC(5, 2) NOT NULL,
    raw_payload     JSONB        NOT NULL DEFAULT '{}'::jsonb,
    reviewed_by     TEXT,
    reviewed_at     TIMESTAMPTZ,
    disposition     TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT sanctions_hits_list_check
        CHECK (list IN ('OFAC', 'UN', 'EU', 'PEP')),
    CONSTRAINT sanctions_hits_disposition_check
        CHECK (disposition IS NULL OR disposition IN ('confirmed', 'false_positive', 'pending')),
    CONSTRAINT sanctions_hits_application_fk
        FOREIGN KEY (application_id) REFERENCES kyc_applications (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_sanctions_hits_application_id
    ON sanctions_hits (application_id);

CREATE INDEX IF NOT EXISTS idx_sanctions_hits_list
    ON sanctions_hits (list);