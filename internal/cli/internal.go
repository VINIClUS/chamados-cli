package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"

	"github.com/VINIClUS/chamados-cli/internal/operations"
	"github.com/spf13/cobra"
)

func internalCommand(opts Options) *cobra.Command {
	root := &cobra.Command{
		Use:    "internal",
		Hidden: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
	}
	root.AddCommand(internalResolveCommand(opts), internalExecuteCommand(opts), internalInspectCommand(opts))
	return root
}

func requireLinuxExecutor() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("internal executor requires Linux")
	}
	return nil
}

func writeInternalResult(out io.Writer, result operations.Result, operationErr error) error {
	if err := json.NewEncoder(out).Encode(result); err != nil {
		return err
	}
	return operationErr
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func operationFromEnvironment() (operations.Request, error) {
	tail, err := strconv.Atoi(envDefault("OPS_TAIL", "200"))
	if err != nil {
		tail = 0
	}
	return operations.Request{
		Operation:   os.Getenv("OPS_OPERATION"),
		Environment: envDefault("OPS_ENVIRONMENT", "staging"),
		Ref:         envDefault("OPS_REF", "main"),
		Service:     os.Getenv("OPS_SERVICE"),
		Tail:        tail,
		BackupID:    os.Getenv("OPS_BACKUP_ID"),
		Confirm:     os.Getenv("OPS_CONFIRM"),
	}, nil
}
