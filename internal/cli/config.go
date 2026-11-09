package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

type Config struct {
	GitLabURL    string `json:"gitlab_url"`
	Project      string `json:"project"`
	OperationRef string `json:"operation_ref"`
}

func (a *app) loadConfig(cmd *cobra.Command) error {
	if a.configPath == "" {
		return fmt.Errorf("cannot find configuration directory; use --config")
	}
	data, err := os.ReadFile(a.configPath)
	if os.IsNotExist(err) {
		return validateGitLabURL(a.base)
	}
	if err != nil {
		return fmt.Errorf("cannot read configuration")
	}
	var config Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("invalid configuration (only gitlab_url, project, operation_ref are allowed)")
	}
	if config.OperationRef != "" && config.OperationRef != "main" {
		return fmt.Errorf("operation_ref must be main")
	}
	if !cmd.Flags().Changed("gitlab-url") && config.GitLabURL != "" {
		a.base = config.GitLabURL
	}
	if !cmd.Flags().Changed("project") && config.Project != "" {
		a.project = config.Project
	}
	return validateGitLabURL(a.base)
}

func (a *app) configCommand() *cobra.Command {
	show := func(cmd *cobra.Command, args []string) error {
		return a.output(Config{a.base, a.project, "main"}, "")
	}
	parent := &cobra.Command{
		Use:   "config",
		Short: "Manage non-secret JSON configuration",
		Args:  cobra.NoArgs,
		RunE:  show,
	}
	parent.AddCommand(&cobra.Command{Use: "show", Args: cobra.NoArgs, RunE: show})
	parent.AddCommand(
		&cobra.Command{
			Use:  "set <key> <value>",
			Args: cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				config := Config{a.base, a.project, "main"}
				switch args[0] {
				case "gitlab_url":
					config.GitLabURL = args[1]
				case "project":
					if strings.TrimSpace(args[1]) == "" {
						return fmt.Errorf("project must not be empty")
					}
					config.Project = args[1]
				case "operation_ref":
					if args[1] != "main" {
						return fmt.Errorf("operation_ref must be main")
					}
				default:
					return fmt.Errorf(
						"allowed configuration keys: gitlab_url, project, operation_ref; tokens belong in GITLAB_TOKEN or --token-file",
					)
				}
				if err := validateGitLabURL(config.GitLabURL); err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Dir(a.configPath), 0700); err != nil {
					return fmt.Errorf("cannot create configuration directory")
				}
				data, err := json.MarshalIndent(config, "", "  ")
				if err != nil {
					return err
				}
				file, err := os.CreateTemp(filepath.Dir(a.configPath), ".chamadosctl-config-*")
				if err != nil {
					return fmt.Errorf("cannot create configuration file")
				}
				defer os.Remove(file.Name())
				if _, err := file.Write(append(data, '\n')); err != nil {
					file.Close()
					return fmt.Errorf("cannot write configuration")
				}
				if err := file.Close(); err != nil {
					return fmt.Errorf("cannot save configuration")
				}
				if err := os.Rename(file.Name(), a.configPath); err != nil {
					return fmt.Errorf("cannot save configuration")
				}
				return a.output(config, "")
			},
		},
	)
	return parent
}

func (a *app) doctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Verify configuration, authentication and project access",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var project struct {
				ID            int64  `json:"id"`
				Path          string `json:"path_with_namespace"`
				DefaultBranch string `json:"default_branch"`
			}
			if err := c.json(cmd.Context(), "GET", "", nil, &project); err != nil {
				return err
			}
			return a.output(
				map[string]any{
					"authenticated":  true,
					"gitlab_url":     a.base,
					"project":        a.project,
					"project_id":     project.ID,
					"path":           project.Path,
					"default_branch": project.DefaultBranch,
					"operation_ref":  "main",
				},
				c.token,
			)
		},
	}
}

func (a *app) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			version := a.opts.Version
			if version == "" {
				version = "dev"
			}
			if a.asJSON {
				return a.output(
					map[string]string{
						"version": version,
						"commit":  a.opts.Commit,
						"date":    a.opts.Date,
					},
					"",
				)
			}
			_, err := fmt.Fprintf(
				a.opts.Out,
				"chamadosctl %s (commit %s, built %s)\n",
				version,
				a.opts.Commit,
				a.opts.Date,
			)
			return err
		},
	}
}
