package operations_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VINIClUS/chamados-cli/internal/executor"
	"github.com/VINIClUS/chamados-cli/internal/operations"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func backendSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "contracts", "backend", name))
	if err != nil {
		t.Fatal(err)
	}
	schema, err := jsonschema.NewCompiler().Compile(path)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func wireValue(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestRequestsMatchThePinnedBackendContract(t *testing.T) {
	schema := backendSchema(t, "operations-input.schema.json")
	for _, operation := range []string{"deploy", "status", "logs", "health", "smoke", "restart", "reset", "backup", "restore", "cleanup"} {
		t.Run(operation, func(t *testing.T) {
			request := operations.Request{Operation: operation, Environment: "staging", Ref: "main", Tail: 200,
				Confirm: "staging", BackupID: "20261107120000.1"}
			if err := request.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(wireValue(t, request)); err != nil {
				t.Fatal(err)
			}
		})
	}
	invalid := operations.Request{Operation: "status", Environment: "staging", Ref: "main", Tail: 0}
	if invalid.Validate() == nil || schema.Validate(wireValue(t, invalid)) == nil {
		t.Fatal("invalid log limit must fail both runtime and Backend contract validation")
	}
}

func TestResultsKeepApplicationAndExecutorProvenanceSeparate(t *testing.T) {
	result := operations.Result{SchemaVersion: 1, Operation: "status", Environment: "staging", Status: "success",
		Revision: strings.Repeat("b", 40), ExecutorSHA: strings.Repeat("a", 40), BackendSHA: strings.Repeat("c", 40)}
	schema := backendSchema(t, "operations-result.schema.json")
	if err := schema.Validate(wireValue(t, result)); err != nil {
		t.Fatal(err)
	}
	result.Revision = strings.Repeat("b", 12) // The existing automatic deployment writes a short revision.
	if err := schema.Validate(wireValue(t, result)); err != nil {
		t.Fatal(err)
	}
	result.ExecutorSHA = "main"
	if schema.Validate(wireValue(t, result)) == nil {
		t.Fatal("executor provenance requires a full SHA")
	}
}

func TestActualBinaryManifestMatchesTheBackendContract(t *testing.T) {
	identity, err := executor.BinaryIdentity(strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	if err := backendSchema(t, "executor-manifest.schema.json").Validate(wireValue(t, identity)); err != nil {
		t.Fatal(err)
	}
}

func TestBackupManifestMatchesTheBackendContract(t *testing.T) {
	manifest := operations.Manifest{SchemaVersion: 1, Environment: "staging", Revision: strings.Repeat("b", 40),
		Ref: "main", PostgresMajor: json.RawMessage(`"16"`), CreatedAt: "2030-01-01T00:00:00Z", SHA256: strings.Repeat("c", 64),
		Members: []operations.Member{{Name: "database.dump", SHA256: strings.Repeat("d", 64), Size: 5},
			{Name: "attachments.tar", SHA256: strings.Repeat("e", 64), Size: 6}}}
	schema := backendSchema(t, "backup-manifest.schema.json")
	if !manifest.Valid("staging") {
		t.Fatal("fixture is not a valid runtime backup manifest")
	}
	if err := schema.Validate(wireValue(t, manifest)); err != nil {
		t.Fatal(err)
	}
	manifest.Members[1].Name = "database.dump"
	if manifest.Valid("staging") || schema.Validate(wireValue(t, manifest)) == nil {
		t.Fatal("duplicate backup members must fail runtime and Backend contract validation")
	}
}
