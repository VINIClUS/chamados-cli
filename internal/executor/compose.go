package executor

import "path/filepath"

// composeTarget describes one immutable application/configuration pair.
// Staged builds never change the target used to back up the running database.
type composeTarget struct {
	Project         string
	EnvironmentFile string
	Application     string
	ComposeFile     string
	Overlay         string
}

func (target composeTarget) arguments(command ...string) []string {
	arguments := []string{
		"compose",
		"-p", target.Project,
		"--env-file", target.EnvironmentFile,
		"-f", target.ComposeFile,
		"-f", target.Overlay,
	}
	return append(arguments, command...)
}

func (e *execution) applicationTarget(application string) composeTarget {
	return composeTarget{
		Project:         e.project(),
		EnvironmentFile: filepath.Join(e.options.Dir, ".env"),
		Application:     application,
		ComposeFile:     findApplicationLayout(application).Compose,
		Overlay:         filepath.Join(e.tooling, "deploy", "docker-compose."+e.env.Request.Environment+".yml"),
	}
}
