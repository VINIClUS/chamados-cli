package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

// resolveSource commits files to a fresh origin and resolves main from the devops
// directory of a clone, as ops:resolve does. Names in executable get the exec bit.
func resolveSource(t *testing.T, files map[string]string, executable ...string) map[string]*tar.Header {
	t.Helper()
	// A parent Git hook/checkout may export these variables. Fixtures must use
	// only the repository passed through -C, never inherited repository state.
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "unrelated.git"))
	t.Setenv("GIT_WORK_TREE", filepath.Join(t.TempDir(), "unrelated-worktree"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(t.TempDir(), "unrelated-common"))
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if err := os.MkdirAll(filepath.Join(origin, "devops"), 0700); err != nil {
		t.Fatal(err)
	}
	git := func(directory string, args ...string) {
		t.Helper()
		var diagnostic bytes.Buffer
		err := RunProcess(context.Background(), Command{
			Program: "git", Args: args, Dir: directory, Stdout: &diagnostic, Stderr: &diagnostic,
		})
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, diagnostic.String())
		}
	}
	git(origin, "init", "-q", "-b", "main")
	git(origin, "config", "user.name", "Local fixture")
	git(origin, "config", "user.email", "local@example.invalid")
	for name, content := range files {
		mode := os.FileMode(0600)
		for _, candidate := range executable {
			if candidate == name {
				mode = 0700
			}
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(origin, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(origin, name), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	git(origin, "add", ".")
	git(origin, "commit", "-q", "-m", "fixture")
	checkout := filepath.Join(root, "checkout")
	git(root, "clone", "-q", origin, checkout)
	if err := os.MkdirAll(filepath.Join(checkout, "devops"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(checkout, "devops"))
	_, err := Resolve(context.Background(), operations.Request{
		Operation: "deploy", Environment: "staging", Ref: "main", Tail: 200,
	}, nil, ".ops-target")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(".ops-target/source.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	headers := map[string]*tar.Header{}
	reader := tar.NewReader(f)
	for {
		header, readErr := reader.Next()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		headers[header.Name] = header
	}
	return headers
}

func TestResolveFromDevOpsDirectoryArchivesOnlyApplication(t *testing.T) {
	names := resolveSource(t, map[string]string{
		"application.txt": "application", "devops/tooling.txt": "must remain protected",
		"cicd/pipeline.yml":                                         "must remain protected",
		"infrastructure/application/docker-compose.yml":             "application recipe",
		"infrastructure/environments/docker-compose.production.yml": "protected overlay",
		"infrastructure/scripts/bootstrap-host.sh":                  "protected bootstrap",
	})
	if names["application.txt"] == nil || names["devops/tooling.txt"] != nil || names["cicd/pipeline.yml"] != nil {
		t.Fatalf("source is not application-only from repository root: %v", names)
	}
	if names["infrastructure/application/docker-compose.yml"] == nil ||
		names["infrastructure/environments/docker-compose.production.yml"] != nil ||
		names["infrastructure/scripts/bootstrap-host.sh"] != nil {
		t.Fatalf("application recipes and protected infrastructure were mixed: %v", names)
	}
}

// The backend suite checks that the contract export script exists and is executable,
// so the tested source keeps exactly that file from scripts/.
func TestResolveKeepsOnlyTheContractExportScriptFromScripts(t *testing.T) {
	names := resolveSource(t, map[string]string{
		"application.txt":                                           "application",
		"scripts/export-openapi.sh":                                 "#!/bin/sh\n",
		"scripts/export-openapi.sh.orig":                            "near miss",
		"scripts/deploy.sh":                                         "must remain protected",
		"scripts/deploy/docker-compose.production.yml":              "must remain protected",
		"scripts/export-*":                                          "as a glob it would exclude the script",
		"devops/scripts/tooling.sh":                                 "same relative name from devops/",
		"infrastructure/environments/docker-compose.production.yml": "protected overlay",
	}, "scripts/export-openapi.sh")
	script := names["scripts/export-openapi.sh"]
	if script == nil || script.Mode&0100 == 0 {
		t.Fatalf("contract export script missing or not executable: %v", script)
	}
	for name, header := range names {
		if strings.HasPrefix(name, "scripts/") && header.Typeflag != tar.TypeDir &&
			name != "scripts/export-openapi.sh" {
			t.Fatalf("%s reached the tested source", name)
		}
	}
	if names["application.txt"] == nil || names["devops/scripts/tooling.sh"] != nil ||
		names["infrastructure/environments/docker-compose.production.yml"] != nil {
		t.Fatalf("source policy changed beyond the export script: %v", names)
	}
}

// Revisions without the script, like the first releases, keep the previous archive.
func TestResolveWithoutTheExportScriptExcludesAllScripts(t *testing.T) {
	names := resolveSource(t, map[string]string{
		"application.txt":   "application",
		"scripts/deploy.sh": "must remain protected",
	})
	for name := range names {
		if name == "scripts" || strings.HasPrefix(name, "scripts/") {
			t.Fatalf("%s reached the tested source", name)
		}
	}
	if names["application.txt"] == nil {
		t.Fatalf("application missing: %v", names)
	}
}
