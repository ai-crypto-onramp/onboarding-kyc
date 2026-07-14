package internal

import (
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Prometheus-style metrics: counters for requests, state transitions, vendor
// calls, screening, webhooks, and re-KYC. Exposed via /metrics.
// ---------------------------------------------------------------------------

type metrics struct {
	requestsTotal      atomic.Int64
	createAppTotal    atomic.Int64
	uploadDocTotal    atomic.Int64
	livenessTotal     atomic.Int64
	screeningTotal    atomic.Int64
	screeningHits     atomic.Int64
	webhookAccept     atomic.Int64
	webhookReject     atomic.Int64
	webhookDuplicate  atomic.Int64
	rekycTotal        atomic.Int64
	vendorCallsTotal  atomic.Int64
	transitionsTotal  atomic.Int64
	latencyMu         sync.Mutex
	latencyBuckets    map[float64]int64
}

var globalMetrics = newMetrics()

func newMetrics() *metrics {
	return &metrics{
		latencyBuckets: map[float64]int64{
			0.005: 0, 0.01: 0, 0.025: 0, 0.05: 0, 0.1: 0, 0.25: 0, 0.5: 0, 1.0: 0,
		},
	}
}

var latencyBucketsSorted = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0}

func (m *metrics) observeLatency(d time.Duration) {
	secs := d.Seconds()
	m.latencyMu.Lock()
	defer m.latencyMu.Unlock()
	for _, upper := range latencyBucketsSorted {
		if secs <= upper {
			m.latencyBuckets[upper]++
			return
		}
	}
}

// metricsHandler renders the Prometheus text exposition format.
func metricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	m := globalMetrics
	writeMetric(w, "onboarding_kyc_requests_total", m.requestsTotal.Load())
	writeMetric(w, "onboarding_kyc_applications_created_total", m.createAppTotal.Load())
	writeMetric(w, "onboarding_kyc_documents_uploaded_total", m.uploadDocTotal.Load())
	writeMetric(w, "onboarding_kyc_liveness_total", m.livenessTotal.Load())
	writeMetric(w, "onboarding_kyc_screening_total", m.screeningTotal.Load())
	writeMetric(w, "onboarding_kyc_screening_hits_total", m.screeningHits.Load())
	writeMetric(w, "onboarding_kyc_webhook_accept_total", m.webhookAccept.Load())
	writeMetric(w, "onboarding_kyc_webhook_reject_total", m.webhookReject.Load())
	writeMetric(w, "onboarding_kyc_webhook_duplicate_total", m.webhookDuplicate.Load())
	writeMetric(w, "onboarding_kyc_rekyc_total", m.rekycTotal.Load())
	writeMetric(w, "onboarding_kyc_vendor_calls_total", m.vendorCallsTotal.Load())
	writeMetric(w, "onboarding_kyc_state_transitions_total", m.transitionsTotal.Load())

	m.latencyMu.Lock()
	for _, upper := range latencyBucketsSorted {
		writeMetricLabel(w, "onboarding_kyc_request_latency_seconds_bucket", m.latencyBuckets[upper], "le", strconv.FormatFloat(upper, 'f', -1, 64))
	}
	m.latencyMu.Unlock()
}

func writeMetric(w http.ResponseWriter, name string, value int64) {
	_, _ = w.Write([]byte("# TYPE " + name + " counter\n"))
	_, _ = w.Write([]byte(name + " "))
	_, _ = w.Write([]byte(strconv.FormatInt(value, 10)))
	_, _ = w.Write([]byte("\n"))
}

func writeMetricLabel(w http.ResponseWriter, name string, value int64, label, labelVal string) {
	_, _ = w.Write([]byte(name + "{" + label + "=\"" + labelVal + "\"} "))
	_, _ = w.Write([]byte(strconv.FormatInt(value, 10)))
	_, _ = w.Write([]byte("\n"))
}