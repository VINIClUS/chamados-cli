package executor

import (
	"os"
	"path/filepath"
)

// applicationLayout keeps historical application revisions restorable while new
// revisions keep their build recipes under infrastructure/application.
type applicationLayout struct {
	Compose            string
	BackendDockerfile  string
	FrontendDockerfile string
}

func findApplicationLayout(application string) applicationLayout {
	directory := filepath.Join(application, "infrastructure", "application")
	modern := applicationLayout{
		Compose:            filepath.Join(directory, "docker-compose.yml"),
		BackendDockerfile:  filepath.Join(directory, "backend.Dockerfile"),
		FrontendDockerfile: filepath.Join(directory, "frontend.Dockerfile"),
	}
	if _, err := os.Stat(modern.Compose); !os.IsNotExist(err) {
		return modern
	}
	return applicationLayout{
		Compose:            filepath.Join(application, "docker-compose.yml"),
		BackendDockerfile:  filepath.Join(application, "Dockerfile"),
		FrontendDockerfile: filepath.Join(application, "src", "web", "Dockerfile"),
	}
}

func (layout applicationLayout) files() []string {
	return []string{layout.Compose, layout.BackendDockerfile, layout.FrontendDockerfile}
}
