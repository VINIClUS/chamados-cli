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

func TestDeployBuildsStagedCodeButDumpsCurrentDatabaseBeforeInstall(t *testing.T) {
	host, envelope, _ := hostFixture(t, "deploy")
	incoming := t.TempDir()
	host.options.Incoming = incoming
	tooling := filepath.Join(incoming, "tooling/deploy")
	if err := os.MkdirAll(tooling, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(tooling, "docker-compose.staging.yml"),
		[]byte("services: {}\n"),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	envBefore := []byte("APP_INITIALIZER_ENABLED=false\nPRESERVED_SECRET=fixture-only\n")
	if err := os.WriteFile(filepath.Join(host.options.Dir, ".env"), envBefore, 0600); err != nil {
		t.Fatal(err)
	}
	var source bytes.Buffer
	archive := tar.NewWriter(&source)
	for _, name := range []string{"Dockerfile", "src/web/Dockerfile", "docker-compose.yml"} {
		content := []byte("fixture")
		if err := archive.WriteHeader(
			&tar.Header{Name: name, Mode: 0600, Size: int64(len(content))},
		); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(incoming, "source.tar")
	if err := os.WriteFile(file, source.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	sum, err := operations.Digest(file)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Target = Target{SchemaVersion: 1, Revision: strings.Repeat("b", 40), Ref: "main", SHA256: sum}
	var commands []string
	host.options.Run = func(_ context.Context, command Command) error {
		arguments := strings.Join(command.Args, " ")
		commands = append(commands, arguments)
		if command.Stdout == nil {
			return nil
		}
		value := ""
		if strings.Contains(arguments, "ps -q --status running") {
			value = "current-database\n"
		}
		if strings.Contains(arguments, "pg_dump") {
			value = "-- current database snapshot\n"
		}
		_, err := io.WriteString(command.Stdout, value)
		return err
	}
	result, err := host.Execute(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != strings.Repeat("b", 40) || !result.Applied {
		t.Fatalf("deployment result: %+v", result)
	}
	currentCompose := filepath.Join(host.options.Dir, "app/docker-compose.yml")
	backend, frontend, dump, up := -1, -1, -1, -1
	for index, command := range commands {
		switch {
		case strings.Contains(command, "build chamados-backend"):
			backend = index
			if strings.Contains(command, currentCompose) {
				t.Fatal("built current application instead of staged code")
			}
		case strings.Contains(command, "build chamados-frontend"):
			frontend = index
		case strings.Contains(command, "pg_dump"):
			dump = index
			if !strings.Contains(command, currentCompose) {
				t.Fatal("dump used staged database configuration")
			}
		case strings.Contains(command, "up -d --remove-orphans"):
			up = index
		}
	}
	if backend < 0 || frontend <= backend || dump <= frontend || up <= dump {
		t.Fatalf("wrong deployment order: %v", commands)
	}
	envAfter, err := os.ReadFile(filepath.Join(host.options.Dir, ".env"))
	if err != nil || !bytes.Equal(envBefore, envAfter) {
		t.Fatal("deployment changed existing environment")
	}
}
