package executor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProtectedToolingFilesFindsLegacyTreeFromBackendRoot(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		"infrastructure/scripts/bootstrap-host.sh",
		"infrastructure/environments/docker-compose.staging.yml",
		"infrastructure/environments/docker-compose.production.yml",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	files := protectedToolingFiles()
	if len(files) != 3 || files[0][0] != "infrastructure/scripts/bootstrap-host.sh" {
		t.Fatalf("legacy tooling resolved from wrong checkout: %v", files)
	}
}
