package cli

import (
	"encoding/json"
	"fmt"

	"github.com/VINIClUS/chamados-cli/internal/executor"
	"github.com/spf13/cobra"
)

func internalInspectCommand(opts Options) *cobra.Command {
	var binary bool
	var directory string
	cmd := &cobra.Command{
		Use:  "inspect [operation-id]",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runInternalInspect(opts, directory, binary, args)
		},
	}
	cmd.Flags().BoolVar(&binary, "binary", false, "Inspect executor identity")
	cmd.Flags().StringVar(&directory, "deploy-dir", "/opt/chamados", "Host state directory")
	return cmd
}

func runInternalInspect(opts Options, directory string, binary bool, args []string) error {
	if binary {
		if len(args) != 0 {
			return fmt.Errorf("binary inspection takes no operation ID")
		}
		identity, err := executor.BinaryIdentity(opts.Commit)
		if err != nil {
			return err
		}
		return json.NewEncoder(opts.Out).Encode(identity)
	}
	if err := requireLinuxExecutor(); err != nil {
		return err
	}
	if len(args) != 1 {
		return fmt.Errorf("operation ID required")
	}
	result, err := executor.NewHost(executor.HostOptions{Dir: directory}).Inspect(args[0])
	if err != nil {
		return err
	}
	return json.NewEncoder(opts.Out).Encode(result)
}
