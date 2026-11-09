package operations

import (
	"encoding/json"
	"testing"
)

func TestRejectsUnsafeOperationsBeforeEffects(t *testing.T) {
	for _, req := range []Request{
		{Operation: "reset", Environment: "production", Ref: "main", Tail: 200, Confirm: "production"},
		{Operation: "smoke", Environment: "production", Ref: "main", Tail: 200},
		{Operation: "restore", Environment: "staging", Ref: "main", Tail: 200, BackupID: "20261107120000.42"},
		{Operation: "logs", Environment: "staging", Ref: "main", Tail: 10001},
		{Operation: "restart", Environment: "staging", Ref: "main", Tail: 200, Service: "$(touch unwanted)"},
	} {
		if err := req.Validate(); err == nil {
			t.Fatalf("accepted unsafe request: %+v", req)
		}
	}
}

func TestDefaultRestartDoesNotSelectDatabase(t *testing.T) {
	req := Request{Operation: "restart", Environment: "staging", Ref: "main", Tail: 200}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	got := req.Services()
	if len(got) != 2 || got[0] != "chamados-backend" || got[1] != "chamados-frontend" {
		t.Fatalf("restart targets: %v", got)
	}
	req.Service = "chamados-postgresql"
	if got = req.Services(); len(got) != 1 || got[0] != "chamados-postgresql" {
		t.Fatalf("explicit database: %v", got)
	}
}

func TestResultPreservesRecoveryMetadataAndLegacyShape(t *testing.T) {
	value := Result{
		SchemaVersion: 1,
		Operation:     "restore",
		Environment:   "staging",
		Status:        "failed",
		Revision:      "",
		OperationID:   "12-34",
		Phase:         "replace",
		Error:         &OperationError{Code: "command_failed", Phase: "replace"},
		Data:          json.RawMessage(`{"backend_stopped":true,"safety_backup_id":"20261107120000.42"}`),
	}
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	data := got["data"].(map[string]any)
	if got["schema_version"] != float64(1) || data["backend_stopped"] != true ||
		data["safety_backup_id"] != "20261107120000.42" {
		t.Fatalf("lost recovery metadata: %s", b)
	}
}
