package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/VINIClUS/chamados-cli/internal/cli"
	"github.com/VINIClUS/chamados-cli/internal/operations"
)

var version = "dev"

var commit = "unknown"

var date = "unknown"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, out, errOut io.Writer) int {
	cmd := cli.NewCommand(cli.Options{Version: version, Commit: commit, Date: date, Out: out, Err: errOut})
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(errOut, err)
		if errors.Is(err, context.Canceled) {
			return 130
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 124
		}
		var operationError *operations.OperationError
		if errors.As(err, &operationError) && operationError.ExitCode > 0 {
			return operationError.ExitCode
		}
		return 1
	}
	return 0
}
