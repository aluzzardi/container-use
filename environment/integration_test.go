package environment

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPersistenceAcrossSessions verifies that user work survives session ends and restarts
// Behavior: "When I leave and come back, my files and changes are still there"
func TestPersistenceAcrossSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	t.Run("persistence", func(t *testing.T) {
		WithEnvironment(t, "persistence", (*TestEnv).SetupPythonProject, func(t *testing.T, env *Environment) {
			ctx := context.Background()

			// --- Setup: Create work in the environment ---
			env.FileWrite(ctx, "Create config", "config.yaml", "database:\n  host: localhost\n  port: 5432")
			_, err := env.Run(ctx, "Process data", "echo 'Processing started at:' > work.log && date >> work.log", "/bin/sh", false)
			require.NoError(t, err)
			env.FileWrite(ctx, "Create marker", ".session_marker", "session-123")

			envID := env.ID
			worktree := env.Worktree

			// --- Action: Simulate session end by clearing in-memory state ---
			environments = make(map[string]*Environment)

			// --- Verify: Physical files persist on disk after session ends ---
			_, err = os.Stat(worktree)
			assert.NoError(t, err, "Worktree directory should exist after session ends")

			// Verify specific files and their contents
			files := map[string]string{
				"config.yaml":     "host: localhost",
				"work.log":        "Processing started at:",
				".session_marker": "session-123",
			}

			for filename, expectedContent := range files {
				content, err := os.ReadFile(filepath.Join(worktree, filename))
				require.NoError(t, err, "File %s should exist on disk", filename)
				assert.Contains(t, string(content), expectedContent, "File %s content should be preserved", filename)
			}

			// --- Verify: Worktree is still a valid git repository ---
			_, err = runGitCommand(ctx, worktree, "status")
			assert.NoError(t, err, "Git repository should remain valid after session ends")

			t.Logf("Successfully verified work persists for environment %s at %s", envID, worktree)
		})
	})
}

// TestGitTracking verifies comprehensive git tracking for all operations
// Behavior: "Every command and file change is recorded for audit/debugging"
func TestGitTracking(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	t.Run("git_tracking", func(t *testing.T) {
		WithEnvironment(t, "git_tracking", SetupNodeProject, func(t *testing.T, env *Environment) {
			ctx := context.Background()
			v := newVerifier(t, env)

			t.Run("FileOperations", func(t *testing.T) {
				// --- Setup: Create various files ---
				files := []struct {
					path    string
					content string
				}{
					{"config.json", `{"name": "test", "version": "1.0.0"}`},
					{"src/app.js", "const app = require('express')();\napp.listen(3000);"},
					{"config.env", "DATABASE_URL=postgres://localhost/myapp"},
					{"docs/README.md", "# Documentation\n\nTest project docs."},
				}

				for _, f := range files {
					err := env.FileWrite(ctx, "Create "+f.path, f.path, f.content)
					require.NoError(t, err)
				}

				// --- Verify: Each file write creates a commit ---
				gitLog, err := runGitCommand(ctx, env.Worktree, "log", "--oneline")
				require.NoError(t, err)

				for _, f := range files {
					assert.Contains(t, gitLog, "Write "+f.path, "File creation should be tracked: %s", f.path)
				}

				// --- Verify: Latest commit contains the expected file ---
				gitShow, err := runGitCommand(ctx, env.Worktree, "show", "--name-only", "--oneline", "HEAD")
				require.NoError(t, err)
				assert.Contains(t, gitShow, "docs/README.md", "Latest commit should show the last file created")

				// --- Verify: Git diff shows file contents ---
				gitDiff, err := runGitCommand(ctx, env.Worktree, "show", "HEAD:config.json")
				require.NoError(t, err)
				assert.Contains(t, gitDiff, `"version": "1.0.0"`, "File content should be retrievable from git")
			})

			t.Run("CommandAuditLog", func(t *testing.T) {
				t.Skip("Skipping - exposes bug where commands creating empty directories fail git commits - see issue #82")

				// --- Setup: Execute various shell commands ---
				commands := []struct {
					explanation string
					cmd         string
				}{
					{"System info", "uname -a"},
					{"Create build directory", "mkdir -p build/dist"},
					{"Run build", "echo 'Building project...' > build/output.log"},
					{"Check test results", "echo 'All tests passed' && mkdir -p .coverage"},
				}

				for _, c := range commands {
					_, err := env.Run(ctx, c.explanation, c.cmd, "/bin/sh", false)
					require.NoError(t, err)
				}

				// --- Verify: Commands are stored in git notes ---
				gitNotes, err := runGitCommand(ctx, env.Worktree, "log", "--notes="+gitNotesLogRef, "--pretty=format:%B%n--- Notes ---%n%N", "-n", "10")
				require.NoError(t, err)

				for _, c := range commands {
					assert.Contains(t, gitNotes, c.cmd, "Command '%s' should be in audit log", c.cmd)
					assert.Contains(t, gitNotes, c.explanation, "Command explanation '%s' should be in audit log", c.explanation)
				}

				// Using helper for repetitive git log checks
				v.gitLogContains("Run echo 'Running tests")
			})

			t.Run("StateRecovery", func(t *testing.T) {
				// --- Setup: Create application state ---
				env.SetEnv(ctx, "Configure environment", []string{
					"API_URL=https://api.production.com",
					"API_KEY=secret-key-123",
					"NODE_ENV=production",
				})

				env.FileWrite(ctx, "Save app state", "state.json", `{
				"version": "2.1.0",
				"initialized": true,
				"lastDeployment": "2024-01-15T10:30:00Z"
			}`)

				// --- Action: Create a checkpoint ---
				err := env.UpdateConfig(ctx, "Save production state", &EnvironmentConfig{BaseImage: env.Config.BaseImage})
				require.NoError(t, err)

				// --- Verify: State is saved in git notes ---
				stateNotes, err := runGitCommand(ctx, env.Worktree, "notes", "--ref="+gitNotesStateRef, "list")
				require.NoError(t, err)
				assert.NotEmpty(t, stateNotes, "State should be saved in git notes")

				// --- Verify: Can retrieve state from notes ---
				noteContent, err := runGitCommand(ctx, env.Worktree, "notes", "--ref="+gitNotesStateRef, "show", "HEAD")
				require.NoError(t, err)
				assert.Contains(t, noteContent, "NODE_ENV", "Environment variables should be in state")

				// --- Verify: History tracks all operations ---
				assert.GreaterOrEqual(t, len(env.History), 3, "History should contain SetEnv, FileWrite, and Update operations")
				assert.Equal(t, "Update environment", env.History[len(env.History)-1].Name, "Latest history entry should be the update")
			})

			t.Run("CommitMetadata", func(t *testing.T) {
				// --- Setup: Perform operation with specific metadata ---
				startTime := time.Now()
				env.FileWrite(ctx, "Timed operation", "timestamp.txt", startTime.Format(time.RFC3339))

				// --- Verify: Commit exists ---
				_, err := runGitCommand(ctx, env.Worktree, "log", "-1", "--pretty=format:%H")
				require.NoError(t, err)

				// --- Verify: Commit timestamp is reasonable ---
				gitTime, err := runGitCommand(ctx, env.Worktree, "log", "-1", "--pretty=format:%ct")
				require.NoError(t, err)
				gitTimeUnix, err := strconv.ParseInt(strings.TrimSpace(gitTime), 10, 64)
				require.NoError(t, err)
				commitTime := time.Unix(gitTimeUnix, 0)
				assert.WithinDuration(t, startTime, commitTime, 5*time.Second, "Commit time should be close to operation time")
			})
		})
	})
}

// TestSequentialOperationGuarantees demonstrates that operations within an environment
// are processed sequentially, which is a key architectural property of container-use.
// This test documents why race conditions don't occur in practice.
func TestSequentialOperationGuarantees(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	t.Run("OperationsAreSequential", func(t *testing.T) {
		WithEnvironment(t, "sequential_ops", SetupNodeProject, func(t *testing.T, env *Environment) {
			ctx := context.Background()

			// --- IMPORTANT ARCHITECTURAL NOTE ---
			// Container-use processes all operations sequentially within an environment.
			// This is because:
			// 1. The MCP server handles one request at a time from the LLM
			// 2. Each operation (FileWrite, Run, etc.) completes fully before the next begins
			// 3. propagateToWorktree happens synchronously at the end of each operation
			//
			// This means there are NO race conditions in normal usage, even though
			// the code doesn't have extensive locking around the global environments map.

			// Initialize counter
			err := env.FileWrite(ctx, "Init counter", "counter.txt", "0")
			require.NoError(t, err)

			// Perform multiple increment operations
			for i := 1; i <= 5; i++ {
				cmd := fmt.Sprintf(`
					current=$(cat counter.txt)
					echo "Read value: $current" >&2
					sleep 0.1  # Simulate processing time
					next=$((current + 1))
					echo $next > counter.txt
					echo "Wrote value: $next" >&2
				`)
				output, err := env.Run(ctx, fmt.Sprintf("Increment %d", i), cmd, "/bin/sh", false)
				require.NoError(t, err)
				t.Logf("Increment %d output: %s", i, output)
			}

			// Verify final value
			content, err := env.FileRead(ctx, "counter.txt", true, 0, 0)
			require.NoError(t, err)
			assert.Equal(t, "5\n", content, "Counter should be exactly 5, proving sequential execution")

			// --- Demonstration: File state consistency ---
			// Each operation sees the complete state from previous operations

			err = env.FileWrite(ctx, "Create base", "state.json", `{"operations": []}`)
			require.NoError(t, err)

			// Append to state file
			for i := 1; i <= 3; i++ {
				cmd := fmt.Sprintf(`
					# Read current state
					current=$(cat state.json)
					# Add new operation
					echo "$current" | sed 's/]}/,"op%d"]}/' > state.json
				`, i)
				_, err := env.Run(ctx, fmt.Sprintf("Append op%d", i), cmd, "/bin/sh", false)
				require.NoError(t, err)
			}

			// Verify all operations were recorded
			state, err := env.FileRead(ctx, "state.json", true, 0, 0)
			require.NoError(t, err)
			assert.Contains(t, state, "op1", "First operation should be recorded")
			assert.Contains(t, state, "op2", "Second operation should be recorded")
			assert.Contains(t, state, "op3", "Third operation should be recorded")

			// --- Verify: Git history shows sequential commits ---
			gitLog, err := runGitCommand(ctx, env.Worktree, "log", "--oneline", "-n", "10")
			require.NoError(t, err)

			// Commits should appear in order
			lines := strings.Split(strings.TrimSpace(gitLog), "\n")
			// Most recent commits first in git log
			assert.Contains(t, lines[0], "op3")
			assert.Contains(t, lines[1], "op2")
			assert.Contains(t, lines[2], "op1")

			t.Log("Git history confirms sequential operation processing")
		})
	})
}

// TestMultipleEnvironmentsRemainIsolated verifies environment isolation
// Behavior: "Changes in one environment don't affect others"
func TestMultipleEnvironmentsRemainIsolated(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	t.Run("multiple_environments", func(t *testing.T) {
		WithEnvironment(t, "multiple_environments", SetupPythonProject, func(t *testing.T, env1 *Environment) {
			ctx := context.Background()

			// --- Setup: Create second environment from same source ---
			env2, err := Create(ctx, "Staging Environment", env1.Source, "staging")
			require.NoError(t, err, "Should create second environment")
			defer env2.Delete(ctx)

			// Create verifiers for convenience
			v1 := newVerifier(t, env1)
			v2 := newVerifier(t, env2)

			t.Run("FilesAreIsolated", func(t *testing.T) {
				// --- Setup: Create different files in each environment ---
				env1.FileWrite(ctx, "Dev config", "config.dev.json", `{"env": "development", "debug": true}`)
				env1.FileWrite(ctx, "Dev data", "data/dev.txt", "Development data")

				env2.FileWrite(ctx, "Staging config", "config.staging.json", `{"env": "staging", "debug": false}`)
				env2.FileWrite(ctx, "Staging data", "data/staging.txt", "Staging data")

				// --- Verify: Cross-environment access fails ---
				// Using helpers for cleaner assertions
				v2.fileNotExists("config.dev.json")
				v1.fileNotExists("config.staging.json")

				// --- Verify: Each environment sees only its own files ---
				output1, _ := env1.Run(ctx, "List configs", "ls config.*.json", "/bin/sh", false)
				assert.Contains(t, output1, "config.dev.json")
				assert.NotContains(t, output1, "config.staging.json")

				output2, _ := env2.Run(ctx, "List configs", "ls config.*.json", "/bin/sh", false)
				assert.Contains(t, output2, "config.staging.json")
				assert.NotContains(t, output2, "config.dev.json")
			})

			t.Run("CommandsAreIsolated", func(t *testing.T) {
				// --- Setup: Create different tools in each environment ---
				env1.Run(ctx, "Create dev tools", "echo 'eslint config' > .eslintrc", "/bin/sh", false)
				env2.Run(ctx, "Create prod tools", "echo 'pm2 config' > pm2.config.js", "/bin/sh", false)

				// --- Verify: Isolation ---
				v1.commandOutputContains("ls -la | grep -E 'eslint|pm2' || echo 'none'", "eslint")

				output2, err := env2.Run(ctx, "Check staging", "ls -la | grep -E 'eslint|pm2' || echo 'none'", "/bin/sh", false)
				require.NoError(t, err)
				assert.Contains(t, output2, "pm2", "Staging should have pm2 config")
				assert.NotContains(t, output2, "eslint", "Staging should not have eslint config")
			})

			t.Run("HistoriesAreIsolated", func(t *testing.T) {
				// --- Verify: Each environment should have its own history ---
				log1, _ := runGitCommand(ctx, env1.Worktree, "log", "--oneline", "-n", "5")
				log2, _ := runGitCommand(ctx, env2.Worktree, "log", "--oneline", "-n", "5")

				// --- Verify: Histories should diverge after creation ---
				assert.Contains(t, log1, "Write config.dev.json", "Dev should have its own commits")
				assert.NotContains(t, log2, "Write config.dev.json", "Staging should not have dev commits")

				assert.Contains(t, log2, "Write config.staging.json", "Staging should have its own commits")
				assert.NotContains(t, log1, "Write config.staging.json", "Dev should not have staging commits")
			})
		})
	})
}

// TestSystemHandlesProblematicFiles verifies edge cases don't break the system
// Behavior: "Python cache, binary files, and other edge cases don't break operations"
func TestSystemHandlesProblematicFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	// This test verifies that Python development workflows work smoothly
	// even when __pycache__ directories are created. It's a regression test
	// to ensure that common Python artifacts don't interfere with operations.
	t.Run("PythonDevelopmentWorkflow", func(t *testing.T) {

		WithEnvironment(t, "python_cache", SetupPythonProject, func(t *testing.T, env *Environment) {
			ctx := context.Background()

			// --- Setup: Simulate Python development by creating cache directories ---
			// We don't need actual Python, just the directories that Python would create
			_, err := env.Run(ctx, "Simulate Python cache",
				"mkdir -p __pycache__ && "+
					"echo 'binary content' > __pycache__/main.cpython-311.pyc && "+
					"echo 'binary content' > __pycache__/utils.cpython-311.pyc",
				"/bin/sh", false)
			require.NoError(t, err)

			// --- Action: Continue development activities ---
			err = env.FileWrite(ctx, "Add feature", "feature.py", "def new_feature():\n    return True")
			require.NoError(t, err, "Should be able to write new Python files")

			err = env.FileWrite(ctx, "Update main", "main.py", "# Updated\nprint('Hello, Updated World!')")
			require.NoError(t, err, "Should be able to update existing Python files")

			// --- Verify: The system continues to work normally with __pycache__ present ---
			// The main point is that __pycache__ doesn't interfere with normal operations

			// Create more files to ensure continued functionality
			_, err = env.Run(ctx, "Create more cache", "touch __pycache__/feature.cpython-311.pyc", "/bin/sh", false)
			require.NoError(t, err, "Should be able to add more cache files")
		})
	})

	t.Run("BinaryDirectories", func(t *testing.T) {
		t.Skip("Skipping - demonstrates unfixed bug where git commit fails when commands create only binary files")
		// This bug is related to issue #82 but distinct:
		// - Issue #82: Commands producing NO git-trackable changes (empty directories)
		// - This bug: Commands producing ONLY binary files that get filtered out
		// Both stem from coupling audit logging to git commits. When addNonBinaryFiles()
		// skips all files, git commit fails with "nothing to commit" despite untracked files.

		WithEnvironment(t, "binary_dirs", SetupPythonProjectNoGitignore, func(t *testing.T, env *Environment) {
			ctx := context.Background()

			// --- Setup: Create directories with only binary files ---
			_, err := env.Run(ctx, "Create binary directory",
				"mkdir -p __pycache__ && "+
					"dd if=/dev/urandom of=__pycache__/main.cpython-39.pyc bs=1024 count=1 2>/dev/null && "+
					"dd if=/dev/urandom of=__pycache__/utils.cpython-39.pyc bs=1024 count=1 2>/dev/null",
				"/bin/sh", false)

			// --- Verify: This should succeed but currently fails ---
			require.NoError(t, err, "Should handle directories with only binary files")

			// --- Verify: Should still be able to work with text files ---
			err = env.FileWrite(ctx, "Add text file", "notes.txt", "System should handle binary directories gracefully")
			require.NoError(t, err, "Should be able to write text files alongside binary directories")
		})
	})

	t.Run("LargeFiles", func(t *testing.T) {
		WithEnvironment(t, "large_files", SetupNodeProject, func(t *testing.T, env *Environment) {
			ctx := context.Background()

			// --- Setup: Create a moderately large file ---
			_, err := env.Run(ctx, "Create large file",
				"dd if=/dev/urandom of=large.dat bs=1M count=5 2>/dev/null", "/bin/sh", false)

			// --- Verify: System should handle this appropriately ---
			if err != nil {
				// --- Verify: Should give meaningful error ---
				assert.Contains(t, err.Error(), "large", "Error should indicate file size issue")
			}

			// --- Verify: Should still be able to work with normal files ---
			err = env.FileWrite(ctx, "Add config", "config.json", `{"maxFileSize": "5MB"}`)
			assert.NoError(t, err, "Should handle normal files even with large files present")
		})
	})
}

// Large project performance ensures the system scales to real-world codebases
func TestLargeProjectPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test")
	}

	t.Run("large_project_performance", func(t *testing.T) {
		// --- Setup: Custom setup for large project ---
		largeProjectSetup := func(te *TestEnv) {
			// --- Setup: Create many files ---
			for i := 0; i < 100; i++ {
				te.WriteFile(filepath.Join("src", fmt.Sprintf("file%d.js", i)),
					fmt.Sprintf("// File %d\nconsole.log('test');", i))
			}
			te.GitCommit("Large project")
		}

		WithEnvironment(t, "large_project_performance", largeProjectSetup, func(t *testing.T, env *Environment) {
			ctx := context.Background()

			// --- Setup: Environment already created by WithEnvironment ---
			t.Logf("Environment ID: %s", env.ID)

			// --- Action: Time file operations ---
			start := time.Now()
			env.FileWrite(ctx, "Test", "new.txt", "test")
			writeTime := time.Since(start)

			t.Logf("File write took: %v", writeTime)

			// --- Verify: Performance assertions ---
			assert.LessOrEqual(t, writeTime, 2*time.Second, "File write should be fast")
		})
	})
}

// TestWorktreeUpdatesAreVisibleAfterRebuild verifies that file changes persist through environment rebuilds
// Behavior: "When I update a file and rebuild, the new version should be used"
func TestWorktreeUpdatesAreVisibleAfterRebuild(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	t.Run("worktree_cache", func(t *testing.T) {
		WithEnvironment(t, "worktree_cache", SetupNodeProject, func(t *testing.T, env *Environment) {
			ctx := context.Background()
			v := newVerifier(t, env)

			// --- Setup: Create initial script ---
			initialScript := `echo "Version 1"`
			err := env.FileWrite(ctx, "Create script", "script.sh", initialScript)
			require.NoError(t, err)

			// --- Verify: Initial version runs ---
			v.commandOutputContains("sh script.sh", "Version 1")

			// --- Action: Update the script ---
			updatedScript := `echo "Version 2"`
			err = env.FileWrite(ctx, "Update script", "script.sh", updatedScript)
			require.NoError(t, err)

			// --- Action: Rebuild environment (this is where the bug occurs) ---
			err = env.UpdateConfig(ctx, "Force rebuild", &EnvironmentConfig{BaseImage: env.Config.BaseImage, SetupCommands: env.Config.SetupCommands})
			require.NoError(t, err)

			// --- Debug: Check what files are in the container after rebuild ---
			listOutput, err := env.Run(ctx, "List files", "ls -la", "/bin/sh", false)
			require.NoError(t, err)
			t.Logf("Files after rebuild:\n%s", listOutput)

			// --- Debug: Check if script.sh exists ---
			catOutput, err := env.Run(ctx, "Cat script", "cat script.sh 2>&1 || echo 'File not found'", "/bin/sh", false)
			require.NoError(t, err)
			t.Logf("Script content after rebuild: %s", catOutput)

			// --- Verify: Updated version should run (but currently runs old version due to cache) ---
			v.commandOutputContains("sh script.sh", "Version 2")
		})
	})
}

// TestUploadAfterModification verifies that Upload sees the latest file changes
// Behavior: "When I modify files locally and upload, the updated versions should be uploaded"
// Error: "no such file or directory" when trying to upload files created in worktree subdirectory
func TestUploadAfterModification(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	t.Run("upload_cache", func(t *testing.T) {
		t.Skip("Skipping - demonstrates Dagger caching issue where Upload uses cached directory instead of updated local files")
		// This test exposes a Dagger caching behavior where:
		// 1. First Upload of local directory works fine
		// 2. Local files are modified
		// 3. Second Upload uses cached version from step 1, not seeing the modifications
		// The Host.directory() call likely needs noCache:true or similar to ensure fresh reads
		WithEnvironment(t, "upload_cache", SetupNodeProject, func(t *testing.T, env *Environment) {
			ctx := context.Background()
			v := newVerifier(t, env)

			// --- Setup: Create a local directory to upload from ---
			localDir := filepath.Join(env.Worktree, "upload-test")
			err := os.MkdirAll(localDir, 0755)
			require.NoError(t, err)

			// --- Setup: Create initial file ---
			initialContent := "console.log('Version 1');"
			err = os.WriteFile(filepath.Join(localDir, "script.js"), []byte(initialContent), 0644)
			require.NoError(t, err)

			// --- Action: Upload to container ---
			err = env.Upload(ctx, "Upload v1", "file://"+localDir, "/app")
			require.NoError(t, err)

			// --- Verify: Initial version uploaded ---
			v.fileExists("/app/script.js", "Version 1")

			// --- Action: Modify local file ---
			updatedContent := "console.log('Version 2');"
			err = os.WriteFile(filepath.Join(localDir, "script.js"), []byte(updatedContent), 0644)
			require.NoError(t, err)

			// --- Action: Upload again (this is where caching might cause issues) ---
			err = env.Upload(ctx, "Upload v2", "file://"+localDir, "/app")
			require.NoError(t, err)

			// --- Verify: Updated version should be uploaded ---
			content, err := env.FileRead(ctx, "/app/script.js", true, 0, 0)
			require.NoError(t, err)
			assert.Contains(t, content, "Version 2", "Should upload updated version")
			assert.NotContains(t, content, "Version 1", "Should not have old cached version")
		})
	})
}

// TestWeirdUserScenarios verifies the system handles edge cases gracefully
// Behavior: "The system should handle or fail gracefully on unusual user actions"
func TestWeirdUserScenarios(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	t.Run("EnvironmentNameCollisions", func(t *testing.T) {
		WithEnvironment(t, "name_collisions", func(te *TestEnv) {
			te.SetupNodeProject()
		}, func(t *testing.T, env *Environment) {
			ctx := context.Background()

			// Create first environment
			env1, err := Create(ctx, "My App", env.Source, "myapp")
			require.NoError(t, err)
			defer env1.Delete(ctx)

			// Create second environment with SAME name
			env2, err := Create(ctx, "My App", env.Source, "myapp")
			require.NoError(t, err)
			defer env2.Delete(ctx)

			// They should have different IDs despite same name
			assert.NotEqual(t, env1.ID, env2.ID, "Same-named environments should get unique IDs")
			assert.True(t, strings.HasPrefix(env1.ID, "myapp/"), "ID should start with name")
			assert.True(t, strings.HasPrefix(env2.ID, "myapp/"), "ID should start with name")

			// Both should be independently accessible
			assert.NotNil(t, Get(env1.ID), "First env should be retrievable")
			assert.NotNil(t, Get(env2.ID), "Second env should be retrievable")
		})
	})

	t.Run("OrphanedWorktreeRecovery", func(t *testing.T) {
		WithEnvironment(t, "orphaned_worktree", func(te *TestEnv) {
			te.SetupPythonProject()
		}, func(t *testing.T, env *Environment) {
			ctx := context.Background()

			newEnv, err := Create(ctx, "Test", env.Source, "orphan-test")
			require.NoError(t, err)

			// Simulate partial deletion - remove from map but leave worktree
			envID := newEnv.ID
			worktreePath := newEnv.Worktree
			delete(environments, envID)

			// Verify worktree still exists on disk
			_, err = os.Stat(worktreePath)
			assert.NoError(t, err, "Worktree should still exist")

			// Try to create new environment with same name - should work
			env2, err := Create(ctx, "Test", env.Source, "orphan-test")
			require.NoError(t, err, "Should be able to create new env despite orphaned worktree")
			defer env2.Delete(ctx)

			// New environment should have different ID and worktree
			assert.NotEqual(t, envID, env2.ID)
			assert.NotEqual(t, worktreePath, env2.Worktree)
		})
	})

	t.Run("CrossRepositoryConfusion", func(t *testing.T) {
		// Initialize Dagger for this test
		initializeDaggerOnce(t)

		// Create two separate repositories
		te1 := NewTestEnv(t, "repo1")
		te1.SetupNodeProject()

		te2 := NewTestEnv(t, "repo2")
		te2.SetupPythonProject()

		ctx := context.Background()

		// Create environment in repo1
		env1, err := Create(ctx, "App", te1.repoDir, "app")
		require.NoError(t, err)
		defer env1.Delete(ctx)

		// Write file in env1
		err = env1.FileWrite(ctx, "Add file", "app.js", "console.log('repo1');")
		require.NoError(t, err)

		// User accidentally tries to use env1 while in repo2
		// This simulates: cd ../repo2 && cu terminal (with env from repo1 active)
		_, err = env1.FileRead(ctx, "main.py", true, 0, 0)
		assert.Error(t, err, "Should fail to read repo2 files from repo1 environment")

		// The environment is still tied to repo1
		jsContent, err := env1.FileRead(ctx, "app.js", true, 0, 0)
		require.NoError(t, err)
		assert.Contains(t, jsContent, "repo1", "Environment should still access its original repo")
	})

	t.Run("ConfigDirEnvironmentLoss", func(t *testing.T) {
		t.Skip("Skipping - tests assumptions about config dir behavior that need design clarification")

		// Context: CONTAINER_USE_CONFIG_DIR was introduced for test isolation to prevent
		// concurrent tests from interfering with each other. However, this raises questions
		// about how the system should behave if this becomes a user-facing feature.
		//
		// Design questions IF config dir becomes user-configurable:
		// 1. Should environments be "lost" when CONTAINER_USE_CONFIG_DIR changes?
		// 2. Should List() respect CONTAINER_USE_CONFIG_DIR or continue using git remotes?
		// 3. What's the expected user experience when switching config directories?
		//
		// Current behavior:
		// - Get() respects the config dir (returns nil when dir changes)
		// - List() ignores config dir (reads from git remotes which persist)
		// - This creates an inconsistency where List() shows envs that Get() can't retrieve
		//
		// Possible design decisions:
		// A. List() should filter results based on what exists in current config dir
		// B. Config dir changes should be transparent (envs remain accessible)
		// C. Provide a migration tool for moving envs between config dirs
		// D. Keep CONTAINER_USE_CONFIG_DIR as test-only and not expose to users

		// Original test code kept for reference when design is clarified
		/*
			WithEnvironment(t, "ConfigDirEnvironmentLoss", func(te *TestEnv) {
				te.SetupNodeProject()
			}, func(t *testing.T, env *Environment) {
				ctx := context.Background()

				// Create environment with current config dir
				newEnv, err := Create(ctx, "App", env.Source, "app")
				require.NoError(t, err)
				envID := newEnv.ID

				// Simulate user changing CONTAINER_USE_CONFIG_DIR
				oldConfigDir := os.Getenv("CONTAINER_USE_CONFIG_DIR")
				newConfigDir := filepath.Join(filepath.Dir(oldConfigDir), "config-new")
				os.Setenv("CONTAINER_USE_CONFIG_DIR", newConfigDir)
				defer os.Setenv("CONTAINER_USE_CONFIG_DIR", oldConfigDir)

				// Clear in-memory state to simulate new session
				environments = make(map[string]*Environment)

				// Try to Get() the environment - it won't be found
				retrievedEnv := Get(envID)
				assert.Nil(t, retrievedEnv, "Environment is 'lost' when config dir changes")

				// List() also won't find it
				envs, err := List(ctx, env.Source)
				require.NoError(t, err)
				assert.NotContains(t, envs, envID, "Lost environment not in list")

				// Restore config dir
				os.Setenv("CONTAINER_USE_CONFIG_DIR", oldConfigDir)
				newEnv.Delete(ctx) // Clean up with correct config dir
			})
		*/
	})
}

// TestEnvironmentConfigurationPersists verifies configuration persistence
// Behavior: "Base images, setup commands, and configuration persist correctly"
func TestEnvironmentConfigurationPersists(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	t.Run("BaseImagePersists", func(t *testing.T) {
		WithEnvironment(t, "base_image", func(te *TestEnv) {
			te.SetupNodeProject()
		}, func(t *testing.T, env *Environment) {
			ctx := context.Background()

			// --- Setup: Create with Alpine base image and git (needed for operations) ---
			newEnv, err := Create(ctx, "Test environment", env.Source, "alpine-test")
			require.NoError(t, err)
			defer newEnv.Delete(ctx)

			// Update to Alpine with giet
			updatedConfig := newEnv.Config.Copy()
			updatedConfig.BaseImage = "alpine:latest"
			updatedConfig.SetupCommands = []string{"apk add --no-cache git"}
			err = newEnv.UpdateConfig(ctx, "Use Alpine Linux", updatedConfig)
			require.NoError(t, err)

			// --- Action: Save config and reload environment ---
			err = newEnv.Config.Save(newEnv.Source)
			require.NoError(t, err)

			// Simulate reopening the environment (load config from disk)
			reloadedConfig := DefaultConfig()
			err = reloadedConfig.Load(newEnv.Source)
			require.NoError(t, err)

			// --- Verify: Configuration persisted correctly ---
			assert.Equal(t, "alpine:latest", reloadedConfig.BaseImage, "Base image should persist")
			assert.Equal(t, []string{"apk add --no-cache git"}, reloadedConfig.SetupCommands, "Setup commands should persist")
		})
	})

	t.Run("SetupCommandsPersist", func(t *testing.T) {
		WithEnvironment(t, "setup_commands", func(te *TestEnv) {
			te.SetupNodeProject()
		}, func(t *testing.T, env *Environment) {
			ctx := context.Background()

			// --- Setup: Create env and add setup commands ---
			newEnv, err := Create(ctx, "Test with setup", env.Source, "setup-test")
			require.NoError(t, err)
			defer newEnv.Delete(ctx)

			setupCmds := []string{
				"apk add --no-cache curl git",
				"echo 'Setup complete' > /setup.log",
			}
			updatedConfig := newEnv.Config.Copy()
			updatedConfig.BaseImage = "alpine:latest"
			updatedConfig.SetupCommands = setupCmds
			err = newEnv.UpdateConfig(ctx, "Install development tools", updatedConfig)
			require.NoError(t, err)

			// --- Action: Save and reload config ---
			err = newEnv.Config.Save(newEnv.Source)
			require.NoError(t, err)

			// Load config from disk
			reloadedConfig := DefaultConfig()
			err = reloadedConfig.Load(newEnv.Source)
			require.NoError(t, err)

			// --- Verify: Setup commands persisted ---
			assert.Equal(t, setupCmds, reloadedConfig.SetupCommands, "Setup commands should persist")

			// --- Verify: Can modify persisted setup commands ---
			// Remove the echo command, keep only package install
			reloadedConfig.SetupCommands = []string{"apk add --no-cache curl git"}
			err = reloadedConfig.Save(newEnv.Source)
			require.NoError(t, err)

			// Load again to verify the modification persisted
			finalConfig := DefaultConfig()
			err = finalConfig.Load(newEnv.Source)
			require.NoError(t, err)
			assert.Equal(t, []string{"apk add --no-cache curl git"}, finalConfig.SetupCommands, "Modified setup commands should persist")
		})
	})

	t.Run("EnvironmentVariableLimitations", func(t *testing.T) {
		t.Skip("Skipping - demonstrates unfixed limitation with environment variable persistence")

		WithEnvironment(t, "envvar_test", func(te *TestEnv) {
			te.SetupNodeProject()
		}, func(t *testing.T, env *Environment) {
			ctx := context.Background()

			newEnv, err := Create(ctx, "Test env vars", env.Source, "envvar-test")
			require.NoError(t, err)
			defer newEnv.Delete(ctx)

			v := newVerifier(t, newEnv)

			// --- Setup: Set environment variables ---
			vars := []string{
				"API_URL=https://api.example.com",
				"NODE_ENV=production",
				"PORT=3000",
			}
			err = newEnv.SetEnv(ctx, "Configure app", vars)
			require.NoError(t, err)

			// --- Verify: Variables work in current session ---
			v.commandOutputContains("echo API_URL=$API_URL NODE_ENV=$NODE_ENV PORT=$PORT", "API_URL=https://api.example.com")
			v.commandOutputContains("echo API_URL=$API_URL NODE_ENV=$NODE_ENV PORT=$PORT", "NODE_ENV=production")
			v.commandOutputContains("echo API_URL=$API_URL NODE_ENV=$NODE_ENV PORT=$PORT", "PORT=3000")

			// --- Action: Rebuild container ---
			updatedConfig := newEnv.Config.Copy()
			err = newEnv.UpdateConfig(ctx, "Rebuild container", updatedConfig)
			require.NoError(t, err)

			// --- Verify: Environment variables should persist (but currently don't) ---
			v.commandOutputContains("echo API_URL=$API_URL", "API_URL=https://api.example.com")
			v.commandOutputContains("echo NODE_ENV=$NODE_ENV", "NODE_ENV=production")
			v.commandOutputContains("echo PORT=$PORT", "PORT=3000")
		})
	})

	t.Run("LifecycleOperations", func(t *testing.T) {
		WithEnvironment(t, "lifecycle", func(te *TestEnv) {
			te.SetupNodeProject()
		}, func(t *testing.T, env *Environment) {
			ctx := context.Background()

			// --- Action: Test Create ---
			newEnv, err := Create(ctx, "Test lifecycle", env.Source, "lifecycle-test")
			require.NoError(t, err, "Should create environment")
			require.NotNil(t, newEnv)

			v := newVerifier(t, newEnv)
			envID := newEnv.ID
			originalWorktree := newEnv.Worktree

			// --- Verify: Environment is registered ---
			assert.NotNil(t, Get(envID), "Environment should be retrievable")

			// --- Verify: Worktree should be at predictable location ---
			assert.Contains(t, originalWorktree, envID, "Worktree path should contain environment ID")

			// --- Action: Test Update with new base image and setup ---
			// Note: Alpine needs git for internal operations
			setupCmds := []string{"apk add --no-cache git nodejs npm"}
			updatedConfig := newEnv.Config.Copy()
			updatedConfig.BaseImage = "alpine:latest"
			updatedConfig.SetupCommands = setupCmds
			err = newEnv.UpdateConfig(ctx, "Install development tools", updatedConfig)
			require.NoError(t, err, "Should update with setup commands")

			// --- Verify: Setup command was executed ---
			v.commandOutputContains("node --version", "v")

			// --- Verify: Worktree location should be stable ---
			assert.Equal(t, originalWorktree, newEnv.Worktree, "Worktree location should not change")

			// --- Action: Test Delete ---
			err = newEnv.Delete(ctx)
			require.NoError(t, err, "Should delete environment")

			// --- Verify: Cleanup ---
			assert.Nil(t, Get(envID), "Environment should be removed from registry")

			// --- Verify: Worktree is deleted ---
			_, err = os.Stat(newEnv.Worktree)
			assert.True(t, os.IsNotExist(err), "Worktree should be deleted")
		})
	})
}
