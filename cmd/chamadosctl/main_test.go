package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInterruptedWatchExits130AndIdentifiesContinuingPipeline(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("secret-token"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, progress bytes.Buffer
	code := run(
		ctx,
		[]string{"--config", filepath.Join(dir, "missing"), "--token-file", token, "pipeline", "watch", "42"},
		&out,
		&progress,
	)
	if code != 130 || !strings.Contains(progress.String(), "pipeline 42") ||
		!strings.Contains(progress.String(), "continues") ||
		strings.Contains(progress.String(), "secret-token") {
		t.Fatalf("code=%d progress=%q", code, progress.String())
	}
}
