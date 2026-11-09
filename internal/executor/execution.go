package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

// operationData keeps the existing wire fields while making recovery state explicit.
type operationData struct {
	Ref             *string `json:"ref,omitempty"`
	Output          *string `json:"output,omitempty"`
	BackendStopped  *bool   `json:"backend_stopped,omitempty"`
	SafetyBackupID  string  `json:"safety_backup_id,omitempty"`
	MutationStarted bool    `json:"mutation_started,omitempty"`
}

type execution struct {
	options            HostOptions
	env                Envelope
	result             *operations.Result
	journal            operationJournal
	data               operationData
	app, tooling, work string
	backendStopped     bool
}

func (e *execution) setRef(ref string) {
	e.data.Ref = &ref
}

func (e *execution) setOutput(output string) {
	e.data.Output = &output
}

func (e *execution) syncData() {
	if e.env.Request.Operation == "backup" || e.env.Request.Operation == "restore" {
		e.data.BackendStopped = &e.backendStopped
	}
	e.result.Data, _ = json.Marshal(e.data)
}

func (e *execution) phase(phase string) error {
	e.result.Phase = phase
	e.result.Status = "running"
	e.syncData()
	return e.journal.Write(e.env, *e.result)
}

func (e *execution) begin() error {
	if err := e.phase("prepare"); err != nil {
		return err
	}
	if e.env.Request.Operation != "cleanup" {
		e.result.Revision = readText(filepath.Join(e.app, "REVISION"))
		e.setRef(readText(filepath.Join(e.app, "REF")))
	}
	return nil
}

func (e *execution) dispatch(ctx context.Context) error {
	switch e.env.Request.Operation {
	case "backup", "restore", "cleanup":
		return e.backup(ctx)
	case "deploy", "reset":
		return e.deploy(ctx)
	default:
		return e.basic(ctx)
	}
}

func (e *execution) preserveStoppedBackend(err error) {
	if err == nil || e.env.Request.Operation != "restore" || !e.backendStopped {
		return
	}
	cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = e.compose(cleanup, nil, nil, "stop", "chamados-backend")
}

func (e *execution) finish(ctx context.Context, operationErr *error) {
	if *operationErr != nil {
		*operationErr = errors.Join(*operationErr, e.fenceCanceledRestore(ctx))
	}
	e.preserveStoppedBackend(*operationErr)
	if *operationErr != nil {
		e.result.Status = "failed"
		e.result.Error = structuredError(*operationErr, e.result.Phase)
	} else {
		e.result.Status = "success"
		e.result.Phase = "complete"
	}
	e.syncData()
	if err := e.journal.Write(e.env, *e.result); err != nil {
		e.result.Status = "failed"
		e.result.Error = &operations.OperationError{Code: "journal_write_failed", Phase: e.result.Phase}
		*operationErr = errors.Join(*operationErr, err)
	}
	if e.work != "" {
		_ = os.RemoveAll(e.work)
	}
}
