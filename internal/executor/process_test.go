//go:build linux

package executor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessesDoNotInheritJobOrOperatorSecrets(t *testing.T) {
	t.Setenv("CI_JOB_TOKEN", "job-secret")
	t.Setenv("GITLAB_TOKEN", "operator-secret")
	var b bytes.Buffer
	err := RunProcess(
		context.Background(),
		Command{
			Program: "sh",
			Args:    []string{"-c", `printf '%s|%s' "${CI_JOB_TOKEN-unset}" "${GITLAB_TOKEN-unset}"`},
			Stdout:  &b,
		},
	)
	if err != nil || b.String() != "unset|unset" {
		t.Fatalf("child credential exposure: %q %v", b.String(), err)
	}
}

func TestTimeoutKillsDescendantsAndClosesPipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	var b bytes.Buffer
	err := RunProcess(ctx, Command{Program: "sh", Args: []string{"-c", "sleep 30 & wait"}, Stdout: &b})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 4*time.Second {
		t.Fatalf("unbounded process: %v %s", err, time.Since(start))
	}
}

func TestLockExcludesAnotherExecutor(t *testing.T) {
	dir := t.TempDir()
	first, err := lockDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := lockDirectory(dir)
	if second != nil {
		_ = second.Close()
	}
	if err == nil {
		t.Fatal("second executor acquired lock")
	}
	if _, err = os.Stat(dir + "/.deploy.lock"); err != nil {
		t.Fatal(err)
	}
}

func TestCancellationDoesNotReturnWhileTermIgnoringChildIsRunning(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunProcess(ctx, Command{Program: "sh", Args: []string{"-c", `trap 'exit 0' TERM; sh -c 'trap "" TERM; printf %s $$ > "$1"; exec sleep 30' _ "$1" & wait`, "_", pidFile}})
	}()
	var pid int
	deadline := time.Now().Add(3 * time.Second)
	for pid == 0 && time.Now().Before(deadline) {
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(string(b))
		if pid == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if pid == 0 {
		t.Fatal("child did not become ready")
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("cancellation did not return")
	}
	// Signal delivery is asynchronous; allow scheduling, not the old delayed kill.
	deadline = time.Now().Add(500 * time.Millisecond)
	for {
		b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if os.IsNotExist(err) {
			break
		}
		fields := strings.Fields(string(b))
		if len(fields) > 2 && fields[2] == "Z" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancellation left descendant running")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
