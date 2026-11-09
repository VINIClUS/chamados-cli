package executor

import (
	"fmt"
	"os"
	"runtime"

	"github.com/VINIClUS/chamados-cli/internal/operations"
)

type Identity struct {
	Protocol    int    `json:"protocol"`
	ExecutorSHA string `json:"executor_sha"`
	SHA256      string `json:"sha256"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
}

func BinaryIdentity(commit string) (Identity, error) {
	exe, err := os.Executable()
	if err != nil {
		return Identity{}, err
	}
	sum, err := operations.Digest(exe)
	return Identity{
		Protocol:    Protocol,
		ExecutorSHA: commit,
		SHA256:      sum,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
	}, err
}

func verifyExecutorArtifact(options RunnerOptions) (Identity, error) {
	var identity Identity
	if err := operations.ReadJSON(options.Manifest, &identity); err != nil {
		return identity, err
	}
	sum, err := operations.Digest(options.Binary)
	if err != nil {
		return identity, err
	}
	if identity.Protocol != Protocol || identity.OS != "linux" || identity.Arch != "amd64" {
		return identity, fmt.Errorf("executor protocol or platform mismatch")
	}
	if !operations.FullSHA.MatchString(options.BuildSHA) || identity.ExecutorSHA != options.BuildSHA {
		return identity, fmt.Errorf("executor artifact provenance mismatch")
	}
	if sum != identity.SHA256 {
		return identity, fmt.Errorf("executor artifact checksum mismatch")
	}
	return identity, nil
}
