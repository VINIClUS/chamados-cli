package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VINIClUS/chamados-cli/internal/cli"
)

func execute(t *testing.T, ctx context.Context, server *httptest.Server, args ...string) (string, string, error) {
	t.Helper()
	var out, progress bytes.Buffer
	root := cli.NewCommand(
		cli.Options{
			Out:          &out,
			Err:          &progress,
			ConfigPath:   filepath.Join(t.TempDir(), "config.json"),
			PollInterval: time.Millisecond,
		},
	)
	root.SetArgs(append([]string{"--gitlab-url", server.URL, "--token-file", tokenFile(t)}, args...))
	err := root.ExecuteContext(ctx)
	return out.String(), progress.String(), err
}

func TestOperationWaitsForFinalJobArtifact(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v4/projects/74155066/pipeline":
			fmt.Fprint(w, `{"id":42,"status":"pending","web_url":"https://gitlab.com/pipelines/42"}`)
		case "GET /api/v4/projects/74155066/pipelines/42":
			fmt.Fprint(w, `{"id":42,"status":"success"}`)
		case "GET /api/v4/projects/74155066/pipelines/42/jobs":
			fmt.Fprint(w, `[{"id":99,"name":"ops:run","status":"success"}]`)
		case "GET /api/v4/projects/74155066/jobs/99/artifacts/operation-result.json":
			fmt.Fprint(
				w,
				`{"schema_version":1,"operation":"health","environment":"staging","status":"success","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","data":{"healthy":true}}`,
			)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	out, _, err := execute(t, context.Background(), server, "health", "--json")
	if err != nil || !strings.Contains(out, `"healthy":true`) || strings.Contains(out, `"pending"`) {
		t.Fatalf("out=%s err=%v", out, err)
	}
}

func TestLegacyAndFailedOperationRevisionsRemainObservable(t *testing.T) {
	for _, tc := range []struct {
		operation, status, revision string
		wantError, invalid          bool
	}{
		{"status", "success", "c503801ac286", false, false},
		{"health", "success", "c503801ac286-dirty", false, false},
		{"health", "failed", "", true, false},
		{"deploy", "success", "c503801ac286", true, true},
	} {
		t.Run(tc.operation+tc.status+tc.revision, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/jobs"):
					fmt.Fprint(w, `[{"id":99,"name":"ops:run","status":"success"}]`)
				case strings.Contains(r.URL.Path, "/artifacts/"):
					json.NewEncoder(w).
						Encode(map[string]any{"schema_version": 1, "operation": tc.operation, "environment": "staging", "status": tc.status, "revision": tc.revision})
				default:
					fmt.Fprint(w, `{"id":42,"status":"success"}`)
				}
			}))
			defer server.Close()
			out, _, err := execute(t, context.Background(), server, "pipeline", "watch", "42")
			if (err != nil) != tc.wantError {
				t.Fatalf("out=%s err=%v", out, err)
			}
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "invalid operation result") {
					t.Fatalf("err=%v", err)
				}
			} else if !strings.Contains(out, `"revision":`) {
				t.Fatalf("result not observable: out=%q err=%v", out, err)
			}
		})
	}
}

func TestPipelineWatchCancelDoesNotCancelRemotePipeline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected mutation %s", r.Method)
		}
		fmt.Fprint(w, `{"id":42,"status":"running","web_url":"https://gitlab.com/pipelines/42"}`)
		cancel()
	}))
	defer server.Close()
	_, _, err := execute(t, ctx, server, "pipeline", "watch", "42")
	if err == nil || !strings.Contains(err.Error(), "42") || !strings.Contains(err.Error(), "continues") {
		t.Fatalf("err=%v", err)
	}
}

func TestGitLabErrorDoesNotLeakTokenOrRetryPost(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(500)
		fmt.Fprint(w, `{"message":"secret-token"}`)
	}))
	defer server.Close()
	out, progress, err := execute(t, context.Background(), server, "health", "--detach")
	if err == nil || requests != 1 || strings.Contains(out+progress+err.Error(), "secret-token") {
		t.Fatalf("requests=%d out=%q progress=%q err=%v", requests, out, progress, err)
	}
}

func TestProductionDeployRequiresProtectedAncestorTag(t *testing.T) {
	for _, tc := range []struct {
		name                string
		protected, ancestor bool
		wantPOST            int
	}{{"unprotected", false, true, 0}, {"diverged", true, false, 0}, {"valid", true, true, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/repository/tags/"):
					fmt.Fprintf(
						w,
						`{"name":"v1.2.3","protected":%t,"commit":{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
						tc.protected,
					)
				case strings.HasSuffix(r.URL.Path, "/repository/merge_base"):
					if r.URL.Query()["refs[]"][1] != "main" {
						t.Error("ancestry not checked against main")
					}
					sha := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
					if tc.ancestor {
						sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
					}
					fmt.Fprintf(w, `{"id":%q}`, sha)
				case r.Method == "POST":
					posts++
					fmt.Fprint(w, `{"id":42,"status":"pending"}`)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			_, _, err := execute(
				t,
				context.Background(),
				server,
				"deploy",
				"production",
				"--ref",
				"v1.2.3",
				"--detach",
			)
			if posts != tc.wantPOST || (tc.wantPOST == 0 && err == nil) ||
				(tc.wantPOST == 1 && err != nil) {
				t.Fatalf("posts=%d err=%v", posts, err)
			}
		})
	}
}

func TestPipelineOutputRedactsServerEchoedToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":42,"status":"pending","web_url":"https://gitlab.com/secret-token"}`)
	}))
	defer server.Close()
	out, progress, err := execute(t, context.Background(), server, "health", "--detach")
	if err != nil || strings.Contains(out+progress, "secret-token") {
		t.Fatalf("out=%q progress=%q err=%v", out, progress, err)
	}
}

func backupManifest(checksum string) string {
	return fmt.Sprintf(
		`{"schema_version":1,"environment":"staging","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ref":"main","postgres_major":"16","created_at":"2026-11-07T12:00:00Z","sha256":%q,"members":[{"name":"database.dump","sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size":10},{"name":"attachments.tar","sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","size":20}]}`,
		checksum,
	)
}

func TestBackupListOnlyIncludesPublishedCompleteManifests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/packages"):
			if r.URL.Query().Get("package_name") != "chamados-backup-staging" ||
				r.URL.Query().Get("package_type") != "generic" {
				t.Errorf("query=%v", r.URL.Query())
			}
			fmt.Fprint(
				w,
				`[{"id":1,"name":"chamados-backup-staging","version":"20261107120000.1"},{"id":2,"name":"chamados-backup-staging","version":"20261107120000.2"},{"id":3,"name":"chamados-backup-staging","version":"20261107120000.3"}]`,
			)
		case strings.Contains(r.URL.Path, "20261107120000.1"):
			fmt.Fprint(w, backupManifest(strings.Repeat("a", 64)))
		case strings.Contains(r.URL.Path, "20261107120000.2"):
			w.WriteHeader(404)
		case strings.Contains(r.URL.Path, "20261107120000.3"):
			fmt.Fprint(w, `{"schema_version":1}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	out, _, err := execute(t, context.Background(), server, "backup", "list", "--json")
	if err != nil || !strings.Contains(out, "20261107120000.1") || strings.Contains(out, "20261107120000.2") ||
		strings.Contains(out, "20261107120000.3") {
		t.Fatalf("out=%s err=%v", out, err)
	}
}

func TestBackupDownloadVerifiesChecksumBeforePublishingFile(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			checksum := fmt.Sprintf("%x", sha256.Sum256([]byte("archive")))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "manifest.json") {
					fmt.Fprint(w, backupManifest(checksum))
					return
				}
				if strings.HasSuffix(r.URL.Path, "backup.tar.gz") {
					if corrupt {
						fmt.Fprint(w, "corrupt")
					} else {
						fmt.Fprint(w, "archive")
					}
					return
				}
				t.Errorf("unexpected request %s", r.URL)
				w.WriteHeader(404)
			}))
			defer server.Close()
			dest := filepath.Join(t.TempDir(), "backup.tar.gz")
			_, _, err := execute(
				t,
				context.Background(),
				server,
				"backup",
				"download",
				"staging",
				"--backup",
				"20261107120000.1",
				"--output",
				dest,
			)
			data, readErr := os.ReadFile(dest)
			if corrupt {
				if err == nil || !os.IsNotExist(readErr) {
					t.Fatalf("corrupt download err=%v read=%v", err, readErr)
				}
			} else if err != nil || string(data) != "archive" {
				t.Fatalf("data=%q err=%v read=%v", data, err, readErr)
			}
		})
	}
}

func TestDownloadRedirectDoesNotForwardTokenToObjectStorage(t *testing.T) {
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte("archive")))
	object := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "" {
			t.Error("token forwarded to different origin")
		}
		fmt.Fprint(w, "archive")
	}))
	defer object.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "manifest.json") {
			fmt.Fprint(w, backupManifest(checksum))
			return
		}
		http.Redirect(w, r, object.URL+"/signed-archive", http.StatusFound)
	}))
	defer server.Close()
	_, _, err := execute(
		t,
		context.Background(),
		server,
		"backup",
		"download",
		"20261107120000.1",
		"--output",
		filepath.Join(t.TempDir(), "archive.tar.gz"),
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestCorruptBackupPreservesExistingDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "manifest.json") {
			fmt.Fprint(w, backupManifest(strings.Repeat("a", 64)))
		} else {
			fmt.Fprint(w, "corrupt")
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	dest := filepath.Join(dir, "backup.tar.gz")
	os.WriteFile(dest, []byte("existing"), 0600)
	_, _, err := execute(
		t,
		context.Background(),
		server,
		"backup",
		"download",
		"20261107120000.1",
		"--output",
		dest,
	)
	data, _ := os.ReadFile(dest)
	entries, _ := os.ReadDir(dir)
	if err == nil || string(data) != "existing" || len(entries) != 1 {
		t.Fatalf("data=%q files=%d err=%v", data, len(entries), err)
	}
}

func TestPipelineListAndLogsUseNativeAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/74155066/pipelines":
			if r.URL.Query().Get("ref") != "main" {
				t.Error("missing trusted ref filter")
			}
			fmt.Fprint(w, `[{"id":42,"status":"success","ref":"main"}]`)
		case "/api/v4/projects/74155066/pipelines/42/jobs":
			fmt.Fprint(w, `[{"id":99,"name":"ops:run","status":"success"}]`)
		case "/api/v4/projects/74155066/jobs/99/trace":
			fmt.Fprint(w, "operation complete secret-token\n")
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	out, _, err := execute(t, context.Background(), server, "pipeline", "list", "--json")
	if err != nil || !strings.Contains(out, `"id":42`) {
		t.Fatalf("out=%q err=%v", out, err)
	}
	out, _, err = execute(t, context.Background(), server, "pipeline", "logs", "42", "--json")
	if err != nil || !strings.Contains(out, "operation complete") || strings.Contains(out, "secret-token") ||
		!json.Valid([]byte(out)) {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestConfigPersistsDefaultsWithoutTokenAndFlagsOverride(t *testing.T) {
	var projects []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		projects = append(projects, r.URL.Path)
		fmt.Fprint(w, `{"id":123,"default_branch":"main"}`)
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "nested", "config.json")
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		cmd := cli.NewCommand(cli.Options{Out: &out, Err: &bytes.Buffer{}, ConfigPath: configPath})
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	for _, args := range [][]string{{"config", "set", "gitlab_url", server.URL}, {"config", "set", "project", "123"}} {
		if _, err := run(args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GITLAB_TOKEN", "secret-token")
	out, err := run("config", "show", "--json")
	if err != nil || strings.Contains(out, "secret-token") || !strings.Contains(out, `"operation_ref":"main"`) {
		t.Fatalf("out=%q err=%v", out, err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil || strings.Contains(string(raw), "token") {
		t.Fatalf("config=%q err=%v", raw, err)
	}
	if _, err := run("config", "set", "token", "secret-token"); err == nil {
		t.Fatal("configuration accepted token")
	}
	if _, err := run("doctor", "--project", "456", "--json"); err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0] != "/api/v4/projects/456" {
		t.Fatalf("projects=%v", projects)
	}
}

func TestVersionWorksWithoutCredentialsOrConfiguration(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	var out bytes.Buffer
	root := cli.NewCommand(
		cli.Options{
			Out:        &out,
			Err:        &bytes.Buffer{},
			ConfigPath: filepath.Join(t.TempDir(), "none"),
			Version:    "v0.1.0",
			Commit:     "abc123",
			Date:       "2026-11-07",
		},
	)
	root.SetArgs([]string{"version", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out.Bytes()) || !strings.Contains(out.String(), "v0.1.0") ||
		!strings.Contains(out.String(), "abc123") {
		t.Fatalf("out=%q", out.String())
	}
}

func TestFailedPipelineReturnsFinalResultAndNonzeroError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST":
			fmt.Fprint(w, `{"id":42,"status":"pending"}`)
		case strings.HasSuffix(r.URL.Path, "/pipelines/42"):
			fmt.Fprint(w, `{"id":42,"status":"failed"}`)
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			fmt.Fprint(w, `[{"id":99,"name":"ops:run","status":"failed"}]`)
		default:
			fmt.Fprint(
				w,
				`{"schema_version":1,"operation":"health","environment":"staging","status":"failed","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","data":{"reason":"service unavailable"}}`,
			)
		}
	}))
	defer server.Close()
	out, _, err := execute(t, context.Background(), server, "health", "--json")
	if err == nil || !strings.Contains(out, "service unavailable") || !json.Valid([]byte(out)) {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestWatchCancellationWhileFetchingJobsReportsPipeline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/jobs") {
			cancel()
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, `{"id":42,"status":"success"}`)
	}))
	defer server.Close()
	_, _, err := execute(t, ctx, server, "pipeline", "watch", "42")
	if err == nil || !strings.Contains(err.Error(), "42") || !strings.Contains(err.Error(), "continues") {
		t.Fatalf("err=%v", err)
	}
}

func TestMissingCredentialsDoesNotCallAPI(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer server.Close()
	var out bytes.Buffer
	root := cli.NewCommand(cli.Options{Out: &out, Err: &out, ConfigPath: filepath.Join(t.TempDir(), "config.json")})
	root.SetArgs([]string{"doctor", "--gitlab-url", server.URL})
	if err := root.Execute(); err == nil || requests != 0 {
		t.Fatalf("requests=%d err=%v", requests, err)
	}
}

func TestBackupListPropagatesRegistryErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/packages") {
			fmt.Fprint(w, `[{"name":"chamados-backup-staging","version":"20261107120000.1"}]`)
		} else {
			w.WriteHeader(403)
			fmt.Fprint(w, "secret-token")
		}
	}))
	defer server.Close()
	_, _, err := execute(t, context.Background(), server, "backup", "list")
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("err=%v", err)
	}
}

func TestConfigRejectsCredentialBearingURLsBeforeSaving(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	var out bytes.Buffer
	root := cli.NewCommand(cli.Options{Out: &out, Err: &out, ConfigPath: config})
	root.SetArgs([]string{"config", "set", "gitlab_url", "https://user:secret-token@gitlab.com"})
	err := root.Execute()
	_, statErr := os.Stat(config)
	if err == nil || !os.IsNotExist(statErr) || strings.Contains(out.String()+err.Error(), "secret-token") {
		t.Fatalf("out=%q err=%v stat=%v", out.String(), err, statErr)
	}
}

func TestMalformedOperationResultIsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST":
			fmt.Fprint(w, `{"id":42,"status":"pending"}`)
		case strings.HasSuffix(r.URL.Path, "/pipelines/42"):
			fmt.Fprint(w, `{"id":42,"status":"success"}`)
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			fmt.Fprint(w, `[{"id":99,"name":"ops:run","status":"success"}]`)
		default:
			fmt.Fprint(
				w,
				`{"schema_version":1,"operation":"health","environment":"staging","status":"success","revision":"main"}`,
			)
		}
	}))
	defer server.Close()
	out, _, err := execute(t, context.Background(), server, "health")
	if err == nil || out != "" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestConfigShowRejectsCredentialBearingURL(t *testing.T) {
	var out bytes.Buffer
	root := cli.NewCommand(cli.Options{Out: &out, Err: &out, ConfigPath: filepath.Join(t.TempDir(), "missing")})
	root.SetArgs([]string{"config", "show", "--gitlab-url", "https://user:secret-token@gitlab.com"})
	err := root.Execute()
	if err == nil || strings.Contains(out.String()+err.Error(), "secret-token") {
		t.Fatalf("out=%q err=%v", out.String(), err)
	}
}

func TestBackupListDoesNotHideManifestNetworkFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/packages") {
			fmt.Fprint(w, `[{"name":"chamados-backup-staging","version":"20261107120000.1"}]`)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer server.Close()
	_, _, err := execute(t, context.Background(), server, "backup", "list")
	if err == nil {
		t.Fatal("network failure silently returned an empty backup list")
	}
}

func tokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDeployDetachSendsTypedInputsToMain(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v4/projects/74155066/pipeline" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("PRIVATE-TOKEN") != "secret-token" {
			t.Error("missing authentication")
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":42,"status":"pending","web_url":"https://gitlab.com/pipelines/42"}`)
	}))
	defer server.Close()
	out, progress, err := execute(
		t,
		context.Background(),
		server,
		"deploy",
		"--ref",
		"feature/test",
		"--detach",
		"--json",
	)
	if err != nil {
		t.Fatal(err)
	}
	if received["ref"] != "main" {
		t.Fatalf("pipeline ref = %#v", received["ref"])
	}
	inputs, ok := received["inputs"].(map[string]any)
	if !ok || inputs["operation"] != "deploy" || inputs["environment"] != "staging" ||
		inputs["ref"] != "feature/test" ||
		inputs["tail"] != float64(200) ||
		inputs["service"] != "" ||
		inputs["confirm"] != "" ||
		inputs["backup_id"] != "" {
		t.Fatalf("inputs = %#v", inputs)
	}
	if !strings.Contains(out, `"id":42`) || strings.Contains(out, "Pipeline") || !strings.Contains(progress, "42") {
		t.Fatalf("out=%q progress=%q", out, progress)
	}
}

// A wrong command/input mapping or bypassed confirmation would break these public requests.
func TestOperationsEnforceEnvironmentAndConfirmation(t *testing.T) {
	for _, tc := range []struct {
		args           []string
		operation, env string
		invalid        bool
	}{
		{[]string{"status", "production", "--service", "chamados-backend"}, "status", "production", false},
		{[]string{"logs", "--env", "staging", "--tail", "50"}, "logs", "staging", false},
		{[]string{"health"}, "health", "staging", false},
		{[]string{"smoke"}, "smoke", "staging", false},
		{[]string{"restart"}, "restart", "staging", false},
		{[]string{"backup", "create"}, "backup", "staging", false},
		{[]string{"reset", "--confirm", "staging"}, "reset", "staging", false},
		{[]string{"restore", "20261107120000.7", "--confirm", "staging"}, "restore", "staging", false},
		{[]string{"restore", "staging", "--backup", "20261107120000.7", "--confirm", "staging"}, "restore", "staging", false},
		{[]string{"reset"}, "", "", true},
		{[]string{"reset", "--env", "production", "--confirm", "production"}, "", "", true},
		{[]string{"restore", "20261107120000.7"}, "", "", true},
		{[]string{"deploy", "--env", "production", "--ref", "main"}, "", "", true},
		{[]string{"smoke", "--env", "production"}, "", "", true},
		{[]string{"logs", "--tail", "0"}, "", "", true},
		{[]string{"restart", "--service", "other"}, "", "", true},
		{[]string{"health", "--env", "unknown"}, "", "", true},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var payload struct {
					Inputs map[string]any `json:"inputs"`
				}
				json.NewDecoder(r.Body).Decode(&payload)
				if payload.Inputs["operation"] != tc.operation ||
					payload.Inputs["environment"] != tc.env {
					t.Errorf("payload=%#v", payload)
				}
				if tc.operation == "restore" && payload.Inputs["backup_id"] != "20261107120000.7" {
					t.Errorf("backup=%v", payload.Inputs["backup_id"])
				}
				fmt.Fprint(w, `{"id":42,"status":"pending"}`)
			}))
			defer server.Close()
			_, _, err := execute(t, context.Background(), server, append(tc.args, "--detach")...)
			if tc.invalid {
				if err == nil || requests != 0 {
					t.Fatalf("invalid operation: requests=%d err=%v", requests, err)
				}
			} else if err != nil || requests != 1 {
				t.Fatalf("requests=%d err=%v", requests, err)
			}
		})
	}
}
