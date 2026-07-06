CREATE TABLE IF NOT EXISTS webhook_events (
    id           BIGSERIAL   PRIMARY KEY,
    vendor       TEXT        NOT NULL,
    event_id     TEXT        NOT NULL,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    raw_payload  JSONB       NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT webhook_events_vendor_check
        CHECK (vendor IN ('onfido', 'sumsub')),
    CONSTRAINT webhook_events_event_id_unique
        UNIQUE (vendor, event_id)
);

CREATE INDEX IF NOT EXISTS idx_webhook_events_received_at
    ON webhook_events (received_at);