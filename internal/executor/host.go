package executor

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

type HostOptions struct {
	Dir, Incoming, Project  string
	BackendURL, FrontendURL string
	PublicURL               string
	HealthTimeout           time.Duration
	Diagnostics             io.Writer
	Run                     func(context.Context, Command) error
}

// Host is the Linux entry point. Journaling and operation state have their own owners.
type Host struct {
	options HostOptions
}

func NewHost(options HostOptions) *Host {
	if options.Dir == "" {
		options.Dir = "/opt/chamados"
	}
	if options.Run == nil {
		options.Run = RunProcess
	}
	if options.Diagnostics == nil {
		options.Diagnostics = io.Discard
	}
	if options.BackendURL == "" {
		options.BackendURL = "http://127.0.0.1:8181/api/auth/csrf"
	}
	if options.FrontendURL == "" {
		options.FrontendURL = "http://127.0.0.1/"
	}
	if options.HealthTimeout <= 0 {
		options.HealthTimeout = 240 * time.Second
	}
	return &Host{options: options}
}

func (h *Host) Inspect(id string) (operations.Result, error) {
	return (operationJournal{Directory: h.options.Dir}).Inspect(id)
}

func validateEnvelope(env Envelope) error {
	if err := env.Request.Validate(); err != nil {
		return err
	}
	if env.Protocol != Protocol || !operations.OperationID.MatchString(env.OperationID) {
		return fmt.Errorf("invalid execution envelope")
	}
	if env.BackendSHA != "" && !operations.FullSHA.MatchString(env.BackendSHA) {
		return fmt.Errorf("invalid backend revision")
	}
	if env.Deadline.IsZero() || !env.Deadline.After(time.Now()) {
		return context.DeadlineExceeded
	}
	return nil
}

func executionContext(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	maximum := time.Now().Add(OperationTimeout)
	if deadline.After(maximum) {
		deadline = maximum
	}
	return context.WithDeadline(parent, deadline)
}

func newHostResult(env Envelope) operations.Result {
	return operations.Result{
		SchemaVersion: 1,
		ExecutorSHA:   env.BuildSHA,
		BackendSHA:    env.BackendSHA,
		Operation:     env.Request.Operation,
		Environment:   env.Request.Environment,
		Status:        "failed",
		Phase:         "validate",
		OperationID:   env.OperationID,
		BackupID:      env.Request.BackupID,
		DeploymentID:  env.DeploymentID,
	}
}

func (h *Host) Execute(parent context.Context, env Envelope) (result operations.Result, err error) {
	result = newHostResult(env)
	if err = validateEnvelope(env); err != nil {
		return result, err
	}
	ctx, cancel := executionContext(parent, env.Deadline)
	defer cancel()

	lock, lockErr := lockDirectory(h.options.Dir)
	if lockErr != nil {
		result.Error = &operations.OperationError{Code: "lock_busy", Phase: "lock", ExitCode: 75}
		return result, result.Error
	}
	defer lock.Close()

	journal := operationJournal{Directory: h.options.Dir}
	if previous, found, readErr := journal.Previous(env); found || readErr != nil {
		if previous.SchemaVersion == 0 {
			result.Phase = "journal"
			result.Error = structuredError(readErr, result.Phase)
			return result, readErr
		}
		return previous, readErr
	}
	state := newExecution(h.options, env, &result, journal)
	defer state.finish(ctx, &err)
	if err = state.begin(); err != nil {
		return result, err
	}
	err = state.dispatch(ctx)
	return result, err
}

func newExecution(options HostOptions, env Envelope, result *operations.Result, journal operationJournal) *execution {
	tooling := filepath.Join(options.Dir, "tooling")
	if options.Incoming != "" {
		tooling = filepath.Join(options.Incoming, "tooling")
	}
	return &execution{
		options: options,
		env:     env,
		result:  result,
		journal: journal,
		app:     filepath.Join(options.Dir, "app"),
		tooling: tooling,
	}
}
