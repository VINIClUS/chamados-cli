package cli

import (
	"context"
	"encoding/json"

	"github.com/VINIClUS/chamados-cli/internal/executor"
	"github.com/spf13/cobra"
)

func internalResolveCommand(opts Options) *cobra.Command {
	return &cobra.Command{
		Use:  "resolve",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInternalResolve(cmd.Context(), opts)
		},
	}
}

func runInternalResolve(parent context.Context, opts Options) error {
	if err := requireLinuxExecutor(); err != nil {
		return err
	}
	if err := executor.ProtectedContext(); err != nil {
		return err
	}
	request, err := operationFromEnvironment()
	if err != nil {
		return err
	}
	client, err := executor.NewJobClient(executor.SettingsFromEnvironment())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, executor.OperationTimeout)
	defer cancel()
	target, err := executor.Resolve(ctx, request, client, ".ops-target")
	if err != nil {
		return err
	}
	return json.NewEncoder(opts.Out).Encode(target)
}
