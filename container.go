package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"dagger.io/dagger"
	"github.com/google/uuid"
)

const (
	AlpineImage = "alpine:3.21.3@sha256:a8560b36e8b8210634f77d9f7f9efd7ffa463e380b75e2e74aff4511df3ef88c"
)

type Version int

type Revision struct {
	Version     Version   `json:"version"`
	Name        string    `json:"name"`
	Explanation string    `json:"explanation"`
	CreatedAt   time.Time `json:"created_at"`

	state *dagger.Container
}

type History []*Revision

func (h History) Latest() *Revision {
	if len(h) == 0 {
		return nil
	}
	return h[len(h)-1]
}

func (h History) LatestVersion() Version {
	latest := h.Latest()
	if latest == nil {
		return 0
	}
	return latest.Version
}

func (h History) Get(version Version) *Revision {
	for _, revision := range h {
		if revision.Version == version {
			return revision
		}
	}
	return nil
}

type Container struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Image      string    `json:"image"`
	Workdir    string    `json:"workdir"`
	History    History   `json:"history"`
	GitState   *GitState `json:"git_state,omitempty"`
	BranchName string    `json:"branch_name,omitempty"`

	mu    sync.Mutex
	state *dagger.Container
}

var containers = map[string]*Container{}

func LoadContainers() error {
	ctr, err := loadState()
	if err != nil {
		return err
	}
	containers = ctr
	return nil
}

func CreateContainer(name, explanation, image, workdir string, includeGitContent bool) (*Container, error) {
	gitState, err := GetGitState()
	if err != nil {
		return nil, fmt.Errorf("failed to capture git state: %v", err)
	}

	container := &Container{
		ID:       uuid.New().String(),
		Name:     name,
		Image:    image,
		Workdir:  workdir,
		GitState: gitState,
	}

	if gitState.IsRepository {
		container.BranchName = "container-" + container.ID[:8]
	}

	containerState := dag.Container().From(image).WithWorkdir(workdir)

	if gitState.IsRepository && includeGitContent {
		hostDir := dag.Host().Directory(".")
		containerState = containerState.WithDirectory("/git-repo", hostDir)

		containerState = containerState.WithExec([]string{"sh", "-c", "command -v git || (apk add --no-cache git 2>/dev/null || apt-get update && apt-get install -y git 2>/dev/null || yum install -y git 2>/dev/null || true)"})
		containerState = containerState.WithExec([]string{"git", "config", "--global", "user.email", "container@example.com"})
		containerState = containerState.WithExec([]string{"git", "config", "--global", "user.name", "Container User"})

		containerState = containerState.WithExec([]string{"git", "checkout", "-b", container.BranchName})

		if hasUncommittedChanges() {
			containerState = containerState.WithWorkdir("/git-repo")
			containerState = containerState.WithExec([]string{"git", "add", "."})
			commitMessage := fmt.Sprintf("Container creation commit: %s", explanation)
			containerState = containerState.WithExec([]string{"git", "commit", "-m", commitMessage})

			commitHash, err := containerState.WithExec([]string{"git", "rev-parse", "HEAD"}).Stdout(context.Background())
			if err != nil {
				return nil, fmt.Errorf("failed to get commit hash: %v", err)
			}

			gitState.CurrentCommit = strings.TrimSpace(commitHash)
			container.GitState = gitState

			containerState = containerState.WithWorkdir(workdir)
		}
	} else {
		containerState = containerState.WithDirectory(".", dag.Directory())
	}

	err = container.apply(context.Background(), "Create container from "+image, explanation, containerState)
	if err != nil {
		return nil, err
	}
	containers[container.ID] = container

	if container.GitState != nil && container.GitState.IsRepository {
		if err := container.syncToHost(context.Background(), container.state); err != nil {
			fmt.Fprintf(debugWriter, "Warning: failed initial sync to host: %v\n", err)
		}
	}

	return container, nil
}

func GetContainer(idOrName string) *Container {
	if container, ok := containers[idOrName]; ok {
		return container
	}
	for _, container := range containers {
		if container.Name == idOrName {
			return container
		}
	}
	return nil
}

func ListContainers() []*Container {
	ctr := make([]*Container, 0, len(containers))
	for _, container := range containers {
		ctr = append(ctr, container)
	}
	return ctr
}

func (s *Container) apply(ctx context.Context, name, explanation string, newState *dagger.Container) error {
	if _, err := newState.Sync(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	version := s.History.LatestVersion() + 1
	s.state = newState
	s.History = append(s.History, &Revision{
		Version:     version,
		Name:        name,
		Explanation: explanation,
		CreatedAt:   time.Now(),
		state:       newState,
	})

	return saveState(s)
}

func (s *Container) Run(ctx context.Context, explanation, command, shell string) (string, error) {
	newState := s.state.WithExec([]string{shell, "-c", command})
	stdout, err := newState.Stdout(ctx)
	if err != nil {
		var exitErr *dagger.ExecError
		if errors.As(err, &exitErr) {
			return fmt.Sprintf("command failed with exit code %d.\nstdout: %s\nstderr: %s", exitErr.ExitCode, exitErr.Stdout, exitErr.Stderr), nil
		}
		return "", err
	}

	newState, err = s.withGitCommit(ctx, newState, fmt.Sprintf("Run %s: %s", command, explanation))
	if err != nil {
		return "", err
	}

	if err := s.apply(ctx, "Run "+command, explanation, newState); err != nil {
		return "", err
	}

	return stdout, nil
}

func (s *Container) RunBackground(ctx context.Context, explanation, command, shell string, ports []int) (map[int]string, error) {
	serviceState := s.state
	for _, port := range ports {
		serviceState = serviceState.WithExposedPort(port, dagger.ContainerWithExposedPortOpts{
			Protocol:    dagger.NetworkProtocolTcp,
			Description: fmt.Sprintf("Port %d", port),
		})
	}

	svc, err := serviceState.AsService(dagger.ContainerAsServiceOpts{
		Args: []string{shell, "-c", command},
	}).Start(context.Background())
	if err != nil {
		var exitErr *dagger.ExecError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("command failed with exit code %d.\nstdout: %s\nstderr: %s", exitErr.ExitCode, exitErr.Stdout, exitErr.Stderr)
		}
		return nil, err
	}

	endpoints := map[int]string{}
	for _, port := range ports {
		tunnel, err := dag.Host().Tunnel(svc, dagger.HostTunnelOpts{Native: true}).Start(context.Background())
		if err != nil {
			return nil, err
		}
		endpoints[port], err = tunnel.Endpoint(ctx, dagger.ServiceEndpointOpts{
			Port: port,
		})
		if err != nil {
			return nil, err
		}
	}

	return endpoints, nil
}

func (s *Container) Revert(ctx context.Context, explanation string, version Version) error {
	revision := s.History.Get(version)
	if revision == nil {
		return errors.New("no revisions found")
	}
	if err := s.apply(ctx, "Revert to "+revision.Name, explanation, revision.state); err != nil {
		return err
	}
	return nil
}

func (s *Container) Fork(ctx context.Context, explanation, name string, version *Version) (*Container, error) {
	revision := s.History.Latest()
	if version != nil {
		revision = s.History.Get(*version)
	}
	if revision == nil {
		return nil, errors.New("version not found")
	}

	forkedContainer := &Container{
		ID:    uuid.New().String(),
		Name:  name,
		Image: s.Image,
	}
	if err := forkedContainer.apply(ctx, "Fork from "+s.Name, explanation, revision.state); err != nil {
		return nil, err
	}
	containers[forkedContainer.ID] = forkedContainer
	return forkedContainer, nil
}

func (s *Container) withGitCommit(ctx context.Context, state *dagger.Container, commitMessage string) (*dagger.Container, error) {
	if s.GitState != nil && s.GitState.IsRepository {
		newState := state.WithWorkdir("/git-repo").
			WithExec([]string{"git", "add", "."}).
			WithExec([]string{"sh", "-c", "git diff --staged --quiet || git commit -m '" + commitMessage + "'"})

		commitHash, err := newState.WithExec([]string{"git", "rev-parse", "HEAD"}).Stdout(ctx)
		if err != nil {
			return nil, err
		}

		s.GitState.CurrentCommit = strings.TrimSpace(commitHash)

		finalState := newState.WithWorkdir(s.Workdir)

		if err := s.syncToHost(ctx, finalState); err != nil {
			fmt.Fprintf(debugWriter, "Warning: failed to sync to host: %v\n", err)
		}

		return finalState, nil
	}
	return state, nil
}

func (s *Container) createBundle(ctx context.Context, state *dagger.Container) ([]byte, error) {
	if s.GitState == nil || !s.GitState.IsRepository {
		return nil, fmt.Errorf("container does not have git content")
	}

	bundleState := state.WithWorkdir("/git-repo").
		WithExec([]string{"git", "bundle", "create", "/tmp/container.bundle", "--all"}).
		WithExec([]string{"sh", "-c", "base64 /tmp/container.bundle > /tmp/container.bundle.b64"})

	bundleDataB64, err := bundleState.File("/tmp/container.bundle.b64").Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create bundle: %v", err)
	}

	bundleData, err := base64.StdEncoding.DecodeString(strings.TrimSpace(bundleDataB64))
	if err != nil {
		return nil, fmt.Errorf("failed to decode bundle: %v", err)
	}

	return bundleData, nil
}

func (s *Container) syncToHost(ctx context.Context, state *dagger.Container) error {
	if s.GitState == nil || !s.GitState.IsRepository {
		return nil
	}

	bundleData, err := s.createBundle(ctx, state)
	if err != nil {
		return fmt.Errorf("failed to create bundle: %v", err)
	}

	return SyncBundleToHost(bundleData, s.ID, s.BranchName)
}
