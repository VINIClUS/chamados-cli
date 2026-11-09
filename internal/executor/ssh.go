package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

// sshExecutor owns the remote channel; it does not create or update deployments.
type sshExecutor struct {
	options   RunnerOptions
	target    string
	arguments []string
	incoming  string
}

func executeSSH(
	ctx context.Context,
	env Envelope,
	identity Identity,
	options RunnerOptions,
) (operations.Result, error) {
	return (sshExecutor{options: options.defaults()}).Execute(ctx, env, identity)
}

func (s sshExecutor) Execute(ctx context.Context, env Envelope, identity Identity) (operations.Result, error) {
	if err := s.configure(env); err != nil {
		return operations.Result{}, err
	}
	terminal := false
	defer func() {
		if terminal {
			s.cleanup()
		}
	}()
	if err := s.upload(ctx, env.Request.Operation == "deploy"); err != nil {
		return operations.Result{}, err
	}
	if err := s.verifyBinary(ctx, identity); err != nil {
		return operations.Result{}, err
	}
	observed, runErr := s.start(ctx, env)
	if observed != nil && observed.Status != "running" && (runErr == nil || observed.Status == "failed") {
		terminal = true
		return *observed, nil
	}
	result, completed, err := s.recoverResult(env, observed, runErr)
	terminal = completed
	return result, err
}

func (s *sshExecutor) configure(env Envelope) error {
	s.target = os.Getenv("DEPLOY_SSH")
	key := os.Getenv("DEPLOY_SSH_KEY")
	if s.target == "" {
		s.target = "root@10.10.10.180"
		if env.Request.Environment == "production" {
			s.target = "root@10.10.10.181"
		}
	}
	if strings.HasPrefix(s.target, "-") || !regexp.MustCompile(`^[a-zA-Z0-9_.@:-]+$`).MatchString(s.target) {
		return fmt.Errorf("invalid SSH target")
	}
	if key == "" {
		name := "chamados-stg_ed25519"
		if env.Request.Environment == "production" {
			name = "chamados-prod_ed25519"
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		key = filepath.Join(home, ".ssh", name)
	}
	s.arguments = []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "ServerAliveInterval=10",
		"-o", "ServerAliveCountMax=3",
	}
	if _, err := os.Stat(key); err == nil {
		s.arguments = append(s.arguments, "-i", key, "-o", "IdentitiesOnly=yes")
	}
	s.incoming = "/opt/chamados/incoming/executor-" + env.OperationID
	return nil
}

func quoteShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (s sshExecutor) inDirectory(command string) string {
	return "cd " + quoteShell(s.incoming) + " && " + command
}

func (s sshExecutor) call(ctx context.Context, command string, input io.Reader) (string, error) {
	output := &boundedOutput{limit: 32 << 20}
	arguments := append(append([]string{}, s.arguments...), "--", s.target, command)
	err := s.options.Run(ctx, Command{
		Program: "ssh",
		Args:    arguments,
		Stdin:   input,
		Stdout:  output,
		Stderr:  s.options.Diagnostics,
	})
	return output.String(), err
}

func (s sshExecutor) upload(ctx context.Context, includeSource bool) error {
	reader, writer := io.Pipe()
	packed := make(chan error, 1)
	go func() {
		err := writeBundle(writer, s.options, includeSource)
		_ = writer.CloseWithError(err)
		packed <- err
	}()
	command := "umask 077; mkdir -p " + quoteShell(s.incoming) + " && tar -xf - -C " + quoteShell(s.incoming)
	_, transferErr := s.call(ctx, command, reader)
	_ = reader.Close()
	return errors.Join(transferErr, <-packed)
}

func (s sshExecutor) verifyBinary(ctx context.Context, expected Identity) error {
	command := s.inDirectory(
		"sha256sum --check SHA256SUMS >&2 && chmod 700 chamadosctl && ./chamadosctl internal inspect --binary",
	)
	output, err := s.call(ctx, command, nil)
	if err != nil {
		return err
	}
	var actual Identity
	if json.Unmarshal([]byte(output), &actual) != nil || actual != expected {
		return fmt.Errorf("remote executor identity mismatch")
	}
	return nil
}

func decodeObservation(output string, env Envelope) *operations.Result {
	var result operations.Result
	if json.Unmarshal([]byte(output), &result) != nil || !validRemoteResult(result, env) {
		return nil
	}
	return &result
}

func (s sshExecutor) start(ctx context.Context, env Envelope) (*operations.Result, error) {
	payload, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	command := s.inDirectory("exec ./chamadosctl internal execute --remote")
	output, runErr := s.call(ctx, command, bytes.NewReader(payload))
	return decodeObservation(output, env), runErr
}

func uncertainResult(env Envelope, observed *operations.Result) operations.Result {
	result := newHostResult(env)
	result.Phase = "transport"
	if observed != nil {
		result = *observed
	}
	result.Status = "failed"
	result.Error = &operations.OperationError{Code: "state_unknown", Phase: "transport"}
	return result
}

func (s sshExecutor) recoverResult(
	env Envelope,
	last *operations.Result,
	runErr error,
) (operations.Result, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := s.inDirectory("exec ./chamadosctl internal inspect " + quoteShell(env.OperationID))
	output, inspectErr := s.call(ctx, command, nil)
	if inspectErr == nil {
		if observed := decodeObservation(output, env); observed != nil {
			if observed.Status != "running" {
				return *observed, true, nil
			}
			err := fmt.Errorf("remote operation %s is still running; inspect before retry", env.OperationID)
			return uncertainResult(env, observed), false, err
		}
	}
	err := errors.Join(runErr, inspectErr, fmt.Errorf("remote state uncertain; last validated result retained"))
	return uncertainResult(env, last), false, err
}

func (s sshExecutor) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = s.call(ctx, "rm -rf -- "+quoteShell(s.incoming), nil)
}
