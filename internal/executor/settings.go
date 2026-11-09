package executor

import (
	"fmt"
	"os"
)

func ProtectedContext() error {
	if os.Getenv("CI_COMMIT_BRANCH") != "main" || os.Getenv("CI_COMMIT_REF_PROTECTED") != "true" {
		return fmt.Errorf("operations require protected main pipeline")
	}
	return nil
}

func SettingsFromEnvironment() Settings {
	return Settings{
		APIURL:      os.Getenv("CI_API_V4_URL"),
		ProjectID:   os.Getenv("CI_PROJECT_ID"),
		ProjectPath: os.Getenv("CI_PROJECT_PATH"),
		PipelineIID: os.Getenv("CI_PIPELINE_IID"),
		Token:       os.Getenv("CI_JOB_TOKEN"),
	}
}
