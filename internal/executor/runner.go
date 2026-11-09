package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

type RunnerOptions struct {
	Binary      string
	Manifest    string
	BuildSHA    string
	ResultFile  string
	Diagnostics io.Writer
	Run         func(context.Context, Command) error
}

func (o RunnerOptions) defaults() RunnerOptions {
	if o.Binary == "" {
		o.Binary = ".ops-executor/chamadosctl"
	}
	if o.Manifest == "" {
		o.Manifest = ".ops-executor/manifest.json"
	}
	if o.ResultFile == "" {
		o.ResultFile = "operation-result.json"
	}
	if o.Run == nil {
		o.Run = RunProcess
	}
	if o.Diagnostics == nil {
		o.Diagnostics = io.Discard
	}
	return o
}

func initialRunnerResult(request operations.Request, id, executorSHA, backendSHA string) operations.Result {
	return operations.Result{
		SchemaVersion: 1,
		ExecutorSHA:   executorSHA,
		BackendSHA:    backendSHA,
		Operation:     request.Operation,
		Environment:   request.Environment,
		Status:        "failed",
		Phase:         "validate",
		OperationID:   id,
		BackupID:      request.BackupID,
	}
}

func validateRunner(request operations.Request, id, revision, backendSHA string) error {
	if err := ProtectedContext(); err != nil {
		return err
	}
	if err := request.Validate(); err != nil {
		return err
	}
	if !operations.OperationID.MatchString(id) {
		return fmt.Errorf("numeric pipeline and job IDs required")
	}
	expectedExecutor := os.Getenv("OPS_EXECUTOR_SHA")
	if expectedExecutor == "" {
		expectedExecutor = os.Getenv("CI_COMMIT_SHA")
	}
	if revision != expectedExecutor {
		return fmt.Errorf("executor revision differs from protected pipeline")
	}
	if !operations.FullSHA.MatchString(backendSHA) {
		return fmt.Errorf("backend revision is not a full SHA")
	}
	if expectedBackend := os.Getenv("CI_COMMIT_SHA"); operations.FullSHA.MatchString(expectedBackend) && backendSHA != expectedBackend {
		return fmt.Errorf("backend revision differs from protected pipeline")
	}
	return nil
}

func saveRunnerResult(path string, result *operations.Result, operationErr *error) {
	if *operationErr != nil && result.Error == nil {
		result.Error = structuredError(*operationErr, result.Phase)
	}
	if err := operations.WriteJSON(path, *result); err != nil {
		*operationErr = errors.Join(*operationErr, err)
	}
}

// Run assembles validated dependencies; runnerWorkflow owns the operation lifecycle.
func Run(
	parent context.Context,
	request operations.Request,
	options RunnerOptions,
) (result operations.Result, err error) {
	options = options.defaults()
	id := os.Getenv("CI_PIPELINE_ID") + "-" + os.Getenv("CI_JOB_ID")
	backendSHA := os.Getenv("OPS_BACKEND_SHA")
	if backendSHA == "" {
		backendSHA = os.Getenv("CI_COMMIT_SHA")
	}
	result = initialRunnerResult(request, id, options.BuildSHA, backendSHA)
	defer saveRunnerResult(options.ResultFile, &result, &err)
	if err = validateRunner(request, id, options.BuildSHA, backendSHA); err != nil {
		return result, err
	}
	identity, err := verifyExecutorArtifact(options)
	if err != nil {
		return result, err
	}
	settings := SettingsFromEnvironment()
	client, err := NewJobClient(settings)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(parent, OperationTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	envelope := Envelope{
		Protocol:    Protocol,
		OperationID: id,
		BuildSHA:    options.BuildSHA,
		BackendSHA:  backendSHA,
		Deadline:    deadline,
		Request:     request,
		Settings:    settings,
		PublicURL:   os.Getenv("DEPLOY_URL"),
	}
	workflow := runnerWorkflow{
		envelope:   envelope,
		identity:   identity,
		api:        client,
		remote:     sshExecutor{options: options},
		resultFile: options.ResultFile,
	}
	return workflow.execute(ctx, result)
}
