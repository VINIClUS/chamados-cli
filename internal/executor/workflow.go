package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

// runnerAPI is the capability needed by the pipeline workflow, not its HTTP transport.
type runnerAPI interface {
	Manifest(context.Context, string, string) (operations.Manifest, error)
	ProtectedTag(context.Context, string, string) error
	CreateDeployment(context.Context, string, string, string) (int64, error)
	FinishDeployment(context.Context, int64, string) error
	PublishRelease(context.Context, string) error
}

type remoteExecutor interface {
	Execute(context.Context, Envelope, Identity) (operations.Result, error)
}

type runnerWorkflow struct {
	envelope   Envelope
	identity   Identity
	api        runnerAPI
	remote     remoteExecutor
	resultFile string
}

func (w *runnerWorkflow) execute(ctx context.Context, result operations.Result) (operations.Result, error) {
	if err := w.resolveTarget(ctx); err != nil {
		return result, err
	}
	if err := w.reserveDeployment(ctx, &result); err != nil {
		return result, err
	}

	result, operationErr := w.executeRemote(ctx, result)
	healthErr := w.checkPublicHealth(ctx, &result)
	metadataErr := w.recordDeployment(&result)
	releaseErr := w.publishRelease(ctx, &result)
	err := errors.Join(operationErr, healthErr, metadataErr, releaseErr)
	if result.Status != "success" && err == nil {
		if result.Error != nil {
			err = result.Error
		} else {
			err = fmt.Errorf("remote operation failed")
		}
	}
	return result, err
}

func (w *runnerWorkflow) resolveTarget(ctx context.Context) error {
	request := w.envelope.Request
	switch request.Operation {
	case "deploy":
		if err := operations.ReadJSON(".ops-target/revision.json", &w.envelope.Target); err != nil {
			return err
		}
		return w.envelope.Target.Verify(".ops-target/source.tar")
	case "restore":
		manifest, err := w.api.Manifest(ctx, request.Environment, request.BackupID)
		if err != nil {
			return err
		}
		ref := manifest.Ref
		if request.Environment == "production" {
			ref = request.Ref
		}
		w.envelope.Target = Target{
			SchemaVersion: 1,
			Revision:      manifest.Revision,
			Ref:           ref,
			SHA256:        manifest.SHA256,
		}
	}
	return nil
}

func (w *runnerWorkflow) reserveDeployment(ctx context.Context, result *operations.Result) error {
	request := w.envelope.Request
	if request.Operation != "deploy" && request.Operation != "restore" {
		return nil
	}
	result.Phase = "reserve_deployment"
	target := w.envelope.Target
	if request.Environment == "production" {
		if err := w.api.ProtectedTag(ctx, target.Ref, target.Revision); err != nil {
			return err
		}
	}
	id, err := w.api.CreateDeployment(ctx, request.Environment, target.Revision, target.Ref)
	if err != nil {
		result.Error = &operations.OperationError{Code: "deployment_create_uncertain", Phase: result.Phase}
		return err
	}
	result.DeploymentID = id
	w.envelope.DeploymentID = id
	return operations.WriteJSON(w.resultFile, *result)
}

func (w *runnerWorkflow) executeRemote(ctx context.Context, result operations.Result) (operations.Result, error) {
	result.Phase = "transport"
	remote, err := w.remote.Execute(ctx, w.envelope, w.identity)
	if remote.SchemaVersion == 1 {
		return remote, err
	}
	result.Error = &operations.OperationError{Code: "state_unknown", Phase: "transport"}
	return result, err
}

func (w *runnerWorkflow) checkPublicHealth(ctx context.Context, result *operations.Result) error {
	if result.Status != "success" || w.envelope.Request.Operation != "deploy" {
		return nil
	}
	result.Phase = "public_health"
	if err := checkPublic(ctx, w.envelope.Request.Environment); err != nil {
		result.Status = "failed"
		result.Error = structuredError(err, result.Phase)
		return err
	}
	return nil
}

func (w *runnerWorkflow) recordDeployment(result *operations.Result) error {
	if w.envelope.DeploymentID == 0 {
		return nil
	}
	result.DeploymentID = w.envelope.DeploymentID
	if result.Error != nil && result.Error.Code == "state_unknown" {
		return nil
	}
	status := "failed"
	if result.Status == "success" {
		status = "success"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.api.FinishDeployment(ctx, w.envelope.DeploymentID, status); err != nil {
		result.Status = "failed"
		result.Phase = "record_deployment"
		result.Error = &operations.OperationError{
			Code:      "deployment_update_pending",
			Phase:     result.Phase,
			Retryable: true,
		}
		return err
	}
	return nil
}

func (w *runnerWorkflow) publishRelease(ctx context.Context, result *operations.Result) error {
	request := w.envelope.Request
	if result.Status != "success" || request.Operation != "deploy" || request.Environment != "production" {
		return nil
	}
	result.Phase = "publish_release"
	if err := w.api.PublishRelease(ctx, w.envelope.Target.Ref); err != nil {
		result.Status = "failed"
		result.Error = &operations.OperationError{
			Code:      "release_publish_pending",
			Phase:     result.Phase,
			Retryable: true,
		}
		return err
	}
	result.Phase = "complete"
	return nil
}
