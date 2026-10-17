package deploy

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func isolateEnv(t *testing.T, keys ...string) {
	t.Helper()
	orig := make(map[string]string)
	for _, k := range keys {
		orig[k] = os.Getenv(k)
		os.Unsetenv(k)
	}
	t.Cleanup(func() {
		for k, v := range orig {
			if v == "" {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, v)
			}
		}
	})
}

var allCIEnvKeys = []string{
	"GITHUB_ACTIONS",
	"GITHUB_SERVER_URL",
	"GITHUB_REPOSITORY",
	"GITHUB_RUN_ID",
	"GITLAB_CI",
	"CI_JOB_URL",
	"CI_PIPELINE_URL",
	"CI_SERVER_URL",
	"CI_PROJECT_PATH",
	"CI_PIPELINE_ID",
}

func TestCIRunURLGitHubActions(t *testing.T) {
	isolateEnv(t, allCIEnvKeys...)

	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_SERVER_URL", "https://github.enterprise.com")
	t.Setenv("GITHUB_REPOSITORY", "org/repo")
	t.Setenv("GITHUB_RUN_ID", "12345678")

	assert.Equal(t, "https://github.enterprise.com/org/repo/actions/runs/12345678", ciRunURL())
}

func TestCIRunURLGitHubActionsDefaultServer(t *testing.T) {
	isolateEnv(t, allCIEnvKeys...)

	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_REPOSITORY", "soroworks/soroforge")
	t.Setenv("GITHUB_RUN_ID", "99999")

	assert.Equal(t, "https://github.com/soroworks/soroforge/actions/runs/99999", ciRunURL())
}

func TestCIRunURLGitHubActionsIncomplete(t *testing.T) {
	isolateEnv(t, allCIEnvKeys...)

	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_REPOSITORY", "soroworks/soroforge")
	// Missing GITHUB_RUN_ID

	assert.Empty(t, ciRunURL())
}

func TestCIRunURLGitLabCIJobURL(t *testing.T) {
	isolateEnv(t, allCIEnvKeys...)

	t.Setenv("GITLAB_CI", "true")
	t.Setenv("CI_JOB_URL", "https://gitlab.com/group/project/-/jobs/456")

	assert.Equal(t, "https://gitlab.com/group/project/-/jobs/456", ciRunURL())
}

func TestCIRunURLGitLabCIPipelineURL(t *testing.T) {
	isolateEnv(t, allCIEnvKeys...)

	t.Setenv("GITLAB_CI", "true")
	t.Setenv("CI_PIPELINE_URL", "https://gitlab.com/group/project/-/pipelines/789")

	assert.Equal(t, "https://gitlab.com/group/project/-/pipelines/789", ciRunURL())
}

func TestCIRunURLGitLabCIConstructed(t *testing.T) {
	isolateEnv(t, allCIEnvKeys...)

	t.Setenv("GITLAB_CI", "true")
	t.Setenv("CI_SERVER_URL", "https://gitlab.example.com")
	t.Setenv("CI_PROJECT_PATH", "team/service")
	t.Setenv("CI_PIPELINE_ID", "101112")

	assert.Equal(t, "https://gitlab.example.com/team/service/-/pipelines/101112", ciRunURL())
}

func TestCIRunURLGitLabCIIncomplete(t *testing.T) {
	isolateEnv(t, allCIEnvKeys...)

	t.Setenv("GITLAB_CI", "true")
	t.Setenv("CI_SERVER_URL", "https://gitlab.example.com")
	// Missing project path and pipeline id

	assert.Empty(t, ciRunURL())
}

func TestCIRunURLOutsideCI(t *testing.T) {
	isolateEnv(t, allCIEnvKeys...)

	assert.Empty(t, ciRunURL())
}
