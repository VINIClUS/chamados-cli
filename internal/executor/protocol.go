package executor

import (
	"time"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

const Protocol = 1
const OperationTimeout = 25 * time.Minute

type Envelope struct {
	Protocol     int                `json:"protocol"`
	OperationID  string             `json:"operation_id"`
	BuildSHA     string             `json:"executor_sha"`
	BackendSHA   string             `json:"backend_sha,omitempty"`
	Deadline     time.Time          `json:"deadline"`
	Request      operations.Request `json:"request"`
	Settings     Settings           `json:"settings"`
	Target       Target             `json:"target,omitempty"`
	PublicURL    string             `json:"public_url,omitempty"`
	DeploymentID int64              `json:"deployment_id,omitempty"`
}
