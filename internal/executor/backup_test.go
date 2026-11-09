package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

type backupFixture struct {
	host         *Host
	envelope     Envelope
	store        map[string][]byte
	calls        []string
	rejectUpload bool
}

func newBackupFixture(t *testing.T) *backupFixture {
	t.Helper()
	f := &backupFixture{store: map[string][]byte{}}
	dir := t.TempDir()
	for _, name := range []string{"app", "tooling/deploy"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{"app/REVISION": strings.Repeat("a", 40), "app/REF": "main", ".env": "PRESERVED_SECRET=never-archive\nAPP_INITIALIZER_ENABLED=false\n", "tooling/deploy/docker-compose.staging.yml": "services: {}\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/repository/commits/") {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": strings.Repeat("a", 40)})
			return
		}
		if r.URL.Path == "/health" {
			_, _ = io.WriteString(w, "ok")
			return
		}
		if r.Method == "PUT" {
			if f.rejectUpload {
				w.WriteHeader(500)
				return
			}
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			f.store[r.URL.RequestURI()] = b
			w.WriteHeader(201)
			return
		}
		if b, ok := f.store[r.URL.RequestURI()]; ok {
			_, _ = w.Write(b)
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(server.Close)
	var attachment bytes.Buffer
	tw := tar.NewWriter(&attachment)
	content := []byte("attachment-snapshot")
	if err := tw.WriteHeader(
		&tar.Header{Name: "nested/file.bin", Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(content))},
	); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(content)
	_ = tw.Close()
	f.host = NewHost(
		HostOptions{
			Dir:         dir,
			BackendURL:  server.URL + "/health",
			FrontendURL: server.URL + "/health",
			Run: func(_ context.Context, c Command) error {
				args := strings.Join(c.Args, " ")
				f.calls = append(f.calls, args)
				var output []byte
				switch {
				case strings.Contains(args, "--format=custom"):
					output = []byte("PGDMP\x01snapshot")
				case strings.Contains(args, "--entrypoint tar") && strings.Contains(args, "-cf -"):
					output = attachment.Bytes()
				case strings.Contains(args, "SHOW server_version_num"):
					output = []byte("160006\n")
				case strings.HasPrefix(args, "inspect -f"):
					output = []byte("sha256:fixture\n")
				case strings.Contains(args, "ps -q --status running"):
					output = []byte("container\n")
				}
				if c.Stdout != nil {
					_, err := c.Stdout.Write(output)
					return err
				}
				return nil
			},
		},
	)
	f.envelope = Envelope{
		Protocol:    1,
		OperationID: "12-34",
		Deadline:    time.Now().Add(time.Minute),
		Request:     operations.Request{Operation: "backup", Environment: "staging", Ref: "main", Tail: 200},
		Settings: Settings{
			APIURL:      server.URL + "/api/v4",
			ProjectID:   "42",
			PipelineIID: "99",
			ProjectPath: "group/project",
			Token:       "ephemeral-job-secret",
		},
	}
	var source bytes.Buffer
	gz := gzip.NewWriter(&source)
	archive := tar.NewWriter(gz)
	for _, name := range []string{"Dockerfile", "src/web/Dockerfile", "docker-compose.yml"} {
		b := []byte("fixture")
		if err := archive.WriteHeader(
			&tar.Header{
				Name:     "project-sha/" + name,
				Typeflag: tar.TypeReg,
				Size:     int64(len(b)),
				Mode:     0600,
			},
		); err != nil {
			t.Fatal(err)
		}
		_, _ = archive.Write(b)
	}
	_ = archive.Close()
	_ = gz.Close()
	f.store["/api/v4/projects/42/repository/archive.tar.gz?sha="+strings.Repeat("a", 40)] = source.Bytes()
	return f
}

func (f *backupFixture) restore(id string) {
	f.envelope.OperationID = "12-35"
	f.envelope.Request.Operation = "restore"
	f.envelope.Request.BackupID = id
	f.envelope.Request.Confirm = "staging"
	f.envelope.Settings.PipelineIID = "100"
	f.calls = nil
}

func TestBackupUploadFailureResumesPreviouslyRunningBackend(t *testing.T) {
	f := newBackupFixture(t)
	f.rejectUpload = true
	result, err := f.host.Execute(context.Background(), f.envelope)
	if err == nil || result.Status != "failed" {
		t.Fatalf("upload failure: %+v %v", result, err)
	}
	var data map[string]any
	if err = json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["backend_stopped"] != false ||
		!strings.Contains(strings.Join(f.calls, "\n"), "start chamados-backend") {
		t.Fatalf("backend not resumed: %s %v", result.Data, f.calls)
	}
	for name := range f.store {
		if strings.HasSuffix(name, "manifest.json") {
			t.Fatal("failed upload committed manifest")
		}
	}
}

func TestCorruptRestoreNeverStopsBackendOrReplacesData(t *testing.T) {
	f := newBackupFixture(t)
	created, err := f.host.Execute(context.Background(), f.envelope)
	if err != nil {
		t.Fatal(err)
	}
	key := "/api/v4/projects/42/packages/generic/chamados-backup-staging/" + created.BackupID + "/backup.tar.gz"
	f.store[key] = append(f.store[key], []byte("corruption")...)
	f.restore(created.BackupID)
	result, err := f.host.Execute(context.Background(), f.envelope)
	if err == nil {
		t.Fatal("accepted corruption")
	}
	calls := strings.Join(f.calls, "\n")
	if strings.Contains(calls, "stop chamados-backend") || strings.Contains(calls, "dropdb") {
		t.Fatalf("corruption reached mutating command: %v", f.calls)
	}
	if result.Applied {
		t.Fatal("corrupt restore reported applied")
	}
}

func TestRestorePublishesSafetyBeforeReplacingDataAndPreservesEnv(t *testing.T) {
	f := newBackupFixture(t)
	before, err := os.ReadFile(filepath.Join(f.host.options.Dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := f.host.Execute(context.Background(), f.envelope)
	if err != nil {
		t.Fatal(err)
	}
	f.restore(created.BackupID)
	original := f.host.options.Run
	f.host.options.Run = func(ctx context.Context, c Command) error {
		if strings.Contains(strings.Join(c.Args, " "), "dropdb") {
			marker := "/api/v4/projects/42/packages/generic/chamados-backup-staging/"
			found := false
			for path := range f.store {
				if strings.HasPrefix(path, marker) && strings.HasSuffix(path, ".100/manifest.json") {
					found = true
				}
			}
			if !found {
				t.Fatal("destructive restore preceded safety manifest")
			}
		}
		return original(ctx, c)
	}
	result, err := f.host.Execute(context.Background(), f.envelope)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "success" || !result.Applied {
		t.Fatalf("restore result: %+v", result)
	}
	after, err := os.ReadFile(filepath.Join(f.host.options.Dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("restore changed .env")
	}
	var data map[string]any
	if err = json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["safety_backup_id"] == nil || data["backend_stopped"] != false {
		t.Fatalf("recovery metadata: %s", result.Data)
	}
}

func TestRestoreHealthFailureKeepsBackendStoppedAndSafetyID(t *testing.T) {
	f := newBackupFixture(t)
	created, err := f.host.Execute(context.Background(), f.envelope)
	if err != nil {
		t.Fatal(err)
	}
	f.restore(created.BackupID)
	f.host.options.FrontendURL = "http://127.0.0.1:1/"
	f.host.options.HealthTimeout = 5 * time.Millisecond
	result, err := f.host.Execute(context.Background(), f.envelope)
	if err == nil {
		t.Fatal("health failure accepted")
	}
	var data map[string]any
	if err = json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !result.Applied || data["backend_stopped"] != true || data["safety_backup_id"] == nil {
		t.Fatalf("lost safety state: %+v %s", result, result.Data)
	}
	if strings.Contains(strings.Join(f.calls, "\n"), "start chamados-backend") {
		t.Fatal("failed restore reopened backend")
	}
	if strings.Contains(strings.Join(f.calls, "\n"), "stop chamados-postgresql") {
		t.Fatal("health timeout stopped PostgreSQL while operation context was still active")
	}
}
