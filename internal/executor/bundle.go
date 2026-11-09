package executor

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// protectedToolingFiles returns current protected deployment files. The legacy
// fallback keeps historical revisions usable in disposable verification.
func protectedToolingFiles() [][2]string {
	current := [][2]string{
		{"scripts/deploy/bootstrap-host.sh", "tooling/deploy/bootstrap-host.sh"},
		{"scripts/deploy/docker-compose.staging.yml", "tooling/deploy/docker-compose.staging.yml"},
		{"scripts/deploy/docker-compose.production.yml", "tooling/deploy/docker-compose.production.yml"},
	}
	currentComplete := true
	for _, file := range current {
		if _, err := os.Stat(file[0]); err != nil {
			currentComplete = false
			break
		}
	}
	if currentComplete {
		return current
	}
	for _, base := range []string{"infrastructure", filepath.Join("..", "infrastructure")} {
		legacy := [][2]string{
			{filepath.Join(base, "scripts/bootstrap-host.sh"), "tooling/deploy/bootstrap-host.sh"},
			{filepath.Join(base, "environments/docker-compose.staging.yml"), "tooling/deploy/docker-compose.staging.yml"},
			{filepath.Join(base, "environments/docker-compose.production.yml"), "tooling/deploy/docker-compose.production.yml"},
		}
		complete := true
		for _, file := range legacy {
			if _, err := os.Stat(file[0]); err != nil {
				complete = false
				break
			}
		}
		if complete {
			return legacy
		}
	}
	return [][2]string{
		{filepath.Join("infrastructure", "scripts/bootstrap-host.sh"), "tooling/deploy/bootstrap-host.sh"},
		{filepath.Join("infrastructure", "environments/docker-compose.staging.yml"), "tooling/deploy/docker-compose.staging.yml"},
		{filepath.Join("infrastructure", "environments/docker-compose.production.yml"), "tooling/deploy/docker-compose.production.yml"},
	}
}

func writeBundle(writer io.Writer, options RunnerOptions, includeSource bool) error {
	tw := tar.NewWriter(writer)
	files := [][2]string{
		{options.Binary, "chamadosctl"},
		{options.Manifest, "manifest.json"},
		{filepath.Join(filepath.Dir(options.Binary), "SHA256SUMS"), "SHA256SUMS"},
	}
	files = append(files, protectedToolingFiles()...)
	if includeSource {
		files = append(files, [2]string{".ops-target/source.tar", "source.tar"})
	}
	for _, file := range files {
		f, err := os.Open(file[0])
		if err != nil {
			_ = tw.Close()
			return err
		}
		info, err := f.Stat()
		if err == nil && !info.Mode().IsRegular() {
			err = fmt.Errorf("non-regular executor artifact")
		}
		if err == nil {
			err = tw.WriteHeader(
				&tar.Header{Name: file[1], Mode: 0600, Size: info.Size(), Typeflag: tar.TypeReg},
			)
		}
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		_ = f.Close()
		if err != nil {
			_ = tw.Close()
			return err
		}
	}
	return tw.Close()
}
