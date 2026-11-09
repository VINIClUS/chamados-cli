// Package cli implements chamadosctl without invoking local tools.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/VINIClUS/chamados-cli/internal/operations"
	"github.com/spf13/cobra"
)

type Options struct {
	Out, Err              io.Writer
	ConfigPath            string
	Version, Commit, Date string
	HTTPClient            *http.Client
	PollInterval          time.Duration
}

type app struct {
	opts                                 Options
	base, project, tokenFile, configPath string
	asJSON                               bool
}

type operation struct {
	environment, ref, service, confirm, backupID string
	tail                                         int
	detach                                       bool
}

var backupVersion = regexp.MustCompile(`^[0-9]{14}\.[0-9]+$`)

func NewCommand(opts Options) *cobra.Command {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Err == nil {
		opts.Err = os.Stderr
	}
	a := &app{opts: opts}
	root := &cobra.Command{
		Use:           "chamadosctl",
		Short:         "Operate SistemaDeChamados through GitLab CI",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetOut(opts.Out)
	root.SetErr(opts.Err)
	root.PersistentFlags().StringVar(&a.base, "gitlab-url", "https://gitlab.com", "GitLab URL")
	root.PersistentFlags().StringVar(&a.project, "project", "74155066", "GitLab project ID or path")
	root.PersistentFlags().
		StringVar(&a.tokenFile, "token-file", "", "Read GitLab token from file (otherwise GITLAB_TOKEN)")
	root.PersistentFlags().BoolVar(&a.asJSON, "json", false, "Write JSON output")
	a.configPath = opts.ConfigPath
	if a.configPath == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			a.configPath = filepath.Join(dir, "chamadosctl", "config.json")
		}
	}
	root.PersistentFlags().StringVar(&a.configPath, "config", a.configPath, "JSON configuration file")
	root.PersistentFlags().
		DurationVar(&a.opts.PollInterval, "poll-interval", opts.PollInterval, "Pipeline polling interval (default 3s)")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if cmd.Name() == "version" {
			return nil
		}
		return a.loadConfig(cmd)
	}
	for _, name := range []string{"deploy", "status", "logs", "health", "smoke", "restart", "reset", "restore"} {
		root.AddCommand(a.operationCommand(name, name))
	}
	backup := &cobra.Command{Use: "backup", Short: "Create and retrieve backups"}
	backup.AddCommand(a.operationCommand("create", "backup"))
	a.addBackupCommands(backup)
	root.AddCommand(backup)
	root.AddCommand(a.pipelineCommand())
	root.AddCommand(a.configCommand(), a.doctorCommand(), a.versionCommand())
	root.AddCommand(internalCommand(opts))
	return root
}

func (a *app) operationCommand(name, kind string) *cobra.Command {
	op := operation{}
	use := name + " [environment]"
	if kind == "restore" {
		use = name + " <environment> --backup ID --confirm ENVIRONMENT"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: "Run " + kind + " in GitLab CI",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if kind == "restore" {
				if len(args) == 1 && backupVersion.MatchString(args[0]) {
					if op.backupID != "" {
						return fmt.Errorf("specify backup ID only once")
					}
					op.backupID = args[0]
				} else if len(args) == 1 {
					op.environment = args[0]
				}
				if op.backupID == "" {
					return fmt.Errorf("restore requires --backup ID")
				}
			} else if len(args) == 1 {
				op.environment = args[0]
			}
			if err := op.validate(kind); err != nil {
				return err
			}
			client, err := a.client()
			if err != nil {
				return err
			}
			if op.environment == "production" && (kind == "deploy" || kind == "restore") {
				if err := client.validateProductionRef(cmd.Context(), op.ref); err != nil {
					return err
				}
			}
			payload := map[string]any{
				"ref": "main",
				"inputs": map[string]any{
					"operation":   kind,
					"environment": op.environment,
					"ref":         op.ref,
					"tail":        op.tail,
					"service":     op.service,
					"backup_id":   op.backupID,
					"confirm":     op.confirm,
				},
			}
			var pipeline Pipeline
			if err := client.json(cmd.Context(), "POST", "/pipeline", payload, &pipeline); err != nil {
				return err
			}
			fmt.Fprintf(a.opts.Err, "Pipeline %d %s\n", pipeline.ID, redact(pipeline.WebURL, client.token))
			if op.detach {
				return a.output(pipeline, client.token)
			}
			return a.watch(cmd.Context(), client, pipeline, kind, op.environment)
		},
	}
	cmd.Flags().StringVar(&op.environment, "env", "staging", "Environment: staging or production")
	cmd.Flags().StringVar(&op.environment, "environment", "staging", "Environment: staging or production")
	cmd.Flags().StringVar(&op.ref, "ref", "main", "Application ref (production deploy/restore requires vX.Y.Z)")
	cmd.Flags().StringVar(&op.service, "service", "", "Service name (empty means all)")
	cmd.Flags().StringVar(&op.confirm, "confirm", "", "Confirm reset/restore by naming the environment")
	cmd.Flags().IntVar(&op.tail, "tail", 200, "Log lines, 1–10000")
	cmd.Flags().BoolVar(&op.detach, "detach", false, "Return pipeline immediately")
	if kind == "restore" {
		cmd.Flags().StringVar(&op.backupID, "backup", "", "Backup ID (YYYYMMDDHHMMSS.pipeline_iid)")
	}
	return cmd
}

func (op operation) validate(kind string) error {
	return (operations.Request{Operation: kind, Environment: op.environment, Ref: op.ref, Service: op.service, Tail: op.tail, BackupID: op.backupID, Confirm: op.confirm}).Validate()
}

func (a *app) output(value any, token string) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	text := redact(string(data), token)
	_, err = fmt.Fprintln(a.opts.Out, text)
	return err
}

func redact(text, token string) string {
	if token != "" {
		return strings.ReplaceAll(text, token, "[REDACTED]")
	}
	return text
}
