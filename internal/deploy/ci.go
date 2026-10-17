package deploy

import (
	"fmt"
	"os"
)

// ciRunURL detects whether we are running in a CI environment and returns the
// run URL. Returns empty string outside CI. Supports GitHub Actions and GitLab CI.
func ciRunURL() string {
	// GitHub Actions: https://docs.github.com/en/actions/writing-workflows/choosing-what-your-workflow-does/store-information-in-variables#default-environment-variables
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		serverURL := os.Getenv("GITHUB_SERVER_URL")
		if serverURL == "" {
			serverURL = "https://github.com"
		}
		repo := os.Getenv("GITHUB_REPOSITORY")
		runID := os.Getenv("GITHUB_RUN_ID")
		if repo != "" && runID != "" {
			return fmt.Sprintf("%s/%s/actions/runs/%s", serverURL, repo, runID)
		}
	}

	// GitLab CI: https://docs.gitlab.com/ee/ci/variables/predefined_variables.html
	if os.Getenv("GITLAB_CI") == "true" {
		if jobURL := os.Getenv("CI_JOB_URL"); jobURL != "" {
			return jobURL
		}
		if pipelineURL := os.Getenv("CI_PIPELINE_URL"); pipelineURL != "" {
			return pipelineURL
		}
		serverURL := os.Getenv("CI_SERVER_URL")
		projectPath := os.Getenv("CI_PROJECT_PATH")
		pipelineID := os.Getenv("CI_PIPELINE_ID")
		if serverURL != "" && projectPath != "" && pipelineID != "" {
			return fmt.Sprintf("%s/%s/-/pipelines/%s", serverURL, projectPath, pipelineID)
		}
	}

	return ""
}
