package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

// operationJournal owns durable results and duplicate-execution decisions.
type operationJournal struct {
	Directory string
}

type journalRecord struct {
	RequestSHA256 string            `json:"request_sha256"`
	Result        operations.Result `json:"result"`
}

func requestHash(env Envelope) string {
	request, _ := json.Marshal(struct {
		Request operations.Request
		Target  Target
	}{Request: env.Request, Target: env.Target})
	sum := sha256.Sum256(request)
	return hex.EncodeToString(sum[:])
}

func (j operationJournal) Path(id string) string {
	return filepath.Join(j.Directory, "operations", id+".json")
}

func (j operationJournal) load(id string) (journalRecord, error) {
	var record journalRecord
	if !operations.OperationID.MatchString(id) {
		return record, fmt.Errorf("invalid operation ID")
	}
	err := operations.ReadJSONLimit(j.Path(id), &record, 64<<20)
	return record, err
}

func (j operationJournal) Inspect(id string) (operations.Result, error) {
	record, err := j.load(id)
	return record.Result, err
}

func (j operationJournal) Write(env Envelope, result operations.Result) error {
	if !operations.OperationID.MatchString(env.OperationID) {
		return fmt.Errorf("invalid operation ID")
	}
	record := journalRecord{RequestSHA256: requestHash(env), Result: result}
	return operations.WriteJSON(j.Path(env.OperationID), record)
}

// Previous returns a recorded result without executing the operation again.
func (j operationJournal) Previous(env Envelope) (operations.Result, bool, error) {
	record, err := j.load(env.OperationID)
	if errors.Is(err, os.ErrNotExist) {
		return operations.Result{}, false, nil
	}
	if err != nil {
		return operations.Result{}, false, err
	}
	if record.RequestSHA256 != requestHash(env) {
		return operations.Result{}, true, fmt.Errorf("operation ID belongs to another request")
	}
	result := record.Result
	if result.Status == "running" {
		result.Status = "failed"
		result.Error = &operations.OperationError{Code: "state_unknown", Phase: result.Phase}
		return result, true, result.Error
	}
	if result.Status == "success" {
		return result, true, nil
	}
	if result.Error != nil {
		return result, true, result.Error
	}
	return result, true, fmt.Errorf("previous operation failed")
}
