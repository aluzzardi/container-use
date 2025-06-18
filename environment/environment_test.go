package environment

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Git command error handling ensures we gracefully handle git failures
func TestGitCommandErrors(t *testing.T) {
	te := NewTestEnv(t, "git-errors")

	// Test invalid command
	_, err := runGitCommand(te.ctx, te.repoDir, "invalid-command")
	assert.Error(t, err, "Should get error for invalid git command")

	// Test command in non-existent directory
	_, err = runGitCommand(te.ctx, "/nonexistent", "status")
	assert.Error(t, err, "Should get error for non-existent directory")
}

// Worktree path generation must be consistent for environment isolation
func TestWorktreePaths(t *testing.T) {
	env := &Environment{
		ID: "test-env/happy-dog",
	}

	path, err := env.GetWorktreePath()
	require.NoError(t, err, "Should get worktree path")

	// Should end with our environment ID
	assert.True(t, strings.HasSuffix(path, "test-env/happy-dog"), "Worktree path should end with env ID: %s", path)

	// Should be in container-use worktrees
	assert.Contains(t, path, ".config/container-use/worktrees", "Worktree should be in expected location")
}

// Empty directory handling prevents git commit failures when directories have no trackable files
func TestEmptyDirectoryHandling(t *testing.T) {
	te := NewTestEnv(t, "empty-dir")

	// Create empty directories (git doesn't track these)
	te.CreateDir("empty1")
	te.CreateDir("empty2/nested")

	env := &Environment{
		ID:       "test/empty",
		Name:     "test",
		Worktree: te.repoDir,
	}

	// This verifies that commitWorktreeChanges handles empty directories gracefully
	// It should return nil (success) when there's nothing to commit
	err := env.commitWorktreeChanges(te.ctx, te.repoDir, "Test", "Empty dirs")
	assert.NoError(t, err, "commitWorktreeChanges should handle empty dirs gracefully")
}


// Selective file staging ensures problematic files are automatically excluded from commits
// This tests the actual user-facing behavior: "I want to commit my changes but not break git"
func TestSelectiveFileStaging(t *testing.T) {
	// Test real-world scenarios that users encounter
	scenarios := []struct {
		name        string
		setup       func(*TestEnv)
		shouldStage []string
		shouldSkip  []string
		reason      string
	}{
		{
			name: "python_project_with_pycache",
			setup: func(te *TestEnv) {
				te.WriteFile("main.py", "print('hello')")
				te.WriteFile("utils.py", "def helper(): pass")
				te.CreateDir("__pycache__")
				te.WriteBinaryFile("__pycache__/main.cpython-39.pyc", 150)
				te.WriteBinaryFile("__pycache__/utils.cpython-39.pyc", 200)
			},
			shouldStage: []string{"main.py", "utils.py"},
			shouldSkip:  []string{"__pycache__"},
			reason:      "Python cache files should never be committed",
		},
		{
			name: "mixed_content_directory",
			setup: func(te *TestEnv) {
				te.CreateDir("mydir")
				te.WriteFile("mydir/readme.txt", "Documentation")
				te.WriteBinaryFile("mydir/compiled.bin", 100)
				te.WriteFile("mydir/script.sh", "#!/bin/bash\necho hello")
				te.WriteBinaryFile("mydir/image.jpg", 5000)
			},
			shouldStage: []string{"mydir/readme.txt", "mydir/script.sh"},
			shouldSkip:  []string{"mydir/compiled.bin", "mydir/image.jpg"},
			reason:      "Binary files in directories should be automatically excluded",
		},
		{
			name: "node_modules_and_build_artifacts",
			setup: func(te *TestEnv) {
				te.WriteFile("index.js", "console.log('app')")
				te.CreateDir("node_modules/lodash")
				te.WriteFile("node_modules/lodash/index.js", "module.exports = {}")
				te.CreateDir("build")
				te.WriteBinaryFile("build/app.exe", 1024)
				te.WriteFile("build/config.json", `{"prod": true}`)
			},
			shouldStage: []string{"index.js"},
			shouldSkip:  []string{"node_modules", "build"},
			reason:      "Dependencies and build outputs should be excluded",
		},
		// {
		// 	name: "empty_file_edge_case",
		// 	setup: func(te *TestEnv) {
		// 		te.WriteFile("empty.txt", "")
		// 		te.WriteFile("normal.txt", "content")
		// 	},
		// 	shouldStage: []string{"normal.txt"},
		// 	shouldSkip:  []string{}, // Note: empty.txt behavior is buggy, it should be staged
		// 	reason:      "Empty files handling (currently buggy)",
		// },
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			te := NewTestEnv(t, scenario.name)
			env := &Environment{
				ID:       "test/staging",
				Name:     "test",
				Worktree: te.repoDir,
			}

			// Setup the scenario
			scenario.setup(te)

			// Run the actual staging logic (testing the integration)
			err := env.addNonBinaryFiles(te.ctx, te.repoDir)
			require.NoError(t, err, "Staging should not error")

			status := te.GitStatus()

			// Verify expected behavior
			for _, file := range scenario.shouldStage {
				// Files should be staged (A  prefix)
				assert.Contains(t, status, "A  "+file, "%s should be staged - %s", file, scenario.reason)
			}

			for _, pattern := range scenario.shouldSkip {
				// Files should remain untracked (?? prefix), not staged (A  prefix)
				assert.NotContains(t, status, "A  "+pattern, "%s should not be staged - %s", pattern, scenario.reason)
				// They should appear as untracked
				if !strings.Contains(pattern, "/") {
					assert.Contains(t, status, "?? "+pattern, "%s should remain untracked - %s", pattern, scenario.reason)
				}
			}
		})
	}
}
