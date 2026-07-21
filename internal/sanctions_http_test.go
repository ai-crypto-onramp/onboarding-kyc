package internal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPSanctionsClientScreen(t *testing.T) {
	var gotAuth, gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotName = r.URL.Query().Get("name")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hits": []map[string]any{
				{"list": "ofac", "matched_name": "BAD ACTOR", "score": 0.95},
			},
		})
	}))
	defer srv.Close()

	c := NewHTTPSanctionsClient(srv.URL, "tok", 0)
	hits, err := c.Screen(context.Background(), "John Bad Actor")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 1 || hits[0].List != "ofac" || hits[0].MatchedName != "BAD ACTOR" {
		t.Fatalf("hits: %+v", hits)
	}
	if hits[0].Score != 0.95 {
		t.Fatalf("score: %v", hits[0].Score)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth: %s", gotAuth)
	}
	if gotName != "John Bad Actor" {
		t.Fatalf("name: %s", gotName)
	}
}

func TestHTTPSanctionsClientScreenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"hits":[]}`)
	}))
	defer srv.Close()

	c := NewHTTPSanctionsClient(srv.URL, "", 0)
	hits, err := c.Screen(context.Background(), "Clean Name")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected no hits, got %+v", hits)
	}
}

func TestHTTPSanctionsClientScreenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewHTTPSanctionsClient(srv.URL, "", 0)
	_, err := c.Screen(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestHTTPSanctionsClientScreenBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `not-json`)
	}))
	defer srv.Close()

	c := NewHTTPSanctionsClient(srv.URL, "", 0)
	_, err := c.Screen(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error for bad json")
	}
}

func TestHTTPSanctionsClientContextCancelled(t *testing.T) {
	c := NewHTTPSanctionsClient("http://example.com", "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Screen(ctx, "x")
	if err == nil {
		t.Fatal("expected ctx error")
	}
}

func TestNewScreeningClientForModeDevDefault(t *testing.T) {
	t.Setenv("SANCTIONS_LIST_URL", "")
	c := NewScreeningClientForMode(true)
	if _, ok := c.(*InMemoryScreeningClient); !ok {
		t.Fatalf("expected *InMemoryScreeningClient, got %T", c)
	}
}

func TestNewScreeningClientForModeHTTP(t *testing.T) {
	t.Setenv("SANCTIONS_LIST_URL", "http://example.com")
	c := NewScreeningClientForMode(false)
	if _, ok := c.(*HTTPSanctionsClient); !ok {
		t.Fatalf("expected *HTTPSanctionsClient, got %T", c)
	}
}
