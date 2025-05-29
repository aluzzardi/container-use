package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type GitState struct {
	IsRepository  bool      `json:"is_repository"`
	RootPath      string    `json:"root_path"`
	CurrentBranch string    `json:"current_branch"`
	CurrentCommit string    `json:"current_commit"`
	RemoteURL     string    `json:"remote_url"`
	CapturedAt    time.Time `json:"captured_at"`
}

func IsGitRepository() bool {
	_, err := exec.Command("git", "rev-parse", "--git-dir").Output()
	return err == nil
}

func GetGitState() (*GitState, error) {
	if !IsGitRepository() {
		return &GitState{
			IsRepository: false,
			CapturedAt:   time.Now(),
		}, nil
	}

	state := &GitState{
		IsRepository: true,
		CapturedAt:   time.Now(),
	}

	if rootPath, err := runGitCommand("rev-parse", "--show-toplevel"); err == nil {
		state.RootPath = strings.TrimSpace(rootPath)
	}

	if branch, err := runGitCommand("branch", "--show-current"); err == nil {
		state.CurrentBranch = strings.TrimSpace(branch)
	}

	if commit, err := runGitCommand("rev-parse", "HEAD"); err == nil {
		state.CurrentCommit = strings.TrimSpace(commit)
	}

	if remoteURL, err := runGitCommand("remote", "get-url", "origin"); err == nil {
		state.RemoteURL = strings.TrimSpace(remoteURL)
	}

	return state, nil
}

func runGitCommand(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func hasUncommittedChanges() bool {
	output, err := runGitCommand("status", "--porcelain")
	if err != nil {
		return false
	}
	return strings.TrimSpace(output) != ""
}

func CreateGitArchive(outputPath string) error {
	if !IsGitRepository() {
		return fmt.Errorf("not in a git repository")
	}

	cmd := exec.Command("git", "archive", "--format=tar", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to create git archive: %v", err)
	}

	return os.WriteFile(outputPath, output, 0644)
}

func GetGitWorkdir() (string, error) {
	if !IsGitRepository() {
		return "", fmt.Errorf("not in a git repository")
	}

	rootPath, err := runGitCommand("rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	rootPath = strings.TrimSpace(rootPath)

	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	relPath, err := filepath.Rel(rootPath, cwd)
	if err != nil {
		return "", err
	}

	if relPath == "." {
		return "", nil
	}
	return relPath, nil
}

// CreateGitBundle creates a git bundle that includes current branch state
func CreateGitBundle(outputPath string) error {
	if !IsGitRepository() {
		return fmt.Errorf("not in a git repository")
	}

	// Create the bundle
	cmd := exec.Command("git", "bundle", "create", outputPath, "HEAD")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to create git bundle: %v\nOutput: %s", err, string(output))
	}

	return nil
}

