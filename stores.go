package main

import "time"

// ReKYCDue identifies an application that is due for re-KYC.
type ReKYCDue struct {
	ID      string
	Version int
}

// ApplicationRepo persists KYC applications and guards state transitions with
// optimistic concurrency on the Version field.
type ApplicationRepo interface {
	Create(app *Application) error
	Get(id string) (*Application, error)
	GetByUserID(userID string) (*Application, error)
	UpdateState(id string, version int, newState State, actor, reason string) (*Application, error)
	Reopen(id string, version int, actor string) (*Application, error)
	ListDueForReKYC(now time.Time) []ReKYCDue
	SetVendorApplicantID(id, vendorID string)
}

// DocumentRepo persists uploaded documents keyed by application id.
type DocumentRepo interface {
	Add(appID string, doc Document)
	List(appID string) []Document
	HasRequiredDocs(appID string) bool
	SweepExpired(now time.Time) int
}

// LivenessRepo persists liveness sessions keyed by application id.
type LivenessRepo interface {
	Add(appID string, s LivenessSession)
	Latest(appID string) (LivenessSession, bool)
	SweepExpired(now time.Time) int
}

// Compile-time assertions that the in-memory stores satisfy the interfaces.
var (
	_ ApplicationRepo = (*ApplicationRepository)(nil)
	_ DocumentRepo    = (*DocumentStore)(nil)
	_ LivenessRepo    = (*LivenessStore)(nil)

	_ ApplicationRepo = (*DBApplicationRepo)(nil)
	_ DocumentRepo    = (*DBDocumentRepo)(nil)
	_ LivenessRepo    = (*DBLivenessRepo)(nil)
)