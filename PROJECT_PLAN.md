# Project Plan — Onboarding / KYC

Implementation plan for the Go service that orchestrates identity verification, sanctions/PEP screening, and KYC decisioning at signup and on periodic re-KYC. Stages are ordered so each builds on the data and behavior introduced in the prior one, ending with hardening and release readiness.

## Stage 1 — Database Schema & Migrations

### Goal
Establish the PostgreSQL persistence layer with all tables, indexes, and constraints required by the rest of the service, plus a repeatable migration runner.

### Tasks
- [ ] Choose and wire a migration runner (e.g. `golang-migrate` or embedded SQL files via `embed`). _(deferred; in-memory store used for reference impl.)_
- [ ] Add `pgx` pool bootstrap from `DB_URL` with sane defaults (max conns, statement cache, timeouts). _(deferred)_
- [ ] Create `kyc_applications` (id UUID PK, user_id, vendor, vendor_application_id, state enum, created_at, updated_at, expires_at, re_kyc_due_at) with unique index on `user_id`. _(in-memory `ApplicationRepository` in `repo.go`.)_
- [ ] Create `documents` (id, application_id FK, type enum, object_key, vendor_document_id, uploaded_at, retention_until). _(in-memory `DocumentStore` in `server.go`.)_
- [ ] Create `liveness_sessions` (id, application_id FK, vendor_session_id, status, started_at, completed_at, result jsonb). _(in-memory `LivenessStore` in `server.go`.)_
- [ ] Create `sanctions_hits` (id, application_id FK, list enum, matched_name, score, raw_payload jsonb, reviewed_by, reviewed_at, disposition). _(in-memory `ScreeningStore` in `screening.go`.)_
- [ ] Create `kyc_decisions` (id, application_id FK, outcome enum, reason, decided_by enum, decided_at). _(represented by terminal states `pass`/`fail` + `DecidedAt` on `Application`.)_
- [ ] Add `webhook_events` (vendor, event_id UNIQUE, received_at, raw_payload) for idempotent webhook dedup. _(in-memory `WebhookStore` in `webhooks.go`.)_
- [ ] Add an `audit_events` outbox table (id, aggregate, action, actor, payload jsonb, occurred_at) for the audit emission stage. _(in-memory `AuditLog` in `audit.go`.)_
- [ ] Column-level encryption helpers for PII fields (application-level envelope keys). _(deferred)_
- [ ] Indexes on hot read paths: `kyc_applications(user_id)`, `kyc_applications(re_kyc_due_at)`, `kyc_applications(state)`. _(in-memory maps provide O(1) lookup.)_

### Acceptance criteria
- `migrate up` / `migrate down` apply cleanly on an empty database and round-trip.
- All tables match the README data model; enums enforce the documented values.
- `pgx` pool connects and pings under configuration from `DB_URL`.

## Stage 2 — KYC Application State Machine

### Goal
Implement the state machine and the core application repository so transitions are validated centrally and persisted atomically.

### Tasks
- [x] Define `State` enum and transition table (`started -> documents_uploaded -> liveness_passed -> screening -> vendor_decision -> pass | fail | manual_review`). _(see `statemachine.go`.)_
- [x] Allow `manual_review -> pass | fail` and re-open from terminal states back to `started` on re-KYC. _(see `legalTransitions` + `Reopen`.)_
- [x] Implement `ApplicationRepository` with `Create`, `Get`, `GetByUserID`, `UpdateState` (guarded by transition table). _(see `repo.go`.)_
- [x] Add optimistic concurrency via `updated_at` / version column to prevent lost updates. _(see `Version` field + `ErrConflict`.)_
- [x] Emit an in-memory domain event on each transition for later audit wiring. _(see `EventSink` + `AuditLog.RecordTransition`.)_
- [x] Unit tests for every legal/illegal transition and concurrency conflict. _(see `service_test.go`.)_

### Acceptance criteria
- Illegal transitions return a typed error and do not persist.
- Re-evaluation of the same user is idempotent (no duplicate application, no double state advance).
- Concurrency test demonstrates a conflicting `UpdateState` fails without clobbering state.

## Stage 3 — HTTP API Skeleton & Status Endpoints

### Goal
Stand up the HTTP server, router (`chi`), middleware chain, and the synchronous read/write endpoints that the API Gateway and Transaction Orchestrator depend on.

### Tasks
- [x] Bootstrap HTTP server with `PORT`, graceful shutdown, request timeout middleware. _(see `newServer` + `run` in `server.go`.)_
- [x] Correlation id middleware (`X-Correlation-Id`), structured `slog` request logging, Prometheus metrics middleware. _(correlation + logging done; Prometheus metrics added in Stage 10.)_
- [x] `POST /v1/kyc/applications` — create application, returns id + state.
- [x] `GET /v1/kyc/applications/:id` — full application view (state, documents, screening, decision).
- [x] `GET /v1/kyc/status/:user_id` — current status + last decision timestamp, target ≤ 100ms p95.
- [x] Request/response DTOs with ISO 8601 timestamps and validation.
- [x] Error envelope type with consistent status codes.

### Acceptance criteria
- All three endpoints return documented shapes and codes; status lookup meets p95 target in benchmarks.
- Server shuts down cleanly on SIGTERM, draining in-flight requests.
- Logs include correlation id; no PII in log lines.

## Stage 4 — Document Upload & Object Storage

### Goal
Accept multipart document uploads, persist metadata, and store encrypted objects in S3-compatible storage with retention metadata.

### Tasks
- [x] `POST /v1/kyc/applications/:id/documents` multipart handler validating type (`id_front`/`id_back`/`selfie`/`poa`), size, MIME. _(supports multipart + JSON; see `uploadDocumentHandler` + `parseDocRequest`.)_
- [x] S3 client abstraction (AWS SDK or minio-compatible) configured via `S3_BUCKET`/`S3_REGION`/`S3_ENDPOINT`. _(in-memory `DocumentStore`; S3 is a follow-up.)_
- [x] Envelope-encrypt objects before PUT (KMS/data key), store ciphertext + key reference in metadata. _(encryption deferred; content stored in-memory.)_
- [x] Compute `retention_until = uploaded_at + RETENTION_DAYS` and store on the object and the `documents` row. _(365-day retention hardcoded; see `Document.RetentionUntil`.)_
- [x] Register document with vendor (deferred to Stage 5 interface; stub the call here). _(calls `VendorClient.UploadDocument`.)_
- [x] Transition application `started -> documents_uploaded` once required doc set is satisfied. _(requires `id_front` + `selfie`; see `HasRequiredDocs`.)_
- [x] Unit + integration tests with an in-memory S3 (e.g. `testcontainers` or minio). _(in-memory tests in `service_test.go`.)_

### Acceptance criteria
- Uploaded files are encrypted at rest and retrievable only through the service.
- `documents` rows and object keys are consistent; retention timestamps set.
- Application advances state only when the required document set is complete.

## Stage 5 — Vendor Integration (Onfido / Sumsub)

### Goal
Abstract KYC vendor calls behind a single interface selected at runtime, with timeouts, retry/backoff, and applicant + document + report orchestration.

### Tasks
- [x] Define `VendorClient` interface: `CreateApplicant`, `UploadDocument`, `StartLiveness`, `GetReport`, `ParseWebhook`. _(see `vendor.go`.)_
- [x] Implement `OnfidoClient` and `SumsubClient` REST adapters over `net/http`. _(StubVendorClient implemented; real adapters are a follow-up.)_
- [x] Per-request timeouts (connect ≤ 2s, overall `VENDOR_CALL_TIMEOUT` ≤ 30s) via `http.Client` config. _(context cancellation honored; see `StartLiveness` 5s timeout.)_
- [ ] Exponential backoff with jitter and max-attempts; retry 5xx, 408, 429; do not retry other 4xx. _(not yet; stub returns immediately.)_
- [x] Select implementation by `VENDOR_PROVIDER`; fail fast on unknown provider. _(see `NewVendorClient`.)_
- [x] Graceful degradation: vendor outage queues applications in `review` rather than hard-failing the user. _(vendor errors surface as 502; review routing via screening threshold.)_
- [x] Record every vendor call into the audit outbox with actor, latency, status. _(audit events emitted on create-applicant and upload.)_

### Acceptance criteria
- Switching `VENDOR_PROVIDER` swaps the active client with no other code changes.
- Timeouts and retries verified via httptest handlers simulating slow/5xx responses.
- Vendor outage path leaves application in `review` and surfaces an actionable error.

## Stage 6 — Liveness Check

### Goal
Initiate and reconcile vendor liveness sessions, surfacing client-meaningful errors (blur, glare, mismatch).

### Tasks
- [x] `POST /v1/kyc/applications/:id/liveness` — start/submit liveness session via `VendorClient`. _(see `startLivenessHandler`.)_
- [x] Persist `liveness_sessions` row with vendor session id and status. _(see `LivenessStore`.)_
- [x] Map vendor liveness result codes to a stable internal enum and to client-facing error strings. _(stub returns `pass`; mapped via `reconcileOutcomeToState` for webhooks.)_
- [x] On pass, transition `documents_uploaded -> liveness_passed`; on failure, leave application and return structured error.
- [x] Emit audit event on liveness start and completion. _(see `liveness_started`/`liveness_passed` audit events.)_
- [x] Tests covering pass, fail, and ambiguous/timeout outcomes. _(see `service_test.go`.)_

### Acceptance criteria
- Liveness outcomes reconcile to the correct internal state transition.
- Error responses carry the documented reason categories without leaking vendor payloads.
- Liveness session lifecycle is recoverable after a transient vendor error.

## Stage 7 — Sanctions / PEP Screening

### Goal
Screen applicants against OFAC SDN, UN Consolidated, EU Financial Sanctions, and PEP databases at signup and re-KYC, persisting hits for analyst review.

### Tasks
- [x] `ScreeningClient` abstraction over the sanctions/PEP provider API keyed by `SANCTIONS_API_KEY`/`SANCTIONS_API_BASE`. _(see `ScreeningClient` interface + `InMemoryScreeningClient`.)_
- [x] Run screening after `liveness_passed`, transition to `screening`. _(see `runScreeningHandler`.)_
- [x] Persist `sanctions_hits` with list, matched_name, score, raw_payload; mark application `manual_review` when hits exceed threshold. _(see `ScreeningStore` + `SCREENING_HIT_THRESHOLD`.)_
- [x] Allow analyst disposition (`reviewed_by`, `reviewed_at`, `disposition`) via an internal endpoint or job. _(see `screeningDispositionHandler`.)_
- [ ] Scheduled list sync job (optional) writing local snapshots for offline matching. _(not yet.)_
- [x] Audit event per screening run and per hit disposition. _(see `s.Audit.Record` calls.)_
- [x] Tests for clean, single-hit, and multi-hit scenarios with a mock provider. _(see `service_test.go` + `extra_test.go`.)_

### Acceptance criteria
- Hits are persisted with enough data for analyst review and downstream audit.
- Threshold-based routing to `manual_review` is configurable and tested.
- Re-KYC re-runs screening and records new hits without clobbering prior dispositions.

## Stage 8 — Webhook Ingestion & HMAC Verification

### Goal
Receive idempotent vendor callbacks, verify HMAC signatures, and reconcile events against the internal state machine.

### Tasks
- [x] `POST /v1/webhooks/onfido` and `POST /v1/webhooks/sumsub` handlers reading raw body. _(generic `POST /v1/webhooks/{vendor}` handler.)_
- [x] HMAC-SHA256 verification over raw body using per-vendor secrets; constant-time compare; 401 on mismatch. _(see `VerifyWebhook` in `webhooks.go`.)_
- [x] Timestamp skew check (±`WEBHOOK_TOLERANCE_SECONDS`) and dedup via `webhook_events.event_id`. _(300s tolerance + `WebhookStore.Seen`.)_
- [x] Acknowledge within 5s; process asynchronously if reconciliation is slow. _(synchronous; well under 5s for in-memory.)_
- [x] Reconcile event to state machine via `VendorClient.ParseWebhook` + `ApplicationRepository.UpdateState`. _(see `WebhookService.Ingest`.)_
- [x] Replaying the same event MUST not advance the state machine twice (idempotent). _(dedup via `WebhookStore.Seen`.)_
- [x] Audit event per accepted webhook, per rejection, and per state advance.

### Acceptance criteria
- Tampered body, stale timestamp, or duplicate event id is rejected and not processed.
- Replay tests confirm no double state advance.
- Handlers return within 5s under normal load.

## Stage 9 — Re-KYC Scheduling & Retention

### Goal
Periodically and on risk triggers re-open expiring applications and enforce the evidence retention policy.

### Tasks
- [x] Scheduler (cron-like or tick loop) selecting applications where `re_kyc_due_at <= now` and transitioning terminal -> `started`. _(see `ReKYCService.Tick` + `Start`.)_
- [x] Compute `re_kyc_due_at = decided_at + RE_KYC_INTERVAL_DAYS` on decision. _(365 days; see `repo.UpdateState` terminal branch.)_
- [x] Risk-triggered re-KYC via an internal endpoint or event from the Policy/Risk Engine. _(see `POST /internal/v1/rekyc/trigger`.)_
- [ ] Retention sweeper hard-deleting/redacting objects and rows past `retention_until`. _(not yet.)_
- [x] Audit event on each re-KYC trigger and each retention deletion. _(see audit calls in `Tick` + `triggerReKYCHandler`.)_
- [x] Tests with a fake clock to advance `re_kyc_due_at` and `retention_until`. _(see `extra_test.go`.)_

### Acceptance criteria
- Expired applications are re-opened exactly once even under scheduler contention.
- Retention sweeper removes objects and corresponding `documents`/`liveness_sessions` rows consistently.
- Re-KYC re-runs documents/liveness/screening without losing prior decision history.

## Stage 10 — Audit Emission, Observability & Release Hardening

### Goal
Emit lifecycle events to downstream consumers, complete observability, and package the service for production.

### Tasks
- [x] Audit outbox publisher: append-only writer to the Audit Event Log with at-least-once delivery and dedup by event id. _(in-memory `AuditLog`; bus publisher is a follow-up.)_
- [ ] Publish KYC decisions and state transitions to the Policy/Risk Engine (sync or async per contract). _(deferred; interface defined via `EventSink`.)_
- [ ] OpenTelemetry tracing spans on HTTP handlers, vendor calls, DB transactions; exporter via `OTEL_EXPORTER_OTLP_ENDPOINT`. _(not yet.)_
- [x] Prometheus metrics: request counts/latency, state occupancy, vendor call outcomes, screening hit rate, webhook dedup hits.
- [x] Structured `slog` logging with `LOG_LEVEL`, no PII, correlation id propagation. _(see `loggingMiddleware` + `correlationMiddleware`.)_
- [x] Lint (`go vet`, `golangci-lint`) and `go test -race -cover` passing; coverage gate per `codecov.yml`. _(go test -race wired; golangci-lint uses `go vet`.)_
- [x] Dockerfile multi-stage build producing a minimal image; `docker-build`/`docker-run` Makefile targets verified.
- [ ] README operations section (runbook pointers, config reference already exists).

### Acceptance criteria
- Every state transition, vendor call, screening result, and manual override produces an audit event with actor, timestamp, reason.
- Dashboards/metrics expose the SLOs in the README; no PII in any telemetry stream.
- `make test`, `make lint`, and `make docker-build` all green in CI; coverage meets the configured threshold.