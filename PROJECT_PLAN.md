# Project Plan — Onboarding / KYC

Implementation plan for the Go service that orchestrates identity verification, sanctions/PEP screening, and KYC decisioning at signup and on periodic re-KYC. Stages are ordered so each builds on the data and behavior introduced in the prior one, ending with hardening and release readiness.

## Stage 1 — Database Schema & Migrations

### Goal
Establish the PostgreSQL persistence layer with all tables, indexes, and constraints required by the rest of the service, plus a repeatable migration runner.

### Tasks
- [ ] Choose and wire a migration runner (e.g. `golang-migrate` or embedded SQL files via `embed`).
- [ ] Add `pgx` pool bootstrap from `DB_URL` with sane defaults (max conns, statement cache, timeouts).
- [ ] Create `kyc_applications` (id UUID PK, user_id, vendor, vendor_application_id, state enum, created_at, updated_at, expires_at, re_kyc_due_at) with unique index on `user_id`.
- [ ] Create `documents` (id, application_id FK, type enum, object_key, vendor_document_id, uploaded_at, retention_until).
- [ ] Create `liveness_sessions` (id, application_id FK, vendor_session_id, status, started_at, completed_at, result jsonb).
- [ ] Create `sanctions_hits` (id, application_id FK, list enum, matched_name, score, raw_payload jsonb, reviewed_by, reviewed_at, disposition).
- [ ] Create `kyc_decisions` (id, application_id FK, outcome enum, reason, decided_by enum, decided_at).
- [ ] Add `webhook_events` (vendor, event_id UNIQUE, received_at, raw_payload) for idempotent webhook dedup.
- [ ] Add an `audit_events` outbox table (id, aggregate, action, actor, payload jsonb, occurred_at) for the audit emission stage.
- [ ] Column-level encryption helpers for PII fields (application-level envelope keys).
- [ ] Indexes on hot read paths: `kyc_applications(user_id)`, `kyc_applications(re_kyc_due_at)`, `kyc_applications(state)`.

### Acceptance criteria
- `migrate up` / `migrate down` apply cleanly on an empty database and round-trip.
- All tables match the README data model; enums enforce the documented values.
- `pgx` pool connects and pings under configuration from `DB_URL`.

## Stage 2 — KYC Application State Machine

### Goal
Implement the state machine and the core application repository so transitions are validated centrally and persisted atomically.

### Tasks
- [ ] Define `State` enum and transition table (`started -> documents_uploaded -> liveness_passed -> screening -> vendor_decision -> pass | fail | manual_review`).
- [ ] Allow `manual_review -> pass | fail` and re-open from terminal states back to `started` on re-KYC.
- [ ] Implement `ApplicationRepository` with `Create`, `Get`, `GetByUserID`, `UpdateState` (guarded by transition table).
- [ ] Add optimistic concurrency via `updated_at` / version column to prevent lost updates.
- [ ] Emit an in-memory domain event on each transition for later audit wiring.
- [ ] Unit tests for every legal/illegal transition and concurrency conflict.

### Acceptance criteria
- Illegal transitions return a typed error and do not persist.
- Re-evaluation of the same user is idempotent (no duplicate application, no double state advance).
- Concurrency test demonstrates a conflicting `UpdateState` fails without clobbering state.

## Stage 3 — HTTP API Skeleton & Status Endpoints

### Goal
Stand up the HTTP server, router (`chi`), middleware chain, and the synchronous read/write endpoints that the API Gateway and Transaction Orchestrator depend on.

### Tasks
- [ ] Bootstrap HTTP server with `PORT`, graceful shutdown, request timeout middleware.
- [ ] Correlation id middleware (`X-Correlation-Id`), structured `slog` request logging, Prometheus metrics middleware.
- [ ] `POST /v1/kyc/applications` — create application, returns id + state.
- [ ] `GET /v1/kyc/applications/:id` — full application view (state, documents, screening, decision).
- [ ] `GET /v1/kyc/status/:user_id` — current status + last decision timestamp, target ≤ 100ms p95.
- [ ] Request/response DTOs with ISO 8601 timestamps and validation.
- [ ] Error envelope type with consistent status codes.

### Acceptance criteria
- All three endpoints return documented shapes and codes; status lookup meets p95 target in benchmarks.
- Server shuts down cleanly on SIGTERM, draining in-flight requests.
- Logs include correlation id; no PII in log lines.

## Stage 4 — Document Upload & Object Storage

### Goal
Accept multipart document uploads, persist metadata, and store encrypted objects in S3-compatible storage with retention metadata.

### Tasks
- [ ] `POST /v1/kyc/applications/:id/documents` multipart handler validating type (`id_front`/`id_back`/`selfie`/`poa`), size, MIME.
- [ ] S3 client abstraction (AWS SDK or minio-compatible) configured via `S3_BUCKET`/`S3_REGION`/`S3_ENDPOINT`.
- [ ] Envelope-encrypt objects before PUT (KMS/data key), store ciphertext + key reference in metadata.
- [ ] Compute `retention_until = uploaded_at + RETENTION_DAYS` and store on the object and the `documents` row.
- [ ] Register document with vendor (deferred to Stage 5 interface; stub the call here).
- [ ] Transition application `started -> documents_uploaded` once required doc set is satisfied.
- [ ] Unit + integration tests with an in-memory S3 (e.g. `testcontainers` or minio).

### Acceptance criteria
- Uploaded files are encrypted at rest and retrievable only through the service.
- `documents` rows and object keys are consistent; retention timestamps set.
- Application advances state only when the required document set is complete.

## Stage 5 — Vendor Integration (Onfido / Sumsub)

### Goal
Abstract KYC vendor calls behind a single interface selected at runtime, with timeouts, retry/backoff, and applicant + document + report orchestration.

### Tasks
- [ ] Define `VendorClient` interface: `CreateApplicant`, `UploadDocument`, `StartLiveness`, `GetReport`, `ParseWebhook`.
- [ ] Implement `OnfidoClient` and `SumsubClient` REST adapters over `net/http`.
- [ ] Per-request timeouts (connect ≤ 2s, overall `VENDOR_CALL_TIMEOUT` ≤ 30s) via `http.Client` config.
- [ ] Exponential backoff with jitter and max-attempts; retry 5xx, 408, 429; do not retry other 4xx.
- [ ] Select implementation by `VENDOR_PROVIDER`; fail fast on unknown provider.
- [ ] Graceful degradation: vendor outage queues applications in `review` rather than hard-failing the user.
- [ ] Record every vendor call into the audit outbox with actor, latency, status.

### Acceptance criteria
- Switching `VENDOR_PROVIDER` swaps the active client with no other code changes.
- Timeouts and retries verified via httptest handlers simulating slow/5xx responses.
- Vendor outage path leaves application in `review` and surfaces an actionable error.

## Stage 6 — Liveness Check

### Goal
Initiate and reconcile vendor liveness sessions, surfacing client-meaningful errors (blur, glare, mismatch).

### Tasks
- [ ] `POST /v1/kyc/applications/:id/liveness` — start/submit liveness session via `VendorClient`.
- [ ] Persist `liveness_sessions` row with vendor session id and status.
- [ ] Map vendor liveness result codes to a stable internal enum and to client-facing error strings.
- [ ] On pass, transition `documents_uploaded -> liveness_passed`; on failure, leave application and return structured error.
- [ ] Emit audit event on liveness start and completion.
- [ ] Tests covering pass, fail, and ambiguous/timeout outcomes.

### Acceptance criteria
- Liveness outcomes reconcile to the correct internal state transition.
- Error responses carry the documented reason categories without leaking vendor payloads.
- Liveness session lifecycle is recoverable after a transient vendor error.

## Stage 7 — Sanctions / PEP Screening

### Goal
Screen applicants against OFAC SDN, UN Consolidated, EU Financial Sanctions, and PEP databases at signup and re-KYC, persisting hits for analyst review.

### Tasks
- [ ] `ScreeningClient` abstraction over the sanctions/PEP provider API keyed by `SANCTIONS_API_KEY`/`SANCTIONS_API_BASE`.
- [ ] Run screening after `liveness_passed`, transition to `screening`.
- [ ] Persist `sanctions_hits` with list, matched_name, score, raw_payload; mark application `manual_review` when hits exceed threshold.
- [ ] Allow analyst disposition (`reviewed_by`, `reviewed_at`, `disposition`) via an internal endpoint or job.
- [ ] Scheduled list sync job (optional) writing local snapshots for offline matching.
- [ ] Audit event per screening run and per hit disposition.
- [ ] Tests for clean, single-hit, and multi-hit scenarios with a mock provider.

### Acceptance criteria
- Hits are persisted with enough data for analyst review and downstream audit.
- Threshold-based routing to `manual_review` is configurable and tested.
- Re-KYC re-runs screening and records new hits without clobbering prior dispositions.

## Stage 8 — Webhook Ingestion & HMAC Verification

### Goal
Receive idempotent vendor callbacks, verify HMAC signatures, and reconcile events against the internal state machine.

### Tasks
- [ ] `POST /v1/webhooks/onfido` and `POST /v1/webhooks/sumsub` handlers reading raw body.
- [ ] HMAC-SHA256 verification over raw body using per-vendor secrets; constant-time compare; 401 on mismatch.
- [ ] Timestamp skew check (±`WEBHOOK_TOLERANCE_SECONDS`) and dedup via `webhook_events.event_id`.
- [ ] Acknowledge within 5s; process asynchronously if reconciliation is slow.
- [ ] Reconcile event to state machine via `VendorClient.ParseWebhook` + `ApplicationRepository.UpdateState`.
- [ ] Replaying the same event MUST not advance the state machine twice (idempotent).
- [ ] Audit event per accepted webhook, per rejection, and per state advance.

### Acceptance criteria
- Tampered body, stale timestamp, or duplicate event id is rejected and not processed.
- Replay tests confirm no double state advance.
- Handlers return within 5s under normal load.

## Stage 9 — Re-KYC Scheduling & Retention

### Goal
Periodically and on risk triggers re-open expiring applications and enforce the evidence retention policy.

### Tasks
- [ ] Scheduler (cron-like or tick loop) selecting applications where `re_kyc_due_at <= now` and transitioning terminal -> `started`.
- [ ] Compute `re_kyc_due_at = decided_at + RE_KYC_INTERVAL_DAYS` on decision.
- [ ] Risk-triggered re-KYC via an internal endpoint or event from the Policy/Risk Engine.
- [ ] Retention sweeper hard-deleting/redacting objects and rows past `retention_until`.
- [ ] Audit event on each re-KYC trigger and each retention deletion.
- [ ] Tests with a fake clock to advance `re_kyc_due_at` and `retention_until`.

### Acceptance criteria
- Expired applications are re-opened exactly once even under scheduler contention.
- Retention sweeper removes objects and corresponding `documents`/`liveness_sessions` rows consistently.
- Re-KYC re-runs documents/liveness/screening without losing prior decision history.

## Stage 10 — Audit Emission, Observability & Release Hardening

### Goal
Emit lifecycle events to downstream consumers, complete observability, and package the service for production.

### Tasks
- [ ] Audit outbox publisher: append-only writer to the Audit Event Log with at-least-once delivery and dedup by event id.
- [ ] Publish KYC decisions and state transitions to the Policy/Risk Engine (sync or async per contract).
- [ ] OpenTelemetry tracing spans on HTTP handlers, vendor calls, DB transactions; exporter via `OTEL_EXPORTER_OTLP_ENDPOINT`.
- [ ] Prometheus metrics: request counts/latency, state occupancy, vendor call outcomes, screening hit rate, webhook dedup hits.
- [ ] Structured `slog` logging with `LOG_LEVEL`, no PII, correlation id propagation.
- [ ] Lint (`go vet`, `golangci-lint`) and `go test -race -cover` passing; coverage gate per `codecov.yml`.
- [ ] Dockerfile multi-stage build producing a minimal image; `docker-build`/`docker-run` Makefile targets verified.
- [ ] README operations section (runbook pointers, config reference already exists).

### Acceptance criteria
- Every state transition, vendor call, screening result, and manual override produces an audit event with actor, timestamp, reason.
- Dashboards/metrics expose the SLOs in the README; no PII in any telemetry stream.
- `make test`, `make lint`, and `make docker-build` all green in CI; coverage meets the configured threshold.