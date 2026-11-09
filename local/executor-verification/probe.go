// Local development adapter. A nested module keeps it out of production builds and CI discovery.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/VINIClUS/chamados-cli/internal/executor"
	"github.com/VINIClUS/chamados-cli/internal/operations"
)

type input struct {
	Mode      string              `json:"mode"`
	Directory string              `json:"directory"`
	Incoming  string              `json:"incoming"`
	Project   string              `json:"project"`
	HealthURL string              `json:"health_url"`
	Envelope  executor.Envelope   `json:"envelope"`
	Manifest  operations.Manifest `json:"manifest"`
	Output    string              `json:"output"`
}

func main() {
	var in input
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		panic(err)
	}
	var result any
	var err error
	switch in.Mode {
	case "pack":
		err = operations.Pack(in.Directory, in.Manifest, in.Output)
	case "host":
		host := executor.NewHost(executor.HostOptions{
			Dir: in.Directory, Incoming: in.Incoming, Project: in.Project,
			BackendURL: in.HealthURL, FrontendURL: in.HealthURL,
			HealthTimeout: time.Second, Diagnostics: os.Stderr,
		})
		result, err = host.Execute(context.Background(), in.Envelope)
	default:
		err = fmt.Errorf("unsupported local probe mode %q", in.Mode)
	}
	message := ""
	if err != nil {
		message = err.Error()
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"result": result, "error": message})
}
