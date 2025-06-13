package environment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	containerUseRemote = "container-use"
	gitNotesLogRef     = "container-use"
	gitNotesStateRef   = "container-use-state"
)

func (env *Environment) SetupTrackingBranch(ctx context.Context, localRepoPath string) error {
	localRepoPath, err := filepath.Abs(localRepoPath)
	if err != nil {
		return err
	}

	// Check if localRepoPath is a git repository, initialize if not
	if _, err := os.Stat(filepath.Join(localRepoPath, ".git")); os.IsNotExist(err) {
		slog.Info("Initializing git repository", "path", localRepoPath)
		_, err = runGitCommand(ctx, localRepoPath, "init")
		if err != nil {
			return err
		}

		// Create an initial commit if the repo is empty
		_, err = runGitCommand(ctx, localRepoPath, "commit", "--allow-empty", "-m", "Initial commit")
		if err != nil {
			return err
		}
	}

	remoteUrl := storage.RemoteUrl(localRepoPath)
	if remoteUrl == "" {
		return fmt.Errorf("failed to initialize remote storage for project: %s", localRepoPath)
	}

	// Extract the repository path from the file:// URL
	cuRepoPath := strings.TrimPrefix(remoteUrl, "file://")

	// Set up remote in source repo pointing to storage
	existingURL, err := runGitCommand(ctx, localRepoPath, "remote", "get-url", containerUseRemote)
	if err != nil {
		_, err = runGitCommand(ctx, localRepoPath, "remote", "add", containerUseRemote, cuRepoPath)
		if err != nil {
			return err
		}
	} else {
		existingURL = strings.TrimSpace(existingURL)
		if existingURL != cuRepoPath {
			_, err = runGitCommand(ctx, localRepoPath, "remote", "set-url", containerUseRemote, cuRepoPath)
			if err != nil {
				return err
			}
		}
	}

	if err := env.createTrackingBranch(ctx, localRepoPath); err != nil {
		return err
	}

	return nil
}

func (env *Environment) createTrackingBranch(ctx context.Context, localRepoPath string) error {
	_, err := runGitCommand(ctx, localRepoPath, "ls-remote", "--exit-code", containerUseRemote, env.ID)
	if err != nil {
		slog.Info("Setting up tracking branch", "environment", env.ID, "branch", env.ID)
		currentBranch, err := runGitCommand(ctx, localRepoPath, "branch", "--show-current")
		if err != nil {
			return err
		}

		currentBranch = strings.TrimSpace(currentBranch)
		if currentBranch == "" {
			return fmt.Errorf("no current branch found")
		}

		_, err = runGitCommand(ctx, localRepoPath, "push", containerUseRemote, fmt.Sprintf("%s:%s", currentBranch, env.ID))
		if err != nil {
			return err
		}
	}

	slog.Info("Syncing remote ref", "environment", env.ID, "branch", env.ID)
	_, err = runGitCommand(ctx, localRepoPath, "fetch", containerUseRemote, env.ID)
	if err != nil {
		return err
	}

	// // Set up the tracking branch locally (idempotently)
	// remoteBranch := fmt.Sprintf("%s/%s", containerUseRemote, env.ID)

	// // Check if local branch already exists
	// _, err = runGitCommand(ctx, localRepoPath, "rev-parse", "--verify", env.ID)
	// if err != nil {
	// 	// Local branch doesn't exist, create it with tracking
	// 	_, err = runGitCommand(ctx, localRepoPath, "branch", "--track", env.ID, remoteBranch)
	// 	if err != nil {
	// 		return err
	// 	}
	// } else {
	// 	// Local branch exists, set up tracking without deleting it
	// 	_, err = runGitCommand(ctx, localRepoPath, "branch", "--set-upstream-to", remoteBranch, env.ID)
	// 	if err != nil {
	// 		return err
	// 	}
	// }

	return nil
}

func (env *Environment) PropagateToTrackedBranch(ctx context.Context, name, explanation string) error {
	slog.Info("Propagating to tracked environment branch", "environment", env.ID, "branch", env.ID)

	localRepoPath, err := filepath.Abs(env.source)
	if err != nil {
		return err
	}

	if err := storage.Save(env, name, explanation); err != nil {
		return err
	}

	if err := env.fetchStorage(ctx, localRepoPath); err != nil {
		return err
	}

	// Propagate both state and log notes to source repo
	if err := env.fetchGitNotes(ctx, gitNotesStateRef); err != nil {
		return err
	}

	if err := env.fetchGitNotes(ctx, gitNotesLogRef); err != nil {
		return err
	}

	return nil
}

func (env *Environment) DeleteTrackingBranch() error {
	slog.Info("Deleting tracking branch", "environment", env.ID, "branch", env.ID)

	localRepoPath, err := filepath.Abs(env.source)
	if err != nil {
		return err
	}

	ctx := context.Background()

	// Delete the remote tracking branch
	_, err = runGitCommand(ctx, localRepoPath, "push", containerUseRemote, "--delete", env.ID)
	if err != nil {
		slog.Warn("Failed to delete remote tracking branch", "err", err)
	}

	// Delete local tracking branch
	_, err = runGitCommand(ctx, localRepoPath, "branch", "-D", env.ID)
	if err != nil {
		slog.Warn("Failed to delete local tracking branch", "err", err)
	}

	return nil
}

func (env *Environment) fetchGitNotes(ctx context.Context, ref string) error {
	fullRef := fmt.Sprintf("refs/notes/%s", ref)
	fetch := func() error {
		_, err := runGitCommand(ctx, env.source, "fetch", containerUseRemote, fullRef+":"+fullRef)
		return err
	}

	if err := fetch(); err != nil {
		if strings.Contains(err.Error(), "[rejected]") {
			if _, err := runGitCommand(ctx, env.source, "update-ref", "-d", fullRef); err == nil {
				return fetch()
			}
		}
		return err
	}
	return nil
}

func (env *Environment) addGitNote(ctx context.Context, note string) error {
	return storage.Note(env, note)
}

func StateFromCommit(ctx context.Context, localRepoPath, commitHash string) (History, error) {
	notes, err := runGitCommand(ctx, localRepoPath, "notes", "--ref", gitNotesStateRef, "show", commitHash)
	if err != nil {
		return nil, err
	}

	var history History
	if err := json.Unmarshal([]byte(notes), &history); err != nil {
		return nil, err
	}

	return history, nil
}

func (env *Environment) loadStateFromNotes(ctx context.Context, localRepoPath string) error {
	history, err := StateFromCommit(ctx, localRepoPath, "HEAD")
	if err != nil {
		return err
	}
	env.History = history
	return nil
}

func (env *Environment) GeneratePatch(ctx context.Context) (string, error) {
	localRepoPath, err := filepath.Abs(env.source)
	if err != nil {
		return "", err
	}

	status, err := runGitCommand(ctx, localRepoPath, "status", "--porcelain")
	if err != nil {
		return "", err
	}

	slog.Debug("Git status output", "status", status)

	if strings.TrimSpace(status) == "" {
		slog.Debug("No changes detected, returning empty patch")
		return "", nil
	}

	slog.Info("Generating patch from uncommitted changes")

	// Check if repository has any commits
	_, err = runGitCommand(ctx, localRepoPath, "rev-parse", "HEAD")
	if err != nil {
		slog.Debug("No HEAD commit found, skipping diff generation")
		return "", nil
	}

	// Generate diff patch for tracked changes
	patch, err := runGitCommand(ctx, localRepoPath, "diff", "HEAD")
	if err != nil {
		return "", err
	}

	slog.Debug("Generated diff patch", "patch_length", len(patch))
	return patch, nil
}

func (env *Environment) fetchStorage(ctx context.Context, localRepoPath string) error {
	slog.Info("Fetching tracking branch from storage to source repository")
	_, err := runGitCommand(ctx, localRepoPath, "fetch", containerUseRemote, env.ID)
	return err
}

func runGitCommand(ctx context.Context, dir string, args ...string) (out string, rerr error) {
	slog.Info(fmt.Sprintf("[%s] $ git %s", dir, strings.Join(args, " ")))
	defer func() {
		slog.Info(fmt.Sprintf("[%s] $ git %s (DONE)", dir, strings.Join(args, " ")), "err", rerr)
	}()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir

	output, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("git command failed (exit code %d): %w\nOutput: %s",
				exitErr.ExitCode(), err, string(output))
		}
		return "", fmt.Errorf("git command failed: %w", err)
	}

	return string(output), nil
}
