package internal

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Metrics endpoint tests.
// ---------------------------------------------------------------------------

func TestMetricsHandler(t *testing.T) {
	// Prime a few counters via the full request path.
	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	createApp(t, srv, "metrics-user")

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	metricsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"onboarding_kyc_requests_total",
		"onboarding_kyc_applications_created_total",
		"onboarding_kyc_state_transitions_total",
		"onboarding_kyc_request_latency_seconds_bucket",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q\n%s", want, body)
		}
	}
}

func TestMetricsRoutingExposesEndpoint(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200 got %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "onboarding_kyc_") {
		t.Fatalf("body does not contain metrics: %s", b)
	}
}

func TestMetricsCountersIncrement(t *testing.T) {
	// Snapshot counters before.
	beforeCreate := globalMetrics.createAppTotal.Load()
	beforeTrans := globalMetrics.transitionsTotal.Load()

	s := newTestServices()
	srv := newTestServer(s)
	defer srv.Close()
	createApp(t, srv, "counter-user")

	if globalMetrics.createAppTotal.Load() != beforeCreate+1 {
		t.Errorf("createAppTotal: want %d got %d", beforeCreate+1, globalMetrics.createAppTotal.Load())
	}
	// The create flow also runs a vendor CreateApplicant + a state transition
	// (started -> documents_uploaded is NOT triggered on create alone, but
	// screening disposition / liveness pass would). At minimum the vendor
	// call counter should have advanced.
	if globalMetrics.vendorCallsTotal.Load() == 0 {
		t.Error("vendorCallsTotal not incremented")
	}
	_ = beforeTrans
}