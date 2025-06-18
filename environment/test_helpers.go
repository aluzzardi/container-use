package environment

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"dagger.io/dagger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testDaggerClient *dagger.Client
	daggerOnce       sync.Once
	daggerErr        error
)

// init sets up logging for tests
func init() {
	// Only show warnings and errors in tests unless TEST_VERBOSE is set
	level := slog.LevelWarn
	if os.Getenv("TEST_VERBOSE") != "" {
		level = slog.LevelInfo
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})))
}

// EnvironmentSetup defines how to set up a test environment
type EnvironmentSetup func(te *TestEnv)

// WithEnvironment runs a test function with a container-use environment
// It handles all initialization, cleanup, and provides a clean test environment
func WithEnvironment(t *testing.T, name string, setup EnvironmentSetup, fn func(t *testing.T, env *Environment)) {
	initializeDaggerOnce(t)

	// Create test environment
	te := NewTestEnv(t, name)

	// Run custom setup if provided
	if setup != nil {
		setup(te)
	}

	// Create environment directly
	env, err := Create(te.ctx, "Test environment", te.repoDir, name)
	require.NoError(t, err, "Failed to create environment")
	te.env = env

	// Run the test
	fn(t, env)
}

// Common setups
var (
	SetupPythonProject = func(te *TestEnv) {
		te.SetupPythonProject()
	}

	SetupPythonProjectNoGitignore = func(te *TestEnv) {
		te.SetupPythonProjectWithOptions(false)
	}

	SetupNodeProject = func(te *TestEnv) {
		te.SetupNodeProject()
	}

	SetupEmptyProject = func(te *TestEnv) {
		// Just create initial commit
		te.WriteFile("README.md", "# Test Project\n")
		te.GitCommit("Initial commit")
	}
)

// initializeDaggerOnce initializes Dagger client once for all tests
func initializeDaggerOnce(t *testing.T) {
	daggerOnce.Do(func() {
		if dag != nil {
			return
		}

		ctx := context.Background()
		client, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
		if err != nil {
			daggerErr = err
			return
		}

		err = Initialize(client)
		if err != nil {
			client.Close()
			daggerErr = err
			return
		}

		testDaggerClient = client
	})

	if daggerErr != nil {
		t.Skipf("Skipping test - Dagger not available: %v", daggerErr)
	}
}

// TestEnv provides simple helpers for testing environments
type TestEnv struct {
	t         *testing.T
	ctx       context.Context
	repoDir   string
	configDir string // Temporary config directory for test isolation
	env       *Environment
}

// NewTestEnv creates a test environment with a git repo.
// WARNING: This function modifies the global CONTAINER_USE_CONFIG_DIR environment
// variable which can cause race conditions with concurrent tests. Do not run tests
// using this helper in parallel.
func NewTestEnv(t *testing.T, name string) *TestEnv {
	ctx := context.Background()

	// Create temp directory for the git repo
	tmpDir, err := os.MkdirTemp("", "cu-test-"+name+"-*")
	require.NoError(t, err, "Failed to create temp dir")

	// Create temp directory for config (worktrees, repos)
	configDir, err := os.MkdirTemp("", "cu-test-config-"+name+"-*")
	require.NoError(t, err, "Failed to create config dir")

	// TODO: Design Limitation - Global CONTAINER_USE_CONFIG_DIR environment variable
	// Expected: Each test should have isolated config directories without affecting other tests
	// Actual: All environments in a process share the same CONTAINER_USE_CONFIG_DIR env var
	// This causes test interference when running concurrently as tests overwrite each other's config
	// Fix would require passing config dir as parameter to environment functions instead of using env var
	//
	// WARNING: This modifies a global environment variable which is NOT safe
	// for concurrent test execution. Tests using NewTestEnv should not run in parallel.
	oldConfigDir := os.Getenv("CONTAINER_USE_CONFIG_DIR")
	os.Setenv("CONTAINER_USE_CONFIG_DIR", configDir)

	// Initialize git repo
	cmds := [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test User"},
		{"config", "commit.gpgsign", "false"},
	}

	for _, cmd := range cmds {
		_, err := runGitCommand(ctx, tmpDir, cmd...)
		require.NoError(t, err, "Failed to run git %v", cmd)
	}

	te := &TestEnv{
		t:         t,
		ctx:       ctx,
		repoDir:   tmpDir,
		configDir: configDir,
	}

	t.Cleanup(func() {
		// Clean up environment if created
		if te.env != nil {
			te.env.Delete(context.Background())
		}

		// Remove directories
		os.RemoveAll(te.repoDir)
		if te.configDir != "" {
			os.RemoveAll(te.configDir)
		}

		// Restore original config dir
		if oldConfigDir == "" {
			os.Unsetenv("CONTAINER_USE_CONFIG_DIR")
		} else {
			os.Setenv("CONTAINER_USE_CONFIG_DIR", oldConfigDir)
		}
	})

	return te
}

// WriteFile creates a file in the repo
func (te *TestEnv) WriteFile(path, content string) {
	fullPath := filepath.Join(te.repoDir, path)
	dir := filepath.Dir(fullPath)

	err := os.MkdirAll(dir, 0755)
	require.NoError(te.t, err, "Failed to create dir")

	err = os.WriteFile(fullPath, []byte(content), 0644)
	require.NoError(te.t, err, "Failed to write file")
}

// WriteBinaryFile creates a binary file
func (te *TestEnv) WriteBinaryFile(path string, size int) {
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % 256)
	}

	fullPath := filepath.Join(te.repoDir, path)
	dir := filepath.Dir(fullPath)

	err := os.MkdirAll(dir, 0755)
	require.NoError(te.t, err, "Failed to create dir")

	err = os.WriteFile(fullPath, content, 0644)
	require.NoError(te.t, err, "Failed to write binary file")
}

// CreateDir creates an empty directory
func (te *TestEnv) CreateDir(path string) {
	fullPath := filepath.Join(te.repoDir, path)
	err := os.MkdirAll(fullPath, 0755)
	require.NoError(te.t, err, "Failed to create directory")
}

// GitCommit commits all changes
func (te *TestEnv) GitCommit(message string) {
	runGitCommand(te.ctx, te.repoDir, "add", ".")
	_, err := runGitCommand(te.ctx, te.repoDir, "commit", "-m", message)
	require.NoError(te.t, err, "Failed to commit")
}

// GitStatus returns the current git status
func (te *TestEnv) GitStatus() string {
	status, err := runGitCommand(te.ctx, te.repoDir, "status", "--porcelain")
	require.NoError(te.t, err, "Failed to get status")
	return status
}

// RunInEnv runs a command in the environment
func (te *TestEnv) RunInEnv(command string) (string, error) {
	require.NotNil(te.t, te.env, "No environment created")
	return te.env.Run(te.ctx, "Test command", command, "/bin/sh", false)
}

// WriteFileInEnv writes a file through the environment
func (te *TestEnv) WriteFileInEnv(path, content string) error {
	require.NotNil(te.t, te.env, "No environment created")
	return te.env.FileWrite(te.ctx, "Test write", path, content)
}

// Common test scenarios

// SetupPythonProject creates a typical Python project
func (te *TestEnv) SetupPythonProject() {
	te.SetupPythonProjectWithOptions(true)
}

// SetupPythonProjectWithOptions creates a Python project with optional .gitignore
func (te *TestEnv) SetupPythonProjectWithOptions(includeGitignore bool) {
	te.WriteFile("main.py", "def main():\n    print('Hello World')\n\nif __name__ == '__main__':\n    main()\n")
	te.WriteFile("utils.py", "def helper():\n    return 42\n")
	te.WriteFile("requirements.txt", "requests==2.31.0\nnumpy==1.24.0\n")
	if includeGitignore {
		te.WriteFile(".gitignore", "__pycache__/\n*.pyc\n.env\nvenv/\n")
	}
	te.GitCommit("Initial Python project")
}

// SetupNodeProject creates a typical Node.js project
func (te *TestEnv) SetupNodeProject() {
	packageJSON := `{
  "name": "test-project",
  "version": "1.0.0",
  "main": "index.js",
  "scripts": {
    "start": "node index.js",
    "test": "jest"
  },
  "dependencies": {
    "express": "^4.18.0"
  }
}`

	te.WriteFile("package.json", packageJSON)
	te.WriteFile("index.js", "console.log('Hello from Node.js');\n")
	te.WriteFile(".gitignore", "node_modules/\n.env\n")
	te.GitCommit("Initial Node project")
}

// Common verification helpers
type verifier struct {
	t   *testing.T
	ctx context.Context
	env *Environment
}

func newVerifier(t *testing.T, env *Environment) *verifier {
	return &verifier{t: t, ctx: context.Background(), env: env}
}

func (v *verifier) fileExists(path, expectedContent string) {
	content, err := v.env.FileRead(v.ctx, path, true, 0, 0)
	require.NoError(v.t, err, "File %s should exist", path)
	assert.Contains(v.t, content, expectedContent, "File %s should contain expected content", path)
}

func (v *verifier) fileNotExists(path string) {
	_, err := v.env.FileRead(v.ctx, path, true, 0, 0)
	assert.Error(v.t, err, "File %s should not exist", path)
}

func (v *verifier) commandOutputContains(cmd, expected string) {
	output, err := v.env.Run(v.ctx, "Test command", cmd, "/bin/sh", false)
	require.NoError(v.t, err)
	assert.Contains(v.t, output, expected)
}

func (v *verifier) gitLogContains(pattern string) {
	output, err := runGitCommand(v.ctx, v.env.Worktree, "log", "--oneline")
	require.NoError(v.t, err)
	assert.Contains(v.t, output, pattern)
}
