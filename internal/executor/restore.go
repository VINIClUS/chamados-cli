package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

type restorePlan struct {
	Manifest          operations.Manifest
	Ref               string
	VerifiedDirectory string
	Application       string
}

func (e *execution) restore(ctx context.Context, snapshot snapshotState) error {
	plan, err := e.prepareRestore(ctx, snapshot)
	if err != nil {
		return err
	}
	if err = e.publishSafetySnapshot(ctx, snapshot); err != nil {
		return err
	}
	if err = e.replaceDatabase(ctx, plan); err != nil {
		return err
	}
	if err = e.replaceAttachments(ctx, snapshot, plan); err != nil {
		return err
	}
	return e.activateRestore(ctx, plan)
}

func (e *execution) prepareRestore(ctx context.Context, snapshot snapshotState) (restorePlan, error) {
	var plan restorePlan
	if err := e.phase("validate_backup"); err != nil {
		return plan, err
	}
	download := filepath.Join(e.work, "download")
	if err := snapshot.client.DownloadBackup(
		ctx,
		e.env.Request.Environment,
		e.env.Request.BackupID,
		download,
	); err != nil {
		return plan, err
	}
	plan.VerifiedDirectory = filepath.Join(e.work, "verified")
	manifest, err := operations.Verify(
		filepath.Join(download, "backup.tar.gz"),
		filepath.Join(download, "manifest.json"),
		e.env.Request.Environment,
		snapshot.major,
		plan.VerifiedDirectory,
	)
	if err != nil {
		return plan, err
	}
	plan.Manifest = manifest
	plan.Ref = manifest.Ref
	if e.env.Request.Environment == "production" {
		plan.Ref = e.env.Request.Ref
		if err = snapshot.client.VerifyProduction(ctx, plan.Ref, manifest.Revision); err != nil {
			return plan, err
		}
	}
	if e.env.Target.Revision != "" && (e.env.Target.Revision != manifest.Revision || e.env.Target.Ref != plan.Ref) {
		return plan, fmt.Errorf("restore target changed since deployment reservation")
	}
	if err = e.validateDatabaseDump(ctx, plan); err != nil {
		return plan, err
	}
	return e.prepareRestoreApplication(ctx, snapshot, plan)
}

func (e *execution) validateDatabaseDump(ctx context.Context, plan restorePlan) error {
	return e.restoreInput(
		ctx,
		filepath.Join(plan.VerifiedDirectory, "database.dump"),
		"exec", "-T", "chamados-postgresql", "pg_restore", "--list",
	)
}

func (e *execution) prepareRestoreApplication(
	ctx context.Context,
	snapshot snapshotState,
	plan restorePlan,
) (restorePlan, error) {
	if err := e.phase("build"); err != nil {
		return plan, err
	}
	source := filepath.Join(e.work, "source.tar.gz")
	if err := snapshot.client.Download(
		ctx,
		"repository/archive.tar.gz?sha="+plan.Manifest.Revision,
		source,
	); err != nil {
		return plan, err
	}
	plan.Application = filepath.Join(e.work, "app")
	if err := operations.ExtractSource(source, plan.Application, true); err != nil {
		return plan, err
	}
	if err := writeRevision(plan.Application, plan.Manifest.Revision, plan.Ref); err != nil {
		return plan, err
	}
	return plan, e.buildApplication(ctx, plan.Application)
}

func (e *execution) publishSafetySnapshot(ctx context.Context, snapshot snapshotState) error {
	if err := e.phase("safety_backup"); err != nil {
		return err
	}
	if err := e.stopBackend(ctx); err != nil {
		return err
	}
	id, err := e.captureSnapshot(ctx, snapshot, snapshot.revision, snapshot.ref, filepath.Join(e.work, "safety"))
	if err != nil {
		return err
	}
	e.data.SafetyBackupID = id
	// The durable safety ID is the barrier before any destructive command.
	return e.phase("replace")
}

func (e *execution) replaceDatabase(ctx context.Context, plan restorePlan) error {
	e.data.MutationStarted = true
	if err := e.compose(
		ctx,
		nil,
		e.options.Diagnostics,
		"exec",
		"-T",
		"chamados-postgresql",
		"sh",
		"-eu",
		"-c",
		`dropdb -U "$POSTGRES_USER" --if-exists --force "$POSTGRES_DB"; createdb -U "$POSTGRES_USER" -O "$POSTGRES_USER" "$POSTGRES_DB"`,
	); err != nil {
		return err
	}
	return e.restoreInput(
		ctx, filepath.Join(plan.VerifiedDirectory, "database.dump"),
		"exec", "-T", "chamados-postgresql", "sh", "-eu", "-c",
		`exec pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --exit-on-error --no-owner --no-privileges`,
	)
}

func (e *execution) replaceAttachments(ctx context.Context, snapshot snapshotState, plan restorePlan) error {
	attachments, err := os.Open(filepath.Join(plan.VerifiedDirectory, "attachments.tar"))
	if err != nil {
		return err
	}
	defer attachments.Close()
	return e.runRestoreContainer(ctx, []string{
		"--rm", "-i", "--network", "none", "--read-only",
		"-v", snapshot.volume + ":/snapshot", "--entrypoint", "sh", snapshot.image, "-eu", "-c",
		`find /snapshot -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +; tar -xf - -C /snapshot`,
	}, attachments)
}

const noRestoreMigrations = `services:
  chamados-backend:
    environment:
      SPRING_LIQUIBASE_ENABLED: "false"
      APP_INITIALIZER_ENABLED: "false"
`

func (e *execution) activateRestore(ctx context.Context, plan restorePlan) error {
	if err := e.install(plan.Application); err != nil {
		return err
	}
	e.result.Revision = plan.Manifest.Revision
	e.setRef(plan.Ref)
	e.result.BackupID = e.env.Request.BackupID
	e.result.Applied = true
	override := filepath.Join(e.work, "no-migrations.yml")
	if err := os.WriteFile(override, []byte(noRestoreMigrations), 0600); err != nil {
		return err
	}
	if err := e.run(
		ctx,
		"docker",
		e.composeArgs("-f", override, "up", "-d", "--no-build", "--remove-orphans"),
		nil,
		e.options.Diagnostics,
	); err != nil {
		return err
	}
	if err := e.phase("health"); err != nil {
		return err
	}
	if err := e.health(ctx); err != nil {
		return err
	}
	e.backendStopped = false
	return nil
}
