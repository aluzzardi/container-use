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
	"slices"
	"strings"

	"dagger.io/dagger"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/mitchellh/go-homedir"
)

const (
	containerUseRemote = "container-use"
	gitNotesLogRef     = "container-use"
	gitNotesStateRef   = "container-use-state"
)

// 10MB
const maxFileSizeForTextCheck = 10 * 1024 * 1024

func getRepoPath(repoName string) (string, error) {
	return homedir.Expand(fmt.Sprintf(
		"~/.config/container-use/repos/%s",
		filepath.Base(repoName),
	))
}

func (env *Environment) GetWorktreePath() (string, error) {
	return homedir.Expand(fmt.Sprintf("~/.config/container-use/worktrees/%s", env.ID))
}

func (env *Environment) DeleteWorktree() error {
	worktreePath, err := env.GetWorktreePath()
	if err != nil {
		return err
	}
	
	// With go-git's shared storage approach, we just need to remove the worktree directory
	// The storage is shared with the bare repo, so no object cleanup needed
	parentDir := filepath.Dir(worktreePath)
	fmt.Printf("Deleting worktree directory at %s\n", parentDir)
	return os.RemoveAll(parentDir)
}

func (env *Environment) DeleteLocalRemoteBranch() error {
	localRepoPath, err := filepath.Abs(env.Source)
	if err != nil {
		slog.Error("Failed to get absolute path for local repo", "source", env.Source, "err", err)
		return err
	}
	repoName := filepath.Base(localRepoPath)
	cuRepoPath, err := getRepoPath(repoName)
	if err != nil {
		return err
	}

	// Open container-use repo for worktree operations
	cuRepo, err := git.PlainOpen(cuRepoPath)
	if err != nil {
		return err
	}

	// Prune worktrees - keep as shell-out since it involves cleanup of worktree metadata
	slog.Info("Pruning git worktrees", "repo", cuRepoPath)
	if _, err = runGitCommand(context.Background(), cuRepoPath, "worktree", "prune"); err != nil {
		slog.Error("Failed to prune git worktrees", "repo", cuRepoPath, "err", err)
		return err
	}

	// Delete local branch
	slog.Info("Deleting local branch", "repo", cuRepoPath, "branch", env.ID)
	branchRef := plumbing.ReferenceName(fmt.Sprintf("refs/heads/%s", env.ID))
	err = cuRepo.Storer.RemoveReference(branchRef)
	if err != nil {
		slog.Error("Failed to delete local branch", "repo", cuRepoPath, "branch", env.ID, "err", err)
		return err
	}

	// Open local repo for remote operations
	localRepo, err := git.PlainOpen(localRepoPath)
	if err != nil {
		return err
	}

	// Prune remote
	remote, err := localRepo.Remote(containerUseRemote)
	if err != nil {
		slog.Error("Failed to get remote", "remote", containerUseRemote, "err", err)
		return err
	}

	err = remote.Fetch(&git.FetchOptions{
		Prune: true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		slog.Error("Failed to fetch and prune container-use remote", "local-repo", localRepoPath, "err", err)
		return err
	}

	return nil
}

func (env *Environment) InitializeWorktree(ctx context.Context, localRepoPath string) (string, error) {
	localRepoPath, err := filepath.Abs(localRepoPath)
	if err != nil {
		return "", err
	}

	cuRepoPath, err := InitializeLocalRemote(ctx, localRepoPath)
	if err != nil {
		return "", err
	}

	worktreePath, err := env.GetWorktreePath()
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(worktreePath); err == nil {
		return worktreePath, nil
	}

	slog.Info("Initializing worktree", "container-id", env.ID, "container-name", env.Name, "id", env.ID)

	// Open local repo
	localRepo, err := git.PlainOpen(localRepoPath)
	if err != nil {
		return "", err
	}

	// Fetch from container-use remote
	remote, err := localRepo.Remote(containerUseRemote)
	if err != nil {
		return "", err
	}

	err = remote.FetchContext(ctx, &git.FetchOptions{})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return "", err
	}

	// Get current branch
	head, err := localRepo.Head()
	if err != nil {
		return "", err
	}
	currentBranch := head.Name().Short()

	// Push current branch to container-use remote (force push)
	err = localRepo.PushContext(ctx, &git.PushOptions{
		RemoteName: containerUseRemote,
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("+%s:%s", head.Name(), head.Name())),
		},
		Force: true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return "", err
	}

	// Create worktree using git command (complex operation)
	// Note: The go-git approach with separate storage/worktree filesystems
	// can cause issues with bare repository object access, so we use git command
	cuRepo, err := git.PlainOpen(cuRepoPath)
	if err != nil {
		return "", err
	}

	// Check if branch exists
	branchRef := plumbing.ReferenceName(fmt.Sprintf("refs/heads/%s", env.ID))
	_, err = cuRepo.Reference(branchRef, true)
	if err != nil {
		// Branch doesn't exist, create worktree with new branch
		_, err = runGitCommand(ctx, cuRepoPath, "worktree", "add", "-b", env.ID, worktreePath, currentBranch)
		if err != nil {
			return "", err
		}
	} else {
		// Branch exists, create worktree with existing branch
		_, err = runGitCommand(ctx, cuRepoPath, "worktree", "add", worktreePath, env.ID)
		if err != nil {
			return "", err
		}
	}

	if err := env.applyUncommittedChanges(ctx, localRepoPath, worktreePath); err != nil {
		return "", fmt.Errorf("failed to apply uncommitted changes: %w", err)
	}

	// Fetch the environment branch to local repo
	err = remote.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/remotes/%s/%s", env.ID, containerUseRemote, env.ID)),
		},
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return "", err
	}

	// Set up tracking branch if not already there
	envBranchRef := plumbing.ReferenceName(fmt.Sprintf("refs/heads/%s", env.ID))
	_, err = localRepo.Reference(envBranchRef, true)
	if err != nil {
		// Create tracking branch
		remoteBranchRef := plumbing.ReferenceName(fmt.Sprintf("refs/remotes/%s/%s", containerUseRemote, env.ID))
		remoteRef, err := localRepo.Reference(remoteBranchRef, true)
		if err != nil {
			return "", err
		}

		newRef := plumbing.NewHashReference(envBranchRef, remoteRef.Hash())
		err = localRepo.Storer.SetReference(newRef)
		if err != nil {
			return "", err
		}

		// Set up tracking configuration
		cfg, err := localRepo.Config()
		if err != nil {
			return "", err
		}

		cfg.Branches[env.ID] = &config.Branch{
			Name:   env.ID,
			Remote: containerUseRemote,
			Merge:  envBranchRef,
		}

		localRepo.Storer.SetConfig(cfg)
	}

	return worktreePath, nil
}

func InitializeLocalRemote(ctx context.Context, localRepoPath string) (string, error) {
	localRepoPath, err := filepath.Abs(localRepoPath)
	if err != nil {
		return "", err
	}

	repoName := filepath.Base(localRepoPath)
	cuRepoPath, err := getRepoPath(repoName)
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(cuRepoPath); err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}

		slog.Info("Initializing local remote", "local-repo-path", localRepoPath, "container-use-repo-path", cuRepoPath)
		
		// Clone as bare repository
		_, err = git.PlainClone(cuRepoPath, true, &git.CloneOptions{
			URL: localRepoPath,
		})
		if err != nil {
			return "", err
		}
	}

	// Open local repo
	localRepo, err := git.PlainOpen(localRepoPath)
	if err != nil {
		return "", err
	}

	// Check if remote exists
	_, err = localRepo.Remote(containerUseRemote)
	if err != nil {
		// Remote doesn't exist, create it
		_, err = localRepo.CreateRemote(&config.RemoteConfig{
			Name: containerUseRemote,
			URLs: []string{cuRepoPath},
		})
		if err != nil {
			return "", err
		}
	} else {
		// Remote exists, update URL if necessary
		cfg, err := localRepo.Config()
		if err != nil {
			return "", err
		}

		remoteCfg := cfg.Remotes[containerUseRemote]
		if len(remoteCfg.URLs) == 0 || remoteCfg.URLs[0] != cuRepoPath {
			remoteCfg.URLs = []string{cuRepoPath}
			err = localRepo.Storer.SetConfig(cfg)
			if err != nil {
				return "", err
			}
		}
	}

	return cuRepoPath, nil
}

// Keep git command runner for complex operations
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

func (env *Environment) propagateToWorktree(ctx context.Context, name, explanation string) (rerr error) {
	slog.Info("Propagating to worktree...",
		"environment.id", env.ID,
		"environment.name", env.Name,
		"workdir", env.Workdir,
		"id", env.ID)
	defer func() {
		slog.Info("Propagating to worktree... (DONE)",
			"environment.id", env.ID,
			"environment.name", env.Name,
			"workdir", env.Workdir,
			"id", env.ID,
			"err", rerr)
	}()

	worktreePath, err := env.GetWorktreePath()
	if err != nil {
		return err
	}

	_, err = env.container.Directory(env.Workdir).Export(
		ctx,
		worktreePath,
		dagger.DirectoryExportOpts{Wipe: true},
	)
	if err != nil {
		return err
	}

	slog.Info("Saving environment")
	if err := env.save(worktreePath); err != nil {
		return err
	}

	if err := env.commitWorktreeChanges(ctx, worktreePath, name, explanation); err != nil {
		return fmt.Errorf("failed to commit worktree changes: %w", err)
	}

	if err := env.commitStateToNotes(ctx); err != nil {
		return fmt.Errorf("failed to add notes: %w", err)
	}

	localRepoPath, err := filepath.Abs(env.Source)
	if err != nil {
		return err
	}

	slog.Info("Fetching container-use remote in source repository")
	localRepo, err := git.PlainOpen(localRepoPath)
	if err != nil {
		return err
	}

	remote, err := localRepo.Remote(containerUseRemote)
	if err != nil {
		return err
	}

	err = remote.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/remotes/%s/%s", env.ID, containerUseRemote, env.ID)),
		},
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return err
	}

	if err := env.propagateGitNotes(ctx, gitNotesStateRef); err != nil {
		return err
	}

	return nil
}

// Keep git notes operations as shell-outs (complex)
func (env *Environment) propagateGitNotes(ctx context.Context, ref string) error {
	fullRef := fmt.Sprintf("refs/notes/%s", ref)
	fetch := func() error {
		_, err := runGitCommand(ctx, env.Source, "fetch", containerUseRemote, fullRef+":"+fullRef)
		return err
	}

	if err := fetch(); err != nil {
		if strings.Contains(err.Error(), "[rejected]") {
			if _, err := runGitCommand(ctx, env.Source, "update-ref", "-d", fullRef); err == nil {
				return fetch()
			}
		}
		return err
	}
	return nil
}

func (env *Environment) commitStateToNotes(ctx context.Context) error {
	buff, err := json.MarshalIndent(env.History, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(os.TempDir(), ".container-use-git-notes-*")
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(buff); err != nil {
		return err
	}

	_, err = runGitCommand(ctx, env.Worktree, "notes", "--ref", gitNotesStateRef, "add", "-f", "-F", f.Name())
	if err != nil {
		return err
	}
	return nil
}

func (env *Environment) addGitNote(ctx context.Context, note string) error {
	_, err := runGitCommand(ctx, env.Worktree, "notes", "--ref", gitNotesLogRef, "append", "-m", note)
	if err != nil {
		return err
	}
	return env.propagateGitNotes(ctx, gitNotesLogRef)
}

func StateFromCommit(ctx context.Context, repoDir, commit string) (History, error) {
	buff, err := runGitCommand(ctx, repoDir, "notes", "--ref", gitNotesStateRef, "show")
	if err != nil {
		return nil, err
	}

	var history History
	if err := json.Unmarshal([]byte(buff), &history); err != nil {
		return nil, err
	}
	return history, nil
}

func (env *Environment) loadStateFromNotes(ctx context.Context, worktreePath string) error {
	buff, err := runGitCommand(ctx, worktreePath, "notes", "--ref", gitNotesStateRef, "show")
	if err != nil {
		if strings.Contains(err.Error(), "no note found") {
			return nil
		}
		return err
	}
	return json.Unmarshal([]byte(buff), &env.History)
}

func (env *Environment) commitWorktreeChanges(ctx context.Context, worktreePath, name, explanation string) error {
	status, err := runGitCommand(ctx, worktreePath, "status", "--porcelain")
	if err != nil {
		return err
	}

	if strings.TrimSpace(status) == "" {
		return nil
	}

	if err := env.addNonBinaryFiles(ctx, worktreePath); err != nil {
		return err
	}

	commitMsg := fmt.Sprintf("%s\n\n%s", name, explanation)
	_, err = runGitCommand(ctx, worktreePath, "commit", "-m", commitMsg)
	return err
}

func (env *Environment) addNonBinaryFiles(ctx context.Context, worktreePath string) error {
	statusOutput, err := runGitCommand(ctx, worktreePath, "status", "--porcelain")
	if err != nil {
		return err
	}

	lines := strings.Split(strings.TrimSpace(statusOutput), "\n")

	for _, line := range lines {
		if line == "" {
			continue
		}
		if len(line) < 3 {
			continue
		}

		indexStatus := line[0]
		workTreeStatus := line[1]
		fileName := strings.TrimSpace(line[2:])
		if fileName == "" {
			continue
		}

		if env.shouldSkipFile(fileName) {
			continue
		}

		switch {
		case indexStatus == '?' && workTreeStatus == '?':
			// ?? = untracked files or directories
			if strings.HasSuffix(fileName, "/") {
				// Untracked directory - traverse and add non-binary files
				dirName := strings.TrimSuffix(fileName, "/")
				if err := env.addFilesFromUntrackedDirectory(ctx, worktreePath, dirName); err != nil {
					return err
				}
			} else {
				// Untracked file - add if not binary
				if !env.isBinaryFile(worktreePath, fileName) {
					_, err = runGitCommand(ctx, worktreePath, "add", fileName)
					if err != nil {
						return err
					}
				}
			}
		case indexStatus == 'A':
			// A = already staged, skip
			continue
		case indexStatus == 'D' || workTreeStatus == 'D':
			// D = deleted files (always stage deletion)
			_, err = runGitCommand(ctx, worktreePath, "add", fileName)
			if err != nil {
				return err
			}
		default:
			// M, R, C and other statuses - add if not binary
			if !env.isBinaryFile(worktreePath, fileName) {
				_, err = runGitCommand(ctx, worktreePath, "add", fileName)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (env *Environment) shouldSkipFile(fileName string) bool {
	skipExtensions := []string{
		".tar", ".tar.gz", ".tgz", ".tar.bz2", ".tbz2", ".tar.xz", ".txz",
		".zip", ".rar", ".7z", ".gz", ".bz2", ".xz",
		".exe", ".bin", ".dmg", ".pkg", ".msi",
		".jpg", ".jpeg", ".png", ".gif", ".bmp", ".tiff", ".svg",
		".mp3", ".mp4", ".avi", ".mov", ".wmv", ".flv", ".mkv",
		".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
		".so", ".dylib", ".dll", ".a", ".lib",
	}

	lowerName := strings.ToLower(fileName)
	for _, ext := range skipExtensions {
		if strings.HasSuffix(lowerName, ext) {
			return true
		}
	}

	skipPatterns := []string{
		"node_modules/", ".git/", "__pycache__/", ".DS_Store",
		"venv/", ".venv/", "env/", ".env/",
		"target/", "build/", "dist/", ".next/",
		"*.tmp", "*.temp", "*.cache", "*.log",
	}

	for _, pattern := range skipPatterns {
		if strings.Contains(lowerName, strings.ToLower(pattern)) {
			return true
		}
	}

	return false
}

// Keep patch application as shell-out (complex)
func (env *Environment) applyUncommittedChanges(ctx context.Context, localRepoPath, worktreePath string) error {
	// Check for uncommitted changes using go-git
	localRepo, err := git.PlainOpen(localRepoPath)
	if err != nil {
		return err
	}

	worktree, err := localRepo.Worktree()
	if err != nil {
		return err
	}

	status, err := worktree.Status()
	if err != nil {
		return err
	}

	if status.IsClean() {
		return nil
	}

	slog.Info("Applying uncommitted changes to worktree", "container-id", env.ID, "container-name", env.Name)

	// Use git command for patch generation and application (complex)
	patch, err := runGitCommand(ctx, localRepoPath, "diff", "HEAD")
	if err != nil {
		return err
	}

	if strings.TrimSpace(patch) != "" {
		cmd := exec.Command("git", "apply")
		cmd.Dir = worktreePath
		cmd.Stdin = strings.NewReader(patch)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to apply patch: %w", err)
		}
	}

	untrackedFiles, err := runGitCommand(ctx, localRepoPath, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return err
	}

	for _, file := range strings.Split(strings.TrimSpace(untrackedFiles), "\n") {
		if file == "" {
			continue
		}
		srcPath := filepath.Join(localRepoPath, file)
		destPath := filepath.Join(worktreePath, file)

		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}

		if err := exec.Command("cp", "-r", srcPath, destPath).Run(); err != nil {
			return fmt.Errorf("failed to copy untracked file %s: %w", file, err)
		}
	}

	return env.commitWorktreeChanges(ctx, worktreePath, "Copy uncommitted changes", "Applied uncommitted changes from local repository")
}

func (env *Environment) addFilesFromUntrackedDirectory(ctx context.Context, worktreePath, dirName string) error {
	dirPath := filepath.Join(worktreePath, dirName)

	return filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(worktreePath, path)
		if err != nil {
			return err
		}

		if info.IsDir() {
			if env.shouldSkipFile(relPath + "/") {
				return filepath.SkipDir
			}
			return nil
		}

		if env.shouldSkipFile(relPath) {
			return nil
		}

		if !env.isBinaryFile(worktreePath, relPath) {
			_, err = runGitCommand(ctx, worktreePath, "add", relPath)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

func (env *Environment) isBinaryFile(worktreePath, fileName string) bool {
	fullPath := filepath.Join(worktreePath, fileName)

	stat, err := os.Stat(fullPath)
	if err != nil {
		return true
	}

	if stat.IsDir() {
		return false
	}

	if stat.Size() > maxFileSizeForTextCheck {
		return true
	}

	file, err := os.Open(fullPath)
	if err != nil {
		slog.Error("Error opening file", "err", err)
		return true
	}
	defer file.Close()

	buffer := make([]byte, 8000)
	n, err := file.Read(buffer)
	if err != nil && n == 0 {
		return true
	}

	buffer = buffer[:n]
	if slices.Contains(buffer, 0) {
		return true
	}

	return false
}