CREATE TABLE IF NOT EXISTS documents (
    id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id     UUID         NOT NULL,
    type               TEXT         NOT NULL,
    object_key         TEXT         NOT NULL,
    vendor_document_id TEXT,
    uploaded_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    retention_until    TIMESTAMPTZ  NOT NULL,
    CONSTRAINT documents_type_check
        CHECK (type IN ('id_front', 'id_back', 'selfie', 'poa')),
    CONSTRAINT documents_application_fk
        FOREIGN KEY (application_id) REFERENCES kyc_applications (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_documents_application_id
    ON documents (application_id);

CREATE INDEX IF NOT EXISTS idx_documents_retention_until
    ON documents (retention_until);