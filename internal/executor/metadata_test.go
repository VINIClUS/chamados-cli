package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

func TestMetadataFailurePreservesAppliedRevisionAndDeploymentID(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "devops")
	if err := os.MkdirAll(module, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(module)
	writeProtectedInfrastructure(t, root)
	for _, dir := range []string{".ops-executor", ".ops-target"} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	binary := ".ops-executor/chamadosctl"
	if err := os.WriteFile(binary, []byte("fixture-executable"), 0700); err != nil {
		t.Fatal(err)
	}
	sum, err := operations.Digest(binary)
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	identity := Identity{Protocol: 1, ExecutorSHA: sha, SHA256: sum, OS: "linux", Arch: "amd64"}
	if err = operations.WriteJSON(".ops-executor/manifest.json", identity); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(".ops-executor/SHA256SUMS", []byte(sum+"  chamadosctl\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(".ops-target/source.tar", []byte("source artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	sourceSum, err := operations.Digest(".ops-target/source.tar")
	if err != nil {
		t.Fatal(err)
	}
	if err = operations.WriteJSON(
		".ops-target/revision.json",
		Target{SchemaVersion: 1, Revision: sha, Ref: "main", SHA256: sourceSum},
	); err != nil {
		t.Fatal(err)
	}
	var posts, puts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			posts.Add(1)
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"id":17}`)
		case "PUT":
			puts.Add(1)
			w.WriteHeader(503)
		default:
			w.WriteHeader(200)
		}
	}))
	defer server.Close()
	for key, value := range map[string]string{"CI_COMMIT_BRANCH": "main", "CI_COMMIT_REF_PROTECTED": "true", "CI_COMMIT_SHA": sha, "CI_PIPELINE_ID": "12", "CI_JOB_ID": "34", "CI_PIPELINE_IID": "99", "CI_JOB_TOKEN": "ephemeral-job-secret", "CI_PROJECT_ID": "42", "CI_API_V4_URL": server.URL + "/api/v4", "DEPLOY_URL": server.URL, "DEPLOY_EDGE_IP": ""} {
		t.Setenv(key, value)
	}
	run := func(_ context.Context, c Command) error {
		for _, arg := range c.Args {
			if strings.Contains(arg, "ephemeral-job-secret") {
				t.Fatal("credential in SSH argv")
			}
		}
		command := c.Args[len(c.Args)-1]
		if strings.Contains(command, "tar -xf") {
			_, err := io.Copy(io.Discard, c.Stdin)
			return err
		}
		if strings.Contains(command, "inspect --binary") {
			return json.NewEncoder(c.Stdout).Encode(identity)
		}
		if strings.Contains(command, "execute --remote") {
			var env Envelope
			if err := json.NewDecoder(c.Stdin).Decode(&env); err != nil {
				return err
			}
			if env.Settings.Token != "ephemeral-job-secret" {
				t.Fatal("missing stdin credential")
			}
			return json.NewEncoder(c.Stdout).
				Encode(operations.Result{SchemaVersion: 1, Operation: "deploy", Environment: "staging", OperationID: "12-34", Status: "success", Phase: "complete", Revision: sha, Applied: true})
		}
		return nil
	}
	result, err := Run(
		context.Background(),
		operations.Request{Operation: "deploy", Environment: "staging", Ref: "main", Tail: 200},
		RunnerOptions{BuildSHA: sha, Run: run},
	)
	if err == nil || result.Status != "failed" || !result.Applied || result.DeploymentID != int64(17) ||
		result.Revision != sha ||
		result.Phase != "record_deployment" {
		t.Fatalf("lost applied operation: %+v %v", result, err)
	}
	if posts.Load() != 1 || puts.Load() != 3 {
		t.Fatalf("metadata attempts: POST=%d PUT=%d", posts.Load(), puts.Load())
	}
	if result.Error == nil || !result.Error.Retryable {
		t.Fatal("missing metadata retry information")
	}
}

func TestLostSSHRetainsObservedSafetyBackupFromRunningJournal(t *testing.T) {
	testUnknownSSHState(t, false)
}

func TestLostSSHRetainsReceivedTerminalResultWhenInspectionFails(t *testing.T) {
	testUnknownSSHState(t, true)
}

func testUnknownSSHState(t *testing.T, completed bool) {
	options, identity := sshFixture(t)
	env := Envelope{
		Protocol:    1,
		OperationID: "12-34",
		Request: operations.Request{
			Operation:   "restore",
			Environment: "staging",
			Ref:         "main",
			Tail:        200,
			Confirm:     "staging",
			BackupID:    "20261107120000.42",
		},
	}
	run := func(_ context.Context, c Command) error {
		command := c.Args[len(c.Args)-1]
		if strings.Contains(command, "tar -xf") {
			_, err := io.Copy(io.Discard, c.Stdin)
			return err
		}
		if strings.Contains(command, "inspect --binary") {
			return json.NewEncoder(c.Stdout).Encode(identity)
		}
		if strings.Contains(command, "execute --remote") {
			if completed {
				_ = json.NewEncoder(c.Stdout).
					Encode(operations.Result{SchemaVersion: 1, Operation: "restore", Environment: "staging", OperationID: "12-34", Status: "success", Revision: strings.Repeat("a", 40), Applied: true, Data: json.RawMessage(`{"safety_backup_id":"20261107120001.99","backend_stopped":false}`)})
			}
			return &ProcessError{Program: "ssh", Code: 255}
		}
		if strings.Contains(command, "internal inspect") {
			if completed {
				return &ProcessError{Program: "ssh", Code: 255}
			}
			return json.NewEncoder(c.Stdout).
				Encode(operations.Result{SchemaVersion: 1, Operation: "restore", Environment: "staging", OperationID: "12-34", Status: "running", Phase: "replace", Data: json.RawMessage(`{"safety_backup_id":"20261107120001.99","backend_stopped":true}`)})
		}
		return nil
	}
	result, err := executeSSH(
		context.Background(),
		env,
		identity,
		optionsWithRun(options, run),
	)
	if err == nil || result.Error == nil || result.Error.Code != "state_unknown" {
		t.Fatalf("unknown operation: %+v %v", result, err)
	}
	if completed && (!result.Applied || result.Revision != strings.Repeat("a", 40)) {
		t.Fatal("lost terminal operation state")
	}
	var data map[string]any
	if err = json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["safety_backup_id"] != "20261107120001.99" {
		t.Fatal("lost observed safety backup")
	}
}

func sshFixture(t *testing.T) (RunnerOptions, Identity) {
	t.Helper()

	root := t.TempDir()
	module := filepath.Join(root, "devops")
	if err := os.MkdirAll(module, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(module)
	writeProtectedInfrastructure(t, root)
	binary := filepath.Join(root, "chamadosctl")
	if err := os.WriteFile(binary, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	sum, err := operations.Digest(binary)
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{Protocol: 1, ExecutorSHA: strings.Repeat("a", 40), SHA256: sum, OS: "linux", Arch: "amd64"}
	manifest := filepath.Join(root, "manifest.json")
	if err = operations.WriteJSON(manifest, identity); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "SHA256SUMS"), []byte(sum+"  chamadosctl\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return RunnerOptions{Binary: binary, Manifest: manifest, Diagnostics: io.Discard}, identity
}

func optionsWithRun(options RunnerOptions, run func(context.Context, Command) error) RunnerOptions {
	options.Run = run
	return options
}

func TestRefusedReplayCannotBecomeOldSuccessDuringSSHRecovery(t *testing.T) {
	host, original, _ := hostFixture(t, "status")
	if _, err := host.Execute(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	options, identity := sshFixture(t)
	changed := original
	changed.Request.Ref = "feature/changed"
	inspections := 0
	options.Run = func(_ context.Context, command Command) error {
		remote := command.Args[len(command.Args)-1]
		switch {
		case strings.Contains(remote, "tar -xf"):
			_, err := io.Copy(io.Discard, command.Stdin)
			return err
		case strings.Contains(remote, "inspect --binary"):
			return json.NewEncoder(command.Stdout).Encode(identity)
		case strings.Contains(remote, "execute --remote"):
			var request Envelope
			if err := json.NewDecoder(command.Stdin).Decode(&request); err != nil {
				return err
			}
			result, operationErr := host.Execute(context.Background(), request)
			if err := json.NewEncoder(command.Stdout).Encode(result); err != nil {
				return err
			}
			if operationErr != nil {
				return &ProcessError{Program: "ssh", Code: 1}
			}
			return nil
		case strings.Contains(remote, "internal inspect"):
			inspections++
			result, err := host.Inspect(original.OperationID)
			if err != nil {
				return err
			}
			return json.NewEncoder(command.Stdout).Encode(result)
		default:
			return nil
		}
	}
	result, err := executeSSH(context.Background(), changed, identity, options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || result.SchemaVersion != 1 || inspections != 0 {
		t.Fatalf("rejected request became old success: %+v inspections=%d", result, inspections)
	}
	retained, err := host.Inspect(original.OperationID)
	if err != nil || retained.Status != "success" {
		t.Fatal("conflict overwrote the original journal")
	}
}
