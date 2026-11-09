package executor

import (
	"context"
	"encoding/json"
	"errors"
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

func hostFixture(t *testing.T, op string) (*Host, Envelope, *[]string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app/REVISION"), []byte(strings.Repeat("a", 40)), 0600); err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "ok")
		}),
	)
	t.Cleanup(server.Close)
	h := NewHost(
		HostOptions{
			Dir:         dir,
			BackendURL:  server.URL,
			FrontendURL: server.URL,
			Run: func(_ context.Context, c Command) error {
				calls = append(calls, strings.Join(c.Args, " "))
				if c.Stdout != nil {
					_, _ = io.WriteString(c.Stdout, "ready\n")
				}
				return nil
			},
		},
	)
	env := Envelope{
		Protocol:    1,
		OperationID: "12-34",
		Deadline:    time.Now().Add(time.Minute),
		Request:     operations.Request{Operation: op, Environment: "staging", Ref: "main", Tail: 200},
	}
	return h, env, &calls
}

func TestHostRestartExcludesDatabaseAndJournalsTerminalResult(t *testing.T) {
	h, env, calls := hostFixture(t, "restart")
	result, err := h.Execute(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "success" ||
		!strings.Contains(strings.Join(*calls, "\n"), "restart chamados-backend chamados-frontend") {
		t.Fatalf("restart: %+v %v", result, *calls)
	}
	got, err := h.Inspect(env.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "success" || got.OperationID != "12-34" {
		t.Fatalf("journal: %+v", got)
	}
	before := len(*calls)
	if _, err = h.Execute(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != before {
		t.Fatal("replayed terminal operation")
	}
}

func TestHostFailureKeepsStructuredErrorAndDoesNotClaimApplied(t *testing.T) {
	h, env, _ := hostFixture(t, "restart")
	h.options.Run = func(context.Context, Command) error {
		return &ProcessError{Program: "docker", Code: 9}
	}
	result, err := h.Execute(context.Background(), env)
	if err == nil || result.Status != "failed" || result.Applied || result.Error == nil ||
		result.Error.ExitCode != 9 {
		t.Fatalf("failure: %+v %v", result, err)
	}
	var got map[string]any
	_ = json.Unmarshal(result.Data, &got)
	if strings.Contains(string(result.Data), "job-secret") {
		t.Fatal("secret in journal")
	}
}

func TestHostRejectsExpiredDeadlineBeforeDocker(t *testing.T) {
	h, env, calls := hostFixture(t, "restart")
	env.Deadline = time.Now().Add(-time.Second)
	_, err := h.Execute(context.Background(), env)
	if err == nil || len(*calls) != 0 {
		t.Fatalf("expired execution: %v %v", err, *calls)
	}
}

func TestHostCannotRepeatUnfinishedOrDifferentRequest(t *testing.T) {
	h, env, calls := hostFixture(t, "restart")
	if err := (operationJournal{Directory: h.options.Dir}).Write(
		env,
		operations.Result{SchemaVersion: 1, Status: "running", Phase: "replace", OperationID: env.OperationID},
	); err != nil {
		t.Fatal(err)
	}
	result, err := h.Execute(context.Background(), env)
	if err == nil || result.Error == nil || result.Error.Code != "state_unknown" || len(*calls) != 0 {
		t.Fatalf("unfinished replay: %+v %v", result, err)
	}
	env.Request.Operation = "reset"
	env.Request.Confirm = "staging"
	if _, err = h.Execute(context.Background(), env); err == nil {
		t.Fatal("reused ID for another operation")
	}
}

func TestHostRejectsUnsafeOperationBeforeLockOrJournal(t *testing.T) {
	h, env, calls := hostFixture(t, "reset")
	env.Request.Environment = "production"
	env.Request.Confirm = "production"
	_, err := h.Execute(context.Background(), env)
	if err == nil || len(*calls) != 0 {
		t.Fatal("unsafe operation reached host")
	}
	if _, err = os.Stat(filepath.Join(h.options.Dir, ".deploy.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid request took lock: %v", err)
	}
}

func TestUnreadableJournalKeepsRequestedResultIdentity(t *testing.T) {
	host, envelope, _ := hostFixture(t, "status")
	journal := operationJournal{Directory: host.options.Dir}
	if err := os.MkdirAll(filepath.Dir(journal.Path(envelope.OperationID)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal.Path(envelope.OperationID), []byte("invalid JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := host.Execute(context.Background(), envelope)
	if err == nil {
		t.Fatal("accepted unreadable journal")
	}
	if result.SchemaVersion != 1 || result.Operation != "status" || result.OperationID != envelope.OperationID {
		t.Fatalf("lost request identity on journal failure: %+v", result)
	}
}

func TestDeployRejectsCorruptArtifactBeforeDockerOrEnv(t *testing.T) {
	h, env, calls := hostFixture(t, "deploy")
	h.options.Incoming = t.TempDir()
	if err := os.WriteFile(filepath.Join(h.options.Incoming, "source.tar"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	env.Target = Target{
		SchemaVersion: 1,
		Revision:      strings.Repeat("a", 40),
		Ref:           "main",
		SHA256:        strings.Repeat("0", 64),
	}
	if _, err := h.Execute(context.Background(), env); err == nil {
		t.Fatal("accepted corrupt source")
	}
	if len(*calls) != 0 {
		t.Fatalf("invalid source reached commands: %v", *calls)
	}
	if _, err := os.Stat(filepath.Join(h.options.Dir, ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid source generated configuration")
	}
}

func TestJournalCanInspectFiniteLargeLogs(t *testing.T) {
	h, env, _ := hostFixture(t, "logs")
	output := strings.Repeat("x", (1<<20)+10) + "\n"
	h.options.Run = func(_ context.Context, c Command) error {
		_, err := io.WriteString(c.Stdout, output)
		return err
	}
	if _, err := h.Execute(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	result, err := h.Inspect(env.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err = json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["output"] != output {
		t.Fatal("journal lost bounded command output")
	}
}

func TestAuthenticatedSmokePreservesCheckMessages(t *testing.T) {
	h, env, _ := hostFixture(t, "smoke")
	if err := os.WriteFile(
		filepath.Join(h.options.Dir, ".env"),
		[]byte(
			"APP_INITIALIZER_ADMIN_EMAIL=fixture@example.invalid\nAPP_INITIALIZER_ADMIN_PASSWORD=fixture-only\nCOOKIE_SECURE=true\n",
		),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/auth/csrf":
			http.SetCookie(w, &http.Cookie{Name: "XSRF-TOKEN", Value: "csrf", Path: "/", Secure: true})
			_, _ = io.WriteString(w, `{"token":"csrf"}`)
		case "/api/auth/login":
			cookie, err := r.Cookie("XSRF-TOKEN")
			if err != nil || cookie.Value != "csrf" || r.Header.Get("X-XSRF-TOKEN") != "csrf" {
				w.WriteHeader(403)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "access_token", Value: "fixture", Path: "/", Secure: true})
			_, _ = io.WriteString(w, `{}`)
		default:
			cookie, err := r.Cookie("access_token")
			if err != nil || cookie.Value != "fixture" {
				w.WriteHeader(401)
				return
			}
			switch r.URL.Path {
			case "/api/auth/me":
				_, _ = io.WriteString(w, `{"secretaries":[{"id":1}]}`)
			case "/api/dashboard/calls":
				_, _ = io.WriteString(w, `{"totalActive":0,"urgent":0,"recentCalls":[]}`)
			case "/api/calls/filter":
				_, _ = io.WriteString(w, `{"content":[],"page":{"number":0,"totalElements":0}}`)
			default:
				w.WriteHeader(404)
			}
		}
	}))
	defer server.Close()
	h.options.BackendURL = server.URL + "/api/auth/csrf"
	result, err := h.Execute(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err = json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["output"] != "Dashboard de chamados: OK\nConsulta combinada de chamados: OK\n" {
		t.Fatalf("lost smoke messages: %s", result.Data)
	}
}
