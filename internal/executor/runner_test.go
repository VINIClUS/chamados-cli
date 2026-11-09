package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

func TestRunnerNeverConnectsWithUntrustedExecutorArtifact(t *testing.T) {
	t.Setenv("CI_COMMIT_SHA", strings.Repeat("a", 40))
	t.Setenv("CI_COMMIT_BRANCH", "main")
	t.Setenv("CI_COMMIT_REF_PROTECTED", "true")
	t.Setenv("CI_PIPELINE_ID", "12")
	t.Setenv("CI_JOB_ID", "34")
	t.Setenv("CI_JOB_TOKEN", "job-secret")
	t.Setenv("CI_API_V4_URL", "http://localhost/api/v4")
	t.Setenv("CI_PROJECT_ID", "42")
	dir := t.TempDir()
	binary := filepath.Join(dir, "chamadosctl")
	if err := os.WriteFile(binary, []byte("executable"), 0700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "manifest.json")
	m := Identity{
		Protocol:    1,
		ExecutorSHA: strings.Repeat("a", 40),
		OS:          "linux",
		Arch:        "amd64",
		SHA256:      strings.Repeat("0", 64),
	}
	if err := operations.WriteJSON(manifest, m); err != nil {
		t.Fatal(err)
	}
	called := false
	result, err := Run(
		context.Background(),
		operations.Request{Operation: "status", Environment: "staging", Ref: "main", Tail: 200},
		RunnerOptions{
			Binary:     binary,
			Manifest:   manifest,
			BuildSHA:   strings.Repeat("a", 40),
			ResultFile: filepath.Join(dir, "result.json"),
			Run: func(context.Context, Command) error {
				called = true
				return nil
			},
		},
	)
	if err == nil || called || result.Status != "failed" {
		t.Fatalf("untrusted executor reached SSH: %+v %v", result, err)
	}
}

func TestEnvelopeSecretIsAbsentFromRequestJournal(t *testing.T) {
	h, env, _ := hostFixture(t, "status")
	env.Settings.Token = "job-secret"
	if _, err := h.Execute(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile((operationJournal{Directory: h.options.Dir}).Path(env.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "job-secret") || strings.Contains(string(b), "job_token") {
		t.Fatal("credential persisted")
	}
	var record map[string]any
	if err = json.Unmarshal(b, &record); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerRejectsExecutorFromDifferentPipelineRevision(t *testing.T) {
	t.Setenv("CI_COMMIT_BRANCH", "main")
	t.Setenv("CI_COMMIT_REF_PROTECTED", "true")
	t.Setenv("CI_PIPELINE_ID", "12")
	t.Setenv("CI_JOB_ID", "34")
	t.Setenv("CI_COMMIT_SHA", strings.Repeat("b", 40))
	t.Setenv("CI_JOB_TOKEN", "job-secret")
	t.Setenv("CI_API_V4_URL", "http://localhost/api/v4")
	t.Setenv("CI_PROJECT_ID", "42")
	dir := t.TempDir()
	binary := filepath.Join(dir, "chamadosctl")
	if err := os.WriteFile(binary, []byte("executable"), 0700); err != nil {
		t.Fatal(err)
	}
	sum, err := operations.Digest(binary)
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "manifest.json")
	m := Identity{Protocol: 1, ExecutorSHA: strings.Repeat("a", 40), OS: "linux", Arch: "amd64", SHA256: sum}
	if err = operations.WriteJSON(manifest, m); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = Run(
		context.Background(),
		operations.Request{Operation: "status", Environment: "staging", Ref: "main", Tail: 200},
		RunnerOptions{
			Binary:     binary,
			Manifest:   manifest,
			BuildSHA:   strings.Repeat("a", 40),
			ResultFile: filepath.Join(dir, "result.json"),
			Run: func(context.Context, Command) error {
				called = true
				return nil
			},
		},
	)
	if err == nil || called {
		t.Fatal("pipeline accepted executor built from another revision")
	}
}

func TestRunnerSeparatesExecutorAndBackendRevisions(t *testing.T) {
	t.Setenv("CI_COMMIT_BRANCH", "main")
	t.Setenv("CI_COMMIT_REF_PROTECTED", "true")
	t.Setenv("CI_COMMIT_SHA", strings.Repeat("b", 40))
	t.Setenv("OPS_EXECUTOR_SHA", strings.Repeat("a", 40))
	request := operations.Request{Operation: "status", Environment: "staging", Ref: "main", Tail: 200}
	if err := validateRunner(request, "12-34", strings.Repeat("a", 40), strings.Repeat("b", 40)); err != nil {
		t.Fatalf("separate executor/backend revisions rejected: %v", err)
	}
	if err := validateRunner(request, "12-34", strings.Repeat("b", 40), strings.Repeat("b", 40)); err == nil {
		t.Fatal("backend revision was accepted as executor revision")
	}
	if err := validateRunner(request, "12-34", strings.Repeat("a", 40), strings.Repeat("c", 40)); err == nil {
		t.Fatal("backend revision from a different protected checkout was accepted")
	}
}
