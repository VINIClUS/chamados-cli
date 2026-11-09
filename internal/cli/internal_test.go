package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInternalIdentityIsHiddenAndNeedsNoOperatorCredentials(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("CI_JOB_TOKEN", "")
	var out bytes.Buffer
	cmd := NewCommand(
		Options{
			Out:        &out,
			Err:        &out,
			ConfigPath: filepath.Join(t.TempDir(), "missing"),
			Commit:     strings.Repeat("a", 40),
		},
	)
	cmd.SetArgs([]string{"internal", "inspect", "--binary"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["protocol"] != float64(1) || got["executor_sha"] != strings.Repeat("a", 40) || got["sha256"] == "" {
		t.Fatalf("invalid identity: %s", out.String())
	}
	out.Reset()
	cmd = NewCommand(Options{Out: &out, Err: &out})
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "internal ") {
		t.Fatal("internal commands exposed in help")
	}
}

func TestInternalRunnerRejectsUnprotectedContextAndWritesFailure(t *testing.T) {
	t.Setenv("CI_COMMIT_BRANCH", "feature")
	t.Setenv("CI_COMMIT_REF_PROTECTED", "false")
	result := filepath.Join(t.TempDir(), "result.json")
	var out bytes.Buffer
	cmd := NewCommand(Options{Out: &out, Err: &out})
	cmd.SetArgs([]string{"internal", "execute", "--result", result})
	if err := cmd.Execute(); err == nil {
		t.Fatal("unprotected execution accepted")
	}
	b, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "failed" {
		t.Fatalf("invalid failure: %s", b)
	}
}
