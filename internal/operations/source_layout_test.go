package operations

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSourceExcludesDevOpsFromRawAndGitLabArchives(t *testing.T) {
	for _, stripRoot := range []bool{false, true} {
		t.Run(map[bool]string{false: "git", true: "gitlab"}[stripRoot], func(t *testing.T) {
			var buffer bytes.Buffer
			var output io.Writer = &buffer
			var compressed *gzip.Writer
			prefix := ""
			if stripRoot {
				prefix = "repository/"
				compressed = gzip.NewWriter(&buffer)
				output = compressed
			}
			writer := tar.NewWriter(output)
			for _, name := range []string{
				"application.txt", "devops/tooling.txt", "cicd/pipeline.yml",
				"infrastructure/application/docker-compose.yml",
				"infrastructure/environments/docker-compose.production.yml",
				"infrastructure/scripts/bootstrap-host.sh",
			} {
				if err := writer.WriteHeader(
					&tar.Header{Name: prefix + name, Mode: 0600, Size: 4},
				); err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(writer, "data"); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if compressed != nil {
				if err := compressed.Close(); err != nil {
					t.Fatal(err)
				}
			}
			root := t.TempDir()
			source := filepath.Join(root, "source.tar")
			if err := os.WriteFile(source, buffer.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(root, "app")
			if err := ExtractSource(source, destination, stripRoot); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(destination, "application.txt")); err != nil {
				t.Fatal(err)
			}
			for _, directory := range []string{"devops", "cicd", "infrastructure/environments", "infrastructure/scripts"} {
				if _, err := os.Stat(filepath.Join(destination, directory)); !os.IsNotExist(err) {
					t.Fatalf("untrusted %s tooling reached application: %v", directory, err)
				}
			}
			if _, err := os.Stat(
				filepath.Join(destination, "infrastructure/application/docker-compose.yml"),
			); err != nil {
				t.Fatal("application infrastructure recipe was excluded:", err)
			}
		})
	}
}
