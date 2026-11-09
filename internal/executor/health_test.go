package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPublicHealthChecksFrontendAndBackend(t *testing.T) {
	paths := map[string]bool{}
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				paths[r.URL.Path] = true
				w.WriteHeader(200)
			},
		),
	)
	defer server.Close()
	t.Setenv("DEPLOY_URL", server.URL)
	t.Setenv("DEPLOY_EDGE_IP", "")
	if err := checkPublic(context.Background(), "staging"); err != nil {
		t.Fatal(err)
	}
	if !paths["/"] || !paths["/api/auth/csrf"] {
		t.Fatalf("missing public check: %v", paths)
	}
}

func TestPublicBackendFailureCannotReportHealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/csrf" {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(200)
		}
	}))
	defer server.Close()
	t.Setenv("DEPLOY_URL", server.URL)
	t.Setenv("DEPLOY_EDGE_IP", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := checkPublic(ctx, "staging"); err == nil {
		t.Fatal("accepted failed public backend")
	}
}
