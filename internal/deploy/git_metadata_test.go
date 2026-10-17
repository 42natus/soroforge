package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitMetadataOutsideRepository(t *testing.T) {
	commit, dirty := gitMetadata(t.TempDir(), "")
	assert.Empty(t, commit)
	assert.False(t, dirty)
}

func TestGitMetadataDetectsCommitAndDirtyState(t *testing.T) {
	dir := t.TempDir()
	runGitTestCommand(t, dir, "init", "-q")
	runGitTestCommand(t, dir, "config", "user.email", "test@example.org")
	runGitTestCommand(t, dir, "config", "user.name", "Test")
	file := filepath.Join(dir, "source.txt")
	require.NoError(t, os.WriteFile(file, []byte("source"), 0o600))
	runGitTestCommand(t, dir, "add", "source.txt")
	runGitTestCommand(t, dir, "commit", "-qm", "initial")

	commit, dirty := gitMetadata(dir, "")
	assert.Len(t, commit, 40)
	assert.False(t, dirty)

	require.NoError(t, os.WriteFile(file, []byte("changed"), 0o600))
	commit, dirty = gitMetadata(dir, "override-hash")
	assert.Equal(t, "override-hash", commit)
	assert.True(t, dirty)
}

func runGitTestCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}
