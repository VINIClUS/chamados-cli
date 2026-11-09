package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/VINIClUS/chamados-cli/internal/executor"
	"github.com/VINIClUS/chamados-cli/internal/operations"
	"github.com/spf13/cobra"
)

type internalExecuteFlags struct {
	remote      bool
	resultFile  string
	operation   string
	environment string
	deployDir   string
}

func internalExecuteCommand(opts Options) *cobra.Command {
	var flags internalExecuteFlags
	cmd := &cobra.Command{
		Use:  "execute",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInternalExecute(cmd, opts, flags)
		},
	}
	cmd.Flags().BoolVar(&flags.remote, "remote", false, "Receive a trusted execution envelope on stdin")
	cmd.Flags().StringVar(&flags.resultFile, "result", "operation-result.json", "Result artifact")
	cmd.Flags().StringVar(&flags.operation, "operation", "", "Internal operation override")
	cmd.Flags().StringVar(&flags.environment, "environment", "", "Internal environment override")
	cmd.Flags().StringVar(
		&flags.deployDir, "deploy-dir", envDefault("CHAMADOS_DEPLOY_DIR", "/opt/chamados"), "Host state directory",
	)
	return cmd
}

func runInternalExecute(cmd *cobra.Command, opts Options, flags internalExecuteFlags) error {
	if err := requireLinuxExecutor(); err != nil {
		return err
	}
	runtime.GOMAXPROCS(1)
	debug.SetMemoryLimit(128 << 20)
	if flags.remote {
		return runInternalRemote(cmd, opts, flags.deployDir)
	}
	request, err := operationFromEnvironment()
	if err != nil {
		return err
	}
	if flags.operation != "" {
		request.Operation = flags.operation
	}
	if flags.environment != "" {
		request.Environment = flags.environment
	}
	result, runErr := executor.Run(cmd.Context(), request, executor.RunnerOptions{
		BuildSHA: opts.Commit, ResultFile: flags.resultFile, Diagnostics: opts.Err,
	})
	return writeInternalResult(opts.Out, result, runErr)
}

func readExecutionEnvelope(in io.Reader, commit string) (executor.Envelope, error) {
	var envelope executor.Envelope
	content, err := io.ReadAll(io.LimitReader(in, (1<<20)+1))
	if err != nil {
		return envelope, err
	}
	if len(content) > 1<<20 || json.Unmarshal(content, &envelope) != nil {
		return envelope, fmt.Errorf("invalid execution envelope")
	}
	if !operations.FullSHA.MatchString(commit) || envelope.BuildSHA != commit {
		return envelope, fmt.Errorf("executor revision mismatch")
	}
	return envelope, nil
}

func remoteHealthTimeout() (time.Duration, error) {
	text := os.Getenv("BACKUP_HEALTH_TIMEOUT")
	if text == "" {
		return 240 * time.Second, nil
	}
	seconds, err := strconv.Atoi(text)
	if err != nil || seconds < 1 {
		return 0, fmt.Errorf("invalid health timeout")
	}
	return time.Duration(seconds) * time.Second, nil
}

func runInternalRemote(cmd *cobra.Command, opts Options, directory string) error {
	envelope, err := readExecutionEnvelope(cmd.InOrStdin(), opts.Commit)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	healthTimeout, err := remoteHealthTimeout()
	if err != nil {
		return err
	}
	host := executor.NewHost(executor.HostOptions{
		Dir:           directory,
		Incoming:      filepath.Dir(executable),
		Project:       os.Getenv("BACKUP_COMPOSE_PROJECT"),
		BackendURL:    os.Getenv("BACKUP_BACKEND_HEALTH_URL"),
		FrontendURL:   os.Getenv("BACKUP_FRONTEND_HEALTH_URL"),
		PublicURL:     envelope.PublicURL,
		HealthTimeout: healthTimeout,
		Diagnostics:   opts.Err,
	})
	result, runErr := host.Execute(cmd.Context(), envelope)
	return writeInternalResult(opts.Out, result, runErr)
}
