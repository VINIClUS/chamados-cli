package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

func TestJobCredentialNeverRedirectsToAnotherOrigin(t *testing.T) {
	leaked := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("JOB-TOKEN") != "" || r.Header.Get("PRIVATE-TOKEN") != ""
		w.WriteHeader(200)
	}))
	defer other.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("JOB-TOKEN") != "job-secret" || r.Header.Get("PRIVATE-TOKEN") != "" {
			t.Error("wrong credential")
		}
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer source.Close()
	c, err := NewJobClient(Settings{APIURL: source.URL + "/api/v4", ProjectID: "42", Token: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.JSON(context.Background(), "GET", "packages", nil, nil)
	if leaked {
		t.Fatal("credential crossed origin")
	}
}

func TestRegistryCommitsManifestLastAndRefusesReplacement(t *testing.T) {
	var puts []string
	committed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("JOB-TOKEN") != "job-secret" {
			t.Error("missing job credential")
		}
		if r.Method == "GET" {
			if committed {
				_, _ = w.Write([]byte(`{}`))
			} else {
				w.WriteHeader(404)
			}
			return
		}
		puts = append(puts, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "manifest.json") {
			committed = true
		}
		w.WriteHeader(201)
	}))
	defer server.Close()
	c, err := NewJobClient(Settings{APIURL: server.URL + "/api/v4", ProjectID: "42", Token: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, "backup.tar.gz")
	if err = os.WriteFile(archive, []byte("backup"), 0600); err != nil {
		t.Fatal(err)
	}
	sum, err := operations.Digest(archive)
	if err != nil {
		t.Fatal(err)
	}
	m := operations.Manifest{
		SchemaVersion: 1,
		Environment:   "staging",
		Revision:      strings.Repeat("a", 40),
		Ref:           "main",
		PostgresMajor: json.RawMessage("16"),
		CreatedAt:     "2026-11-07T12:00:00Z",
		SHA256:        sum,
	}
	if err = operations.WriteJSON(filepath.Join(dir, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
	if err = c.Publish(context.Background(), "staging", "20261107120000.42", dir); err != nil {
		t.Fatal(err)
	}
	if len(puts) != 2 || !strings.HasSuffix(puts[0], "backup.tar.gz") ||
		!strings.HasSuffix(puts[1], "manifest.json") {
		t.Fatalf("commit order: %v", puts)
	}
	if err = c.Publish(context.Background(), "staging", "20261107120000.42", dir); err == nil {
		t.Fatal("overwrote committed backup")
	}
	if len(puts) != 2 {
		t.Fatal("overwrite reached PUT")
	}
}

func TestUncertainDeploymentCreateIsNeverRetried(t *testing.T) {
	posts := 0
	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			posts++
			w.WriteHeader(503)
		}),
	)
	defer server.Close()
	c, err := NewJobClient(Settings{APIURL: server.URL + "/api/v4", ProjectID: "42", Token: "job-secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateDeployment(context.Background(), "staging", strings.Repeat("a", 40), "main")
	if err == nil || posts != 1 {
		t.Fatalf("create response: posts=%d err=%v", posts, err)
	}
}
