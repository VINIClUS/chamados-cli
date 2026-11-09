package executor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// A canceled Docker client does not cancel an exec inside the daemon. Stopping
// PostgreSQL fences every in-container exec before the terminal journal is written.
// The backend remains stopped; recovery must use the reported safety backup.
func (e *execution) fenceCanceledRestore(operationContext context.Context) error {
	if e.env.Request.Operation != "restore" || !e.data.MutationStarted || operationContext.Err() == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return e.compose(ctx, nil, e.options.Diagnostics, "stop", "chamados-postgresql")
}

func (e *execution) restoreContainerName() string {
	identity := e.options.Dir + ":" + e.env.Request.Environment + ":" + e.env.OperationID
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("chamados-restore-%x", sum[:12])
}

// The name and ownership label allow a fresh Docker connection to remove only
// this operation's helper, even if the attached client was killed before returning.
func (e *execution) runRestoreContainer(ctx context.Context, args []string, input io.Reader) error {
	name := e.restoreContainerName()
	label := "chamados.operation=" + e.env.OperationID
	arguments := []string{"run", "--name", name, "--label", label}
	arguments = append(arguments, args...)
	err := e.run(ctx, "docker", arguments, input, e.options.Diagnostics)
	if err == nil {
		return nil
	}
	return errors.Join(err, e.removeRestoreContainer(name))
}

func (e *execution) removeRestoreContainer(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, err := e.output(ctx, "docker", "inspect", "-f", `{{index .Config.Labels "chamados.operation"}}`, name)
	if err != nil {
		// --rm may already have completed. Preserve the original error and the
		// cleanup error when Docker cannot confirm the helper's identity.
		return fmt.Errorf("cannot inspect restore helper during cleanup: %w", err)
	}
	if strings.TrimSpace(owner) != e.env.OperationID {
		return fmt.Errorf("refusing to remove restore helper with different ownership")
	}
	return e.run(ctx, "docker", []string{"rm", "--force", name}, nil, e.options.Diagnostics)
}
