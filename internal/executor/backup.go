package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

type snapshotState struct {
	client        *JobClient
	image, volume string
	major         int
	revision, ref string
}

func (e *execution) backup(ctx context.Context) (err error) {
	client, err := NewJobClient(e.env.Settings)
	if err != nil {
		return err
	}
	if e.env.Request.Operation == "cleanup" {
		if err = e.phase("retention"); err != nil {
			return err
		}
		return client.Cleanup(ctx, e.env.Request.Environment)
	}
	if err = e.temp(); err != nil {
		return err
	}
	state, err := e.snapshotState(ctx, client)
	if err != nil {
		return err
	}
	if e.env.Request.Operation == "restore" {
		return e.restore(ctx, state)
	}
	running, err := e.composeOutput(ctx, "ps", "-q", "--status", "running", "chamados-backend")
	if err != nil {
		return err
	}
	// Capture must reopen a previously running backend on both success and failure.
	defer func() {
		if strings.TrimSpace(running) != "" {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			restartErr := e.compose(cleanup, nil, e.options.Diagnostics, "start", "chamados-backend")
			cancel()
			if restartErr == nil {
				e.backendStopped = false
			}
			if err == nil {
				err = restartErr
			}
		}
	}()
	if err = e.phase("capture"); err != nil {
		return err
	}
	if err = e.stopBackend(ctx); err != nil {
		return err
	}
	id, err := e.captureSnapshot(ctx, state, state.revision, state.ref, filepath.Join(e.work, "snapshot"))
	if err != nil {
		return err
	}
	e.result.BackupID = id
	e.result.Revision = state.revision
	e.setRef(state.ref)
	return nil
}

func (e *execution) snapshotState(ctx context.Context, c *JobClient) (snapshotState, error) {
	s := snapshotState{client: c, volume: e.project() + "_call_attachments"}
	if readText(filepath.Join(e.app, "REVISION")) == "" {
		return s, fmt.Errorf("no existing deployment to back up")
	}
	if err := e.compose(ctx, nil, nil, "config", "--quiet"); err != nil {
		return s, err
	}
	revision := readText(filepath.Join(e.app, "REVISION"))
	if !regexp.MustCompile(`^[a-fA-F0-9]{7,40}$`).MatchString(revision) {
		return s, fmt.Errorf("legacy dirty/unknown revision cannot be backed up")
	}
	full, err := c.ResolveRevision(ctx, revision)
	if err != nil {
		return s, err
	}
	if !strings.HasPrefix(strings.ToLower(full), strings.ToLower(revision)) {
		return s, fmt.Errorf("revision resolution mismatch")
	}
	s.revision = full
	s.ref = readText(filepath.Join(e.app, "REF"))
	if s.ref == "" {
		s.ref = full
	}
	container, err := e.composeOutput(ctx, "ps", "-q", "--status", "running", "chamados-postgresql")
	if err != nil {
		return s, err
	}
	container = strings.TrimSpace(container)
	if container == "" {
		return s, fmt.Errorf("PostgreSQL must be running")
	}
	s.image, err = e.output(ctx, "docker", "inspect", "-f", "{{.Image}}", container)
	if err != nil {
		return s, err
	}
	s.image = strings.TrimSpace(s.image)
	if s.image == "" || strings.HasPrefix(s.image, "-") || strings.ContainsAny(s.image, "\r\n\x00") {
		return s, fmt.Errorf("invalid database image")
	}
	if _, err = e.output(
		ctx,
		"docker",
		"run",
		"--rm",
		"--network",
		"none",
		"--read-only",
		"--entrypoint",
		"tar",
		s.image,
		"--help",
	); err != nil {
		return s, err
	}
	if _, err = e.output(ctx, "docker", "volume", "inspect", s.volume); err != nil {
		return s, err
	}
	version, err := e.composeOutput(
		ctx,
		"exec",
		"-T",
		"chamados-postgresql",
		"sh",
		"-eu",
		"-c",
		`exec psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc 'SHOW server_version_num'`,
	)
	if err != nil {
		return s, err
	}
	number, err := strconv.Atoi(strings.TrimSpace(version))
	if err != nil || number < 100000 {
		return s, fmt.Errorf("cannot identify PostgreSQL major")
	}
	s.major = number / 10000
	return s, nil
}

func (e *execution) stopBackend(ctx context.Context) error {
	e.backendStopped = true
	return e.compose(ctx, nil, e.options.Diagnostics, "stop", "chamados-backend")
}

func (e *execution) captureSnapshot(
	ctx context.Context,
	s snapshotState,
	revision, ref, directory string,
) (string, error) {
	raw := filepath.Join(directory, "raw")
	if err := os.MkdirAll(raw, 0700); err != nil {
		return "", err
	}
	files := []struct {
		name string
		args []string
	}{
		{
			"database.dump",
			e.composeArgs(
				"exec",
				"-T",
				"chamados-postgresql",
				"sh",
				"-eu",
				"-c",
				`exec pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --format=custom --no-owner --no-privileges`,
			),
		},
		{
			"attachments.tar",
			[]string{
				"run",
				"--rm",
				"--network",
				"none",
				"--read-only",
				"-v",
				s.volume + ":/snapshot:ro",
				"--entrypoint",
				"tar",
				s.image,
				"-C",
				"/snapshot",
				"-cf",
				"-",
				".",
			},
		},
	}
	for _, file := range files {
		f, err := os.OpenFile(filepath.Join(raw, file.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", err
		}
		err = e.run(ctx, "docker", file.args, nil, f)
		closeErr := f.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	m := operations.Manifest{
		SchemaVersion: 1,
		Environment:   e.env.Request.Environment,
		Revision:      revision,
		Ref:           ref,
		PostgresMajor: json.RawMessage(strconv.Itoa(s.major)),
		CreatedAt:     time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	output := filepath.Join(directory, "package")
	if err := operations.Pack(raw, m, output); err != nil {
		return "", err
	}
	if !regexp.MustCompile(`^[0-9]+$`).MatchString(e.env.Settings.PipelineIID) {
		return "", fmt.Errorf("numeric pipeline IID required")
	}
	id := time.Now().UTC().Format("20060102150405") + "." + e.env.Settings.PipelineIID
	if err := s.client.Publish(ctx, e.env.Request.Environment, id, output); err != nil {
		return "", err
	}
	return id, nil
}

func (e *execution) restoreInput(ctx context.Context, file string, args ...string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return e.compose(ctx, f, nil, args...)
}
