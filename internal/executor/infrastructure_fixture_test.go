package executor

import (
	"os"
	"path/filepath"
	"testing"
)

func writeProtectedInfrastructure(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{
		"scripts/bootstrap-host.sh",
		"environments/docker-compose.staging.yml",
		"environments/docker-compose.production.yml",
	} {
		path := filepath.Join(root, "infrastructure", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("protected fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
