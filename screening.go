package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ScreeningClient screens an applicant name against sanctions/PEP lists.
type ScreeningClient interface {
	Screen(ctx context.Context, fullName string) ([]ScreeningHit, error)
}

// ScreeningHit is a single match returned by the screening client.
type ScreeningHit struct {
	List        string
	MatchedName string
	Score       float64
}

// InMemoryScreeningClient matches applicant names against an in-memory
// sanctions list using case-insensitive substring matching.
type InMemoryScreeningClient struct {
	mu    sync.Mutex
	names []string
}

// NewInMemoryScreeningClient seeds a default sanctions list.
func NewInMemoryScreeningClient() *InMemoryScreeningClient {
	return &InMemoryScreeningClient{
		names: []string{"BAD ACTOR", "EVIL DOER"},
	}
}

// Screen returns hits whose listed name is a case-insensitive substring of
// the applicant full name.
func (c *InMemoryScreeningClient) Screen(ctx context.Context, fullName string) ([]ScreeningHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	hay := strings.ToUpper(strings.TrimSpace(fullName))
	var hits []ScreeningHit
	for _, n := range c.names {
		if strings.Contains(hay, strings.ToUpper(n)) {
			hits = append(hits, ScreeningHit{
				List:        "internal_sanctions",
				MatchedName: n,
				Score:       1.0,
			})
		}
	}
	return hits, nil
}

// ScreeningThreshold returns the hit count at or above which an application is
// routed to manual_review. Defaults to 1; overridable via
// SCREENING_HIT_THRESHOLD env.
func ScreeningThreshold() int {
	if v := os.Getenv("SCREENING_HIT_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

// ScreeningService runs screening and records hits in the audit/screening
// store.
type ScreeningService struct {
	screener ScreeningClient
	repo     *ApplicationRepository
	store    *ScreeningStore
	audit    *AuditLog
	now      func() time.Time
}

// NewScreeningService wires the screening service.
func NewScreeningService(screener ScreeningClient, repo *ApplicationRepository, store *ScreeningStore, audit *AuditLog) *ScreeningService {
	return &ScreeningService{
		screener: screener,
		repo:     repo,
		store:    store,
		audit:    audit,
		now:      time.Now,
	}
}

// Run screens the applicant and persists hits, returning the hits and
// whether the threshold was exceeded (manual review required).
func (s *ScreeningService) Run(ctx context.Context, app *Application, fullName string) ([]SanctionsHit, bool, error) {
	hits, err := s.screener.Screen(ctx, fullName)
	if err != nil {
		return nil, false, err
	}
	var persisted []SanctionsHit
	for _, h := range hits {
		sh := SanctionsHit{
			ID:            newUUID(),
			ApplicationID: app.ID,
			List:          h.List,
			MatchedName:   h.MatchedName,
			Score:         h.Score,
		}
		s.store.Add(app.ID, sh)
		persisted = append(persisted, sh)
	}
	threshold := ScreeningThreshold()
	manualReview := len(hits) >= threshold
	if s.audit != nil {
		s.audit.Record(app.ID, "screening", "system", map[string]any{
			"hits":         len(hits),
			"manual_review": manualReview,
		})
	}
	return persisted, manualReview, nil
}

// Disposition records an analyst disposition on the application's screening
// and transitions the application from manual_review to pass/fail.
func (s *ScreeningService) Disposition(ctx context.Context, appID, disposition, reviewedBy string) error {
	if disposition != "clear" && disposition != "block" {
		return errBadDisposition
	}
	app, err := s.repo.Get(appID)
	if err != nil {
		return err
	}
	if app.State != StateManualReview {
		return errNotInReview
	}
	s.store.SetDisposition(appID, reviewedBy, disposition)
	var to State
	if disposition == "clear" {
		to = StatePass
	} else {
		to = StateFail
	}
	if _, err := s.repo.UpdateState(appID, app.Version, to, reviewedBy, "screening disposition: "+disposition); err != nil {
		return err
	}
	if s.audit != nil {
		s.audit.Record(appID, "screening_disposition", reviewedBy, map[string]any{
			"disposition": disposition,
		})
	}
	return nil
}

var errBadDisposition = NewAppError("bad_disposition", "disposition must be clear or block", 400)
var errNotInReview = NewAppError("not_in_review", "application not in manual_review", 409)

// ScreeningStore holds sanctions hits per application in memory.
type ScreeningStore struct {
	mu   sync.Mutex
	hits map[string][]SanctionsHit
}

// NewScreeningStore creates a new ScreeningStore.
func NewScreeningStore() *ScreeningStore {
	return &ScreeningStore{hits: make(map[string][]SanctionsHit)}
}

// Add appends a hit for an application.
func (s *ScreeningStore) Add(appID string, hit SanctionsHit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits[appID] = append(s.hits[appID], hit)
}

// List returns the hits for an application.
func (s *ScreeningStore) List(appID string) []SanctionsHit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SanctionsHit, len(s.hits[appID]))
	copy(out, s.hits[appID])
	return out
}

// SetDisposition marks all hits for an application as reviewed.
func (s *ScreeningStore) SetDisposition(appID, reviewedBy, disposition string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for i := range s.hits[appID] {
		s.hits[appID][i].ReviewedBy = reviewedBy
		s.hits[appID][i].ReviewedAt = now
		s.hits[appID][i].Disposition = disposition
	}
}