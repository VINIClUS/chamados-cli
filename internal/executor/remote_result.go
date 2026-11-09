package executor

import "github.com/VINIClUS/chamados-cli/internal/operations"

func validRemoteResult(r operations.Result, env Envelope) bool {
	if r.SchemaVersion != 1 || r.OperationID != env.OperationID || r.Operation != env.Request.Operation ||
		r.Environment != env.Request.Environment {
		return false
	}
	if r.ExecutorSHA != "" && r.ExecutorSHA != env.BuildSHA {
		return false
	}
	if env.BackendSHA != "" && r.BackendSHA != "" && r.BackendSHA != env.BackendSHA {
		return false
	}
	if r.Status != "success" && r.Status != "failed" && r.Status != "running" {
		return false
	}
	if r.Status == "success" && (r.Operation == "deploy" || r.Operation == "restore" || r.Operation == "backup") &&
		!operations.FullSHA.MatchString(r.Revision) {
		return false
	}
	return true
}
