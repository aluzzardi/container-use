package repository

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/dagger/container-use/environment"
	petname "github.com/dustinkirkland/golang-petname"
)

const (
	cuGlobalConfigPath = "~/.config/container-use"
	cuRepoPath         = cuGlobalConfigPath + "/repos"
	cuWorktreePath     = cuGlobalConfigPath + "/worktrees"
	containerUseRemote = "container-use"
	gitNotesLogRef     = "container-use"
	gitNotesStateRef   = "container-use-state"
)

type Repository struct {
	userRepoPath string
	forkRepoPath string
}

func Open(ctx context.Context, repo string) (*Repository, error) {
	output, err := runGitCommand(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		if strings.Contains(err.Error(), "not a git repository") {
			return nil, errors.New("you must be in a git repository to use container-use")
		}
		return nil, err
	}
	userRepoPath := strings.TrimSpace(output)

	forkRepoPath, err := getContainerUseRemote(ctx, userRepoPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		forkRepoPath, err = normalizeForkPath(ctx, userRepoPath)
		if err != nil {
			return nil, err
		}
	}

	r := &Repository{
		userRepoPath: userRepoPath,
		forkRepoPath: forkRepoPath,
	}

	if err := r.ensureFork(ctx); err != nil {
		return nil, fmt.Errorf("unable to fork the repository: %w", err)
	}
	if err := r.ensureLocalRemote(ctx); err != nil {
		return nil, fmt.Errorf("unable to set container-use remote: %w", err)
	}

	return r, nil
}

func (r *Repository) ensureFork(ctx context.Context) error {
	// Make sure the fork repo path exists, otherwise create it
	_, err := os.Stat(r.forkRepoPath)
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}

	slog.Info("Initializing local remote", "user-repo", r.userRepoPath, "fork-repo", r.forkRepoPath)
	if err := os.MkdirAll(r.forkRepoPath, 0755); err != nil {
		return err
	}
	_, err = runGitCommand(ctx, r.userRepoPath, "clone", "--bare", r.userRepoPath, r.forkRepoPath)
	if err != nil {
		return err
	}
	return nil
}

func (r *Repository) ensureLocalRemote(ctx context.Context) error {
	currentForkPath, err := getContainerUseRemote(ctx, r.userRepoPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		_, err := runGitCommand(ctx, r.userRepoPath, "remote", "add", containerUseRemote, r.forkRepoPath)
		return err
	}

	if currentForkPath != r.forkRepoPath {
		_, err := runGitCommand(ctx, r.userRepoPath, "remote", "set-url", containerUseRemote, r.forkRepoPath)
		return err
	}

	return nil
}

func (r *Repository) SourcePath() string {
	return r.userRepoPath
}

func (r *Repository) exists(ctx context.Context, id string) error {
	if _, err := runGitCommand(ctx, r.forkRepoPath, "rev-parse", "--verify", id); err != nil {
		if strings.Contains(err.Error(), "Needed a single revision") {
			return fmt.Errorf("environment %q not found", id)
		}
		return err
	}
	return nil
}

func (r *Repository) Get(ctx context.Context, id string) (*environment.Environment, error) {
	if err := r.exists(ctx, id); err != nil {
		return nil, err
	}

	name, _, _ := strings.Cut(id, "/")
	worktree, err := r.initializeWorktree(ctx, id)
	if err != nil {
		return nil, err
	}

	state, err := r.loadState(ctx, worktree)
	if err != nil {
		return nil, err
	}

	env, err := environment.Load(ctx, id, name, state, worktree)
	if err != nil {
		return nil, err
	}

	return env, nil
}

func (r *Repository) Create(ctx context.Context, name, description, explanation string) (*environment.Environment, error) {
	id := fmt.Sprintf("%s/%s", name, petname.Generate(2, "-"))
	worktree, err := r.initializeWorktree(ctx, id)
	if err != nil {
		return nil, err
	}

	env, err := environment.New(ctx, id, name, description, worktree)
	if err != nil {
		return nil, err
	}

	if err := r.propagateToWorktree(ctx, env, "Create env "+name, explanation); err != nil {
		return nil, err
	}

	return env, nil
}

func (r *Repository) Update(ctx context.Context, env *environment.Environment, operation, explanation string) error {
	if err := r.addGitNote(ctx, env, env.Notes.Pop()); err != nil {
		return err
	}
	return r.propagateToWorktree(ctx, env, operation, explanation)
}

func (r *Repository) List(ctx context.Context) ([]*environment.Environment, error) {
	branches, err := runGitCommand(ctx, r.forkRepoPath, "branch", "--format", "%(refname:short)")
	if err != nil {
		return nil, err
	}

	envs := []*environment.Environment{}
	for _, branch := range strings.Split(branches, "\n") {
		branch = strings.TrimSpace(branch)
		// FIXME(aluzzardi): This logic is broken
		if !strings.Contains(branch, "/") {
			continue
		}

		env, err := r.Get(ctx, branch)
		if err != nil {
			return nil, err
		}

		envs = append(envs, env)
	}

	return envs, nil
}

func (r *Repository) Delete(ctx context.Context, id string) error {
	if err := r.exists(ctx, id); err != nil {
		return err
	}

	if err := r.deleteWorktree(id); err != nil {
		return err
	}
	if err := r.deleteLocalRemoteBranch(id); err != nil {
		return err
	}
	return nil
}
