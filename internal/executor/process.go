// Package executor implements the trusted Linux runner and host operations.
package executor

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Command struct {
	Program        string
	Args           []string
	Dir            string
	Env            []string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

func safeEnvironment() []string {
	var env []string
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "DOCKER_HOST", "DOCKER_CONTEXT", "SSH_AUTH_SOCK"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

func RunProcess(ctx context.Context, spec Command) error {
	cmd := exec.CommandContext(ctx, spec.Program, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = append(safeEnvironment(), spec.Env...)
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	cmd.WaitDelay = 2 * time.Second
	configureProcess(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		terminateProcessGroup(cmd)
		return ctx.Err()
	}
	if err != nil {
		var code int
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		}
		return &ProcessError{Program: spec.Program, Code: code}
	}
	return nil
}

type ProcessError struct {
	Program string
	Code    int
}

func (e *ProcessError) Error() string {
	return fmt.Sprintf("%s failed (exit %d)", e.Program, e.Code)
}

type boundedOutput struct {
	strings.Builder
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, fmt.Errorf("command output exceeds limit")
	}
	return b.Builder.Write(p)
}

func capture(ctx context.Context, program string, args ...string) (string, error) {
	b := &boundedOutput{limit: 32 << 20}
	err := RunProcess(ctx, Command{Program: program, Args: args, Stdout: b})
	return strings.TrimSpace(b.String()), err
}
