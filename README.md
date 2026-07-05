# Onboarding / KYC

Go service that orchestrates identity verification (document + liveness), sanctions/PEP screening, and KYC decisioning at signup, feeding results downstream to the Policy/Risk Engine.

## Overview / Responsibilities

- Orchestrate end-to-end KYC flow across identity-verification vendors (Onfido / Sumsub).
- Collect government-issued ID documents and run liveness (selfie) checks.
- Screen applicants against sanctions lists (OFAC, UN, EU) and PEP databases at signup.
- Maintain a KYC status state machine (`pending` / `pass` / `fail` / `review`) per applicant.
- Schedule and trigger periodic re-KYC for existing users.
- Retain verification evidence (documents, liveness captures, vendor reports, decisions) per the defined retention policy to support compliance and audits.
- Emit KYC decisions and lifecycle events to downstream consumers (Policy/Risk Engine, Audit Event Log).

## Language & Tech Stack

- **Language:** Go (standard library + `chi` / `gin` HTTP router, `pgx` for PostgreSQL).
- **Vendor SDKs:** Onfido and Sumsub REST clients (one selected at runtime via `VENDOR_PROVIDER`).
- **Screening data:** Sanctions lists (OFAC SDN, UN Consolidated, EU Financial Sanctions) and PEP databases (commercial provider + internal allow/deny lists).
- **Storage:** PostgreSQL for application state; S3-compatible object storage for documents and liveness media.
- **Observability:** structured logging (slog), OpenTelemetry traces, Prometheus metrics.

## System Requirements

The service MUST:

- Orchestrate a multi-step KYC flow across vendors: create application → upload documents → liveness check → sanctions/PEP screening → vendor decision → record outcome.
- Support document upload (front/back of ID, selfie, proof of address) and a vendor-side liveness check, surfacing errors (blur, glare, mismatch) back to the client.
- Run sanctions and PEP screening at signup and on each re-KYC, persisting hits with source list, match score, and raw payload for analyst review.
- Implement a KYC status state machine with transitions among: `pending` / `pass` / `fail` / `review`, and enforce valid transitions only (idempotent re-evaluation per user).
- Schedule re-KYC at a configurable cadence (e.g. periodic or risk-triggered) and queue applicants whose verification is expiring.
- Apply an evidence retention policy: documents and vendor reports retained for a configurable number of days, then hard-deleted / redacted per policy.
- Expose synchronous read APIs for the API Gateway and Transaction Orchestrator to query KYC status by user.
- Accept idempotent vendor callbacks (webhooks) and reconcile them against the internal state machine.
- Emit events on every state transition and decision for the Audit Event Log and the Policy/Risk Engine.

## Non-Functional Requirements

- **Vendor call timeouts:** outbound vendor API calls MUST use per-request timeouts (connect ≤ 2s, overall ≤ 30s); webhook acknowledgements MUST return within 5s.
- **Idempotent callbacks:** webhook endpoints MUST deduplicate by vendor event id and be safe to retry; replaying an event MUST not advance the state machine twice.
- **PII encryption at rest:** documents and PII fields MUST be encrypted at rest (DB column-level encryption + envelope encryption for objects in object storage); access logged.
- **Audit trail:** every state transition, vendor call, screening result, and manual override MUST be written to the Audit Event Log (append-only) with actor, timestamp, and reason.
- **Retry / backoff:** failed vendor calls MUST use exponential backoff with jitter and a max-attempts cap; non-2xx responses on idempotent endpoints MUST be retried, 4xx (except 408/429) MUST NOT.
- **Availability:** target 99.9% monthly uptime; vendor outages MUST degrade gracefully (queue applications in `review` rather than hard-failing users).
- **Performance:** status lookups (`GET /v1/kyc/status/:user_id`) ≤ 100ms p95; application creation ≤ 300ms p95 excluding vendor latency.
- **Security:** all inbound traffic TLS; webhook HMAC verification; least-privilege service accounts; no PII in logs/metrics.

## Technical Specifications

### API Surface

REST over HTTPS; JSON request/response bodies; ISO 8601 timestamps; correlation id propagated via `X-Correlation-Id`.

### Endpoints

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/kyc/applications` | Create a new KYC application for a user; returns application id and current state. |
| `GET` | `/v1/kyc/applications/:id` | Fetch application state, documents, screening results, and decision. |
| `POST` | `/v1/kyc/applications/:id/documents` | Upload a document (multipart) for the application; stores in object storage and registers with vendor. |
| `POST` | `/v1/kyc/applications/:id/liveness` | Initiate / submit a liveness session (challenges, captures) against the vendor. |
| `GET` | `/v1/kyc/status/:user_id` | Return the user's current KYC status (`pending` / `pass` / `fail` / `review`) and last decision timestamp. |
| `POST` | `/v1/webhooks/onfido` | Onfido callback receiver (HMAC-verified); reconciles vendor events to internal state. |
| `POST` | `/v1/webhooks/sumsub` | Sumsub callback receiver (HMAC-verified); reconciles vendor events to internal state. |

### Data Model

- `kyc_applications` — `id`, `user_id`, `vendor`, `vendor_application_id`, `state`, `created_at`, `updated_at`, `expires_at`, `re_kyc_due_at`.
- `documents` — `id`, `application_id`, `type` (id_front/id_back/selfie/poa), `object_key`, `vendor_document_id`, `uploaded_at`, `retention_until`.
- `liveness_sessions` — `id`, `application_id`, `vendor_session_id`, `status`, `started_at`, `completed_at`, `result`.
- `sanctions_hits` — `id`, `application_id`, `list` (OFAC/UN/EU/PEP), `matched_name`, `score`, `raw_payload`, `reviewed_by`, `reviewed_at`, `disposition`.
- `kyc_decisions` — `id`, `application_id`, `outcome` (pass/fail/manual_review), `reason`, `decided_by` (vendor/analyst/system), `decided_at`.

### State Machine

```
started -> documents_uploaded -> liveness_passed -> screening -> vendor_decision -> pass | fail | manual_review
```

- `started` — application created, awaiting documents.
- `documents_uploaded` — required documents registered; liveness may proceed.
- `liveness_passed` — liveness check passed by vendor.
- `screening` — sanctions/PEP screening in progress.
- `vendor_decision` — vendor + screening results received; awaiting final adjudication.
- `pass` / `fail` / `manual_review` — terminal states; `manual_review` transitions to `pass` or `fail` after analyst action. Any terminal state can be re-opened by a re-KYC event back to `started`.

### Integrations

- **Onfido / Sumsub** — applicant creation, document/liveness submission, result retrieval via webhook.
- **Sanctions / PEP data providers** — screening API (and scheduled list sync) for OFAC, UN, EU and PEP matches.
- **Policy/Risk Engine** (downstream) — receives KYC decisions and status transitions as events to inform per-tx gating.
- **Audit Event Log** — receives an event per state transition, vendor call, screening hit, and decision.

### Webhook Security

- All webhook endpoints verify an HMAC-SHA256 signature over the raw request body using the per-vendor shared secret (`ONFIDO_WEBHOOK_SECRET` / `SUMSUB_WEBHOOK_SECRET`).
- Signature header expected from vendor (`X-Signature` / `X-Webhook-Signature`) is compared in constant time; mismatched signatures return `401` and are not processed.
- Replay protection via vendor event id dedup; stale events outside an accepted timestamp window (±5 min) are rejected.

## Dependencies

- **PostgreSQL** — application, document, liveness, screening, and decision records.
- **S3-compatible object storage** — encrypted storage of documents and liveness media (with retention/expiry lifecycle).
- **Vendor APIs** — Onfido and/or Sumsub (selected by `VENDOR_PROVIDER`).
- **Sanctions / PEP provider API** — for screening and list updates.
- **Policy/Risk Engine** — downstream consumer of KYC decisions.
- **Audit Event Log** — downstream consumer of KYC lifecycle events.
- **Identity & Auth** — validates user identity and session tokens on inbound requests.

## Configuration

Configuration is provided via environment variables:

| Variable | Description | Default |
|---|---|---|
| `PORT` | HTTP listen port | `8080` |
| `DB_URL` | PostgreSQL DSN | — |
| `VENDOR_PROVIDER` | Active KYC vendor (`onfido` / `sumsub`) | `onfido` |
| `ONFIDO_API_KEY` | Onfido API key | — |
| `ONFIDO_API_BASE` | Onfido API base URL | `https://api.onfido.com/v3` |
| `ONFIDO_WEBHOOK_SECRET` | Onfido webhook HMAC secret | — |
| `SUMSUB_API_KEY` | Sumsub API key | — |
| `SUMSUB_API_BASE` | Sumsub API base URL | `https://api.sumsub.com` |
| `SUMSUB_WEBHOOK_SECRET` | Sumsub webhook HMAC secret | — |
| `SANCTIONS_API_KEY` | Sanctions/PEP provider API key | — |
| `SANCTIONS_API_BASE` | Sanctions/PEP provider base URL | — |
| `S3_BUCKET` | Object storage bucket for documents | — |
| `S3_REGION` | Object storage region | `us-east-1` |
| `S3_ENDPOINT` | Override endpoint for S3-compatible storage | — |
| `RETENTION_DAYS` | Evidence retention window in days | `1095` |
| `RE_KYC_INTERVAL_DAYS` | Periodic re-KYC cadence in days | `365` |
| `VENDOR_CALL_TIMEOUT` | Outbound vendor call timeout | `30s` |
| `WEBHOOK_TOLERANCE_SECONDS` | Max webhook timestamp skew | `300` |
| `LOG_LEVEL` | Log level (`debug` / `info` / `warn` / `error`) | `info` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OpenTelemetry collector endpoint | — |

## Local Development

```sh
# Build
go build ./...

# Run (requires PostgreSQL, object storage, and vendor creds)
go run ./cmd/onboarding-kyc

# Test
go test ./...

# Lint / vet
go vet ./...
golangci-lint run
```