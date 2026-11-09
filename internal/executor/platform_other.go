//go:build !linux

package executor

import (
	"fmt"
	"os"
	"os/exec"
)

func configureProcess(_ *exec.Cmd) {
	// Process groups belong to the Linux internal executor.
}

func terminateProcessGroup(_ *exec.Cmd) {
	// Public commands on other platforms never start the internal executor.
}

func lockDirectory(_ string) (*os.File, error) {
	return nil, fmt.Errorf("internal executor requires Linux")
}
