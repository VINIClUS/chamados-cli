package cli

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"

	"github.com/VINIClUS/chamados-cli/internal/operations"
	"github.com/spf13/cobra"
)

type Job struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type Result = operations.Result

func (a *app) pipelineCommand() *cobra.Command {
	parent := &cobra.Command{Use: "pipeline", Short: "Inspect operation pipelines"}
	watch := &cobra.Command{
		Use:  "watch <pipeline-id>",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id < 1 {
				return fmt.Errorf("invalid pipeline ID")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			return a.watch(cmd.Context(), c, Pipeline{ID: id}, "", "")
		},
	}
	var limit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List pipelines on protected main",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 1 || limit > 100 {
				return fmt.Errorf("limit must be between 1 and 100")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			var pipelines []Pipeline
			if err := c.json(
				cmd.Context(),
				"GET",
				fmt.Sprintf("/pipelines?ref=main&per_page=%d", limit),
				nil,
				&pipelines,
			); err != nil {
				return err
			}
			if pipelines == nil {
				pipelines = []Pipeline{}
			}
			return a.output(pipelines, c.token)
		},
	}
	list.Flags().IntVar(&limit, "limit", 20, "Maximum pipelines (1–100)")
	var selector string
	logs := &cobra.Command{
		Use:   "logs <pipeline-id>",
		Short: "Read a job trace (default ops:run)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id < 1 {
				return fmt.Errorf("invalid pipeline ID")
			}
			c, err := a.client()
			if err != nil {
				return err
			}
			jobs, err := c.jobs(cmd.Context(), id)
			if err != nil {
				return err
			}
			for _, job := range jobs {
				if job.Name != selector && fmt.Sprint(job.ID) != selector {
					continue
				}
				resp, err := c.get(cmd.Context(), fmt.Sprintf("/jobs/%d/trace", job.ID))
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
				if err != nil {
					return fmt.Errorf("cannot read job trace")
				}
				if len(data) > 32<<20 {
					return fmt.Errorf("job trace exceeds 32 MiB; inspect it in GitLab")
				}
				text := redact(string(data), c.token)
				if a.asJSON {
					return a.output(
						map[string]any{"pipeline_id": id, "job_id": job.ID, "trace": text},
						c.token,
					)
				}
				_, err = fmt.Fprint(a.opts.Out, text)
				return err
			}
			return fmt.Errorf("selected job not found in pipeline %d", id)
		},
	}
	logs.Flags().StringVar(&selector, "job", "ops:run", "Job name or ID within the pipeline")
	parent.AddCommand(watch, list, logs)
	return parent
}

var legacyRevision = regexp.MustCompile(`^[0-9a-f]{7,40}(-dirty)?$`)

func (a *app) watch(ctx context.Context, c *apiClient, p Pipeline, operation, environment string) (watchErr error) {
	interval := a.opts.PollInterval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	canceled := func() error {
		return fmt.Errorf(
			"%w; local watch stopped; pipeline %d %s continues remotely; resume with chamadosctl pipeline watch %d",
			ctx.Err(),
			p.ID,
			redact(p.WebURL, c.token),
			p.ID,
		)
	}
	defer func() {
		if watchErr != nil && ctx.Err() != nil {
			watchErr = canceled()
		}
	}()
	for {
		if ctx.Err() != nil {
			return canceled()
		}
		var current Pipeline
		err := c.json(ctx, "GET", fmt.Sprintf("/pipelines/%d", p.ID), nil, &current)
		if ctx.Err() != nil {
			return canceled()
		}
		if err != nil {
			return err
		}
		if current.WebURL != "" {
			p.WebURL = current.WebURL
		}
		fmt.Fprintf(a.opts.Err, "Pipeline %d: %s\n", p.ID, redact(current.Status, c.token))
		switch current.Status {
		case "success", "failed", "canceled", "skipped":
			jobs, err := c.jobs(ctx, p.ID)
			if err != nil {
				return err
			}
			for _, job := range jobs {
				if job.Name != "ops:run" {
					continue
				}
				var result Result
				if err := c.json(
					ctx,
					"GET",
					fmt.Sprintf("/jobs/%d/artifacts/operation-result.json", job.ID),
					nil,
					&result,
				); err != nil {
					return fmt.Errorf(
						"pipeline %d (%s), final result unavailable: %w",
						p.ID,
						current.Status,
						err,
					)
				}
				validRevision := fullSHA.MatchString(result.Revision)
				strictRevision := (result.Status == "success" || result.Status == "succeeded") &&
					(result.Operation == "deploy" || result.Operation == "restore" || result.Operation == "backup")
				if !strictRevision {
					validRevision = result.Revision == "" ||
						legacyRevision.MatchString(result.Revision)
				}
				if result.SchemaVersion != 1 || result.Operation == "" ||
					validateEnvironment(result.Environment) != nil ||
					!validRevision ||
					(operation != "" && result.Operation != operation) ||
					(environment != "" && result.Environment != environment) {
					return fmt.Errorf("pipeline %d returned an invalid operation result", p.ID)
				}
				if err := a.output(result, c.token); err != nil {
					return err
				}
				if current.Status != "success" ||
					(result.Status != "success" && result.Status != "succeeded") {
					return fmt.Errorf(
						"pipeline %d: operation %s",
						p.ID,
						redact(result.Status, c.token),
					)
				}
				return nil
			}
			return fmt.Errorf(
				"pipeline %d finished %s without ops:run result",
				p.ID,
				redact(current.Status, c.token),
			)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return canceled()
		case <-timer.C:
		}
	}
}

func (c *apiClient) jobs(ctx context.Context, id int64) ([]Job, error) {
	var all []Job
	for page := 1; ; page++ {
		var jobs []Job
		if err := c.json(
			ctx,
			"GET",
			fmt.Sprintf("/pipelines/%d/jobs?per_page=100&page=%d&include_retried=false", id, page),
			nil,
			&jobs,
		); err != nil {
			return nil, err
		}
		all = append(all, jobs...)
		if len(jobs) < 100 {
			return all, nil
		}
	}
}
