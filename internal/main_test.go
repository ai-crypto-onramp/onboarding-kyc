package internal

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	healthzHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json, got %q", ct)
	}

	body := strings.TrimSpace(rec.Body.String())
	want := `{"status":"ok"}`
	if body != want {
		t.Fatalf("expected body %q, got %q", want, body)
	}
}
func TestNewMuxRoutes(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	tests := []struct {
		name     string
		path     string
		wantCode int
		wantBody string
	}{
		{name: "healthz ok", path: "/healthz", wantCode: http.StatusOK, wantBody: `{"status":"ok"}`},
		{name: "unknown path", path: "/nope", wantCode: http.StatusNotFound, wantBody: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tt.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantCode {
				t.Fatalf("expected status %d, got %d", tt.wantCode, resp.StatusCode)
			}

			if tt.wantBody != "" {
				b, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				if got := strings.TrimSpace(string(b)); got != tt.wantBody {
					t.Fatalf("expected body %q, got %q", tt.wantBody, got)
				}
			}
		})
	}
}

func TestRunReturnsErrorWhenAddrInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	if err := Run(ln.Addr().String()); err == nil {
		t.Fatal("expected error when address is already in use, got nil")
	}
}

func TestRunReturnsErrorOnInvalidAddr(t *testing.T) {
	if err := Run("not-a-valid-addr:xyz"); err == nil {
		t.Fatal("expected error for invalid address, got nil")
	}
}
