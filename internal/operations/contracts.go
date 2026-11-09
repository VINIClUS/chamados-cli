// Package operations defines the versioned contracts shared by the CLI and executor.
package operations

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	ReleaseTag    = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	BackupVersion = regexp.MustCompile(`^[0-9]{14}\.[0-9]+$`)
	FullSHA       = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
	SHA256        = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
	OperationID   = regexp.MustCompile(`^[0-9]+-[0-9]+$`)
)

type Request struct {
	Operation   string `json:"operation"`
	Environment string `json:"environment"`
	Ref         string `json:"ref"`
	Service     string `json:"service,omitempty"`
	Tail        int    `json:"tail"`
	BackupID    string `json:"backup_id,omitempty"`
	Confirm     string `json:"confirm,omitempty"`
}

func (r Request) Validate() error {
	if r.Environment != "staging" && r.Environment != "production" {
		return fmt.Errorf("environment must be staging or production")
	}
	switch r.Operation {
	case "deploy", "status", "logs", "health", "smoke", "restart", "reset", "backup", "restore", "cleanup":
	default:
		return fmt.Errorf("invalid operation")
	}
	if (r.Operation == "reset" || r.Operation == "smoke") && r.Environment == "production" {
		return fmt.Errorf("%s is prohibited in production", r.Operation)
	}
	if (r.Operation == "reset" || r.Operation == "restore") && r.Confirm != r.Environment {
		return fmt.Errorf("%s requires --confirm %s", r.Operation, r.Environment)
	}
	if r.Operation == "restore" && !BackupVersion.MatchString(r.BackupID) {
		return fmt.Errorf("invalid backup ID (expected YYYYMMDDHHMMSS.pipeline_iid)")
	}
	if (r.Operation == "deploy" || r.Operation == "restore") && r.Environment == "production" &&
		!ReleaseTag.MatchString(r.Ref) {
		return fmt.Errorf("production requires protected vX.Y.Z tag ancestral to main")
	}
	if r.Tail < 1 || r.Tail > 10000 {
		return fmt.Errorf("tail must be between 1 and 10000")
	}
	switch r.Service {
	case "", "chamados-backend", "chamados-frontend", "chamados-postgresql":
	default:
		return fmt.Errorf("unknown service")
	}
	return ValidateRef(r.Ref)
}

func ValidateRef(ref string) error {
	if strings.TrimSpace(ref) == "" || strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") ||
		strings.Contains(ref, "@{") ||
		strings.ContainsAny(ref, "\x00\r\n") {
		return fmt.Errorf("invalid ref")
	}
	return nil
}

func (r Request) Services() []string {
	if r.Service != "" {
		return []string{r.Service}
	}
	if r.Operation == "restart" {
		return []string{"chamados-backend", "chamados-frontend"}
	}
	return nil
}

type OperationError struct {
	Code      string `json:"code"`
	Phase     string `json:"phase"`
	ExitCode  int    `json:"exit_code,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

func (e *OperationError) Error() string {
	return fmt.Sprintf("operation %s: %s", e.Phase, e.Code)
}

type Result struct {
	SchemaVersion int             `json:"schema_version"`
	ExecutorSHA   string          `json:"executor_sha,omitempty"`
	BackendSHA    string          `json:"backend_sha,omitempty"`
	Operation     string          `json:"operation"`
	Environment   string          `json:"environment"`
	Status        string          `json:"status"`
	Revision      string          `json:"revision"`
	BackupID      string          `json:"backup_id,omitempty"`
	DeploymentID  any             `json:"deployment_id,omitempty"`
	Data          json.RawMessage `json:"data,omitempty"`
	OperationID   string          `json:"operation_id,omitempty"`
	Phase         string          `json:"phase,omitempty"`
	Applied       bool            `json:"applied,omitempty"`
	Error         *OperationError `json:"error,omitempty"`
}

type Member struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	SchemaVersion int             `json:"schema_version"`
	Environment   string          `json:"environment"`
	Revision      string          `json:"revision"`
	Ref           string          `json:"ref"`
	PostgresMajor json.RawMessage `json:"postgres_major"`
	CreatedAt     string          `json:"created_at"`
	SHA256        string          `json:"sha256,omitempty"`
	Members       []Member        `json:"members,omitempty"`
}

func (m Manifest) Major() int {
	n, _ := strconv.Atoi(strings.Trim(string(m.PostgresMajor), `"`))
	return n
}

func (m Manifest) MetadataValid() bool {
	_, err := time.Parse(time.RFC3339, m.CreatedAt)
	return m.SchemaVersion == 1 && (m.Environment == "staging" || m.Environment == "production") &&
		FullSHA.MatchString(m.Revision) &&
		ValidateRef(m.Ref) == nil &&
		m.Major() >= 10 &&
		err == nil
}

func (m Manifest) Valid(environment string) bool {
	if !m.MetadataValid() || m.Environment != environment || !SHA256.MatchString(m.SHA256) || len(m.Members) != 2 {
		return false
	}
	seen := map[string]bool{}
	for _, item := range m.Members {
		if !SHA256.MatchString(item.SHA256) || item.Size < 0 || seen[item.Name] {
			return false
		}
		seen[item.Name] = true
	}
	return seen["database.dump"] && seen["attachments.tar"]
}
