package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

func TestModernBuildKeepsCurrentLegacyComposeTarget(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "app")
	staged := filepath.Join(root, "staged")
	for _, name := range []string{
		"app/docker-compose.yml",
		"staged/infrastructure/application/docker-compose.yml",
		"staged/infrastructure/application/backend.Dockerfile",
		"staged/infrastructure/application/frontend.Dockerfile",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var commands []string
	envelope := Envelope{Request: operations.Request{Operation: "deploy", Environment: "staging"}}
	result := newHostResult(envelope)
	state := newExecution(HostOptions{
		Dir: root,
		Run: func(_ context.Context, command Command) error {
			commands = append(commands, strings.Join(command.Args, " "))
			return nil
		},
	}, envelope, &result, operationJournal{Directory: root})
	target := state.applicationTarget(current)
	if err := state.buildApplication(context.Background(), staged); err != nil {
		t.Fatal(err)
	}
	modern := filepath.Join(staged, "infrastructure/application/docker-compose.yml")
	if !strings.Contains(strings.Join(commands, "\n"), modern) {
		t.Fatalf("modern application was not built from its Compose: %v", commands)
	}
	if !strings.Contains(
		strings.Join(target.arguments("config"), " "),
		filepath.Join(current, "docker-compose.yml"),
	) {
		t.Fatal("staged layout changed the current deployment target")
	}
}
