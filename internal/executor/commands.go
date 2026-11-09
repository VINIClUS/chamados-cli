package executor

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

func (e *execution) run(ctx context.Context, program string, args []string, in io.Reader, out io.Writer) error {
	return e.options.Run(
		ctx,
		Command{Program: program, Args: args, Stdin: in, Stdout: out, Stderr: e.options.Diagnostics},
	)
}

func (e *execution) output(ctx context.Context, program string, args ...string) (string, error) {
	b := &boundedOutput{limit: 32 << 20}
	err := e.run(ctx, program, args, nil, b)
	return b.String(), err
}

func (e *execution) project() string {
	if e.options.Project != "" {
		return e.options.Project
	}
	if e.env.Request.Environment == "production" {
		return "chamados-prod"
	}
	return "chamados-stg"
}

func (e *execution) composeArgs(args ...string) []string {
	return e.applicationTarget(e.app).arguments(args...)
}

func (e *execution) compose(ctx context.Context, in io.Reader, out io.Writer, args ...string) error {
	return e.run(ctx, "docker", e.composeArgs(args...), in, out)
}

func (e *execution) composeOutput(ctx context.Context, args ...string) (string, error) {
	return e.output(ctx, "docker", e.composeArgs(args...)...)
}

func (e *execution) temp() error {
	if e.work != "" {
		return nil
	}
	dir := filepath.Join(e.options.Dir, "incoming")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	work, err := os.MkdirTemp(dir, "operation-*")
	e.work = work
	return err
}

func readText(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func structuredError(err error, phase string) *operations.OperationError {
	result := &operations.OperationError{Code: "operation_failed", Phase: phase}
	var process *ProcessError
	if errors.As(err, &process) {
		result.Code = "command_failed"
		result.ExitCode = process.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		result.Code = "timeout"
		result.ExitCode = 124
	}
	if errors.Is(err, context.Canceled) {
		result.Code = "canceled"
		result.ExitCode = 130
	}
	return result
}
