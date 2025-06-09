package environment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"dagger.io/dagger"

	petname "github.com/dustinkirkland/golang-petname"
)

var dag *dagger.Client

const (
	defaultImage     = "ubuntu:24.04"
	alpineImage      = "alpine:3.21.3@sha256:a8560b36e8b8210634f77d9f7f9efd7ffa463e380b75e2e74aff4511df3ef88c"
	configDir        = ".container-use"
	instructionsFile = "AGENT.md"
	environmentFile  = "environment.json"
	lockFile         = "lock"
)

type Version int

type Revision struct {
	Version     Version   `json:"version"`
	Name        string    `json:"name"`
	Explanation string    `json:"explanation"`
	Output      string    `json:"output,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	State       string    `json:"state"`

	container *dagger.Container `json:"-"`
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

func Initialize(client *dagger.Client) error {
	dag = client
	return nil
}

type Environment struct {
	ID       string `json:"-"`
	Name     string `json:"-"`
	Source   string `json:"-"`
	Worktree string `json:"-"`

	Instructions  string   `json:"-"`
	Workdir       string   `json:"workdir"`
	BaseImage     string   `json:"base_image"`
	SetupCommands []string `json:"setup_commands,omitempty"`
	Secrets       []string `json:"secrets,omitempty"`

	History History `json:"-"`

	mu        sync.Mutex
	container *dagger.Container
}

func (env *Environment) save(baseDir string) error {
	cfg := path.Join(baseDir, configDir)
	if err := os.MkdirAll(cfg, 0755); err != nil {
		return err
	}

	if err := os.WriteFile(path.Join(cfg, instructionsFile), []byte(env.Instructions), 0644); err != nil {
		return err
	}

	envState, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(path.Join(cfg, environmentFile), envState, 0644); err != nil {
		return err
	}

	return nil
}

func (env *Environment) load(baseDir string) error {
	cfg := path.Join(baseDir, configDir)

	instructions, err := os.ReadFile(path.Join(cfg, instructionsFile))
	if err != nil {
		return err
	}
	env.Instructions = string(instructions)

	envState, err := os.ReadFile(path.Join(cfg, environmentFile))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(envState, env); err != nil {
		return err
	}

	return nil
}

func (env *Environment) isLocked(baseDir string) bool {
	if _, err := os.Stat(path.Join(baseDir, configDir, lockFile)); err == nil {
		return true
	}
	return false
}

func (env *Environment) apply(ctx context.Context, name, explanation, output string, newState *dagger.Container) error {
	if _, err := newState.Sync(ctx); err != nil {
		return err
	}

	env.mu.Lock()
	defer env.mu.Unlock()
	revision := &Revision{
		Version:     env.History.LatestVersion() + 1,
		Name:        name,
		Explanation: explanation,
		Output:      output,
		CreatedAt:   time.Now(),
		container:   newState,
	}
	containerID, err := revision.container.ID(ctx)
	if err != nil {
		return err
	}
	revision.State = string(containerID)
	env.container = revision.container
	env.History = append(env.History, revision)

	return nil
}

var environments = map[string]*Environment{}

func Create(ctx context.Context, explanation, source, name string) (*Environment, error) {
	env := &Environment{
		ID:           fmt.Sprintf("%s/%s", name, petname.Generate(2, "-")),
		Name:         name,
		Source:       source,
		BaseImage:    defaultImage,
		Instructions: "No instructions found. Please look around the filesystem and update me",
		Workdir:      "/workdir",
	}
	if err := env.load(source); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	worktreePath, err := env.InitializeWorktree(ctx, source, false)
	if err != nil {
		return nil, fmt.Errorf("failed intializing worktree: %w", err)
	}
	env.Worktree = worktreePath

	initialWorktree, err := env.InitializeWorktree(ctx, env.Source, true)
	if err != nil {
		return nil, err
	}

	container, err := env.buildBase(ctx, initialWorktree)
	if err != nil {
		return nil, err
	}

	slog.Info("Creating environment", "id", env.ID, "name", env.Name, "workdir", env.Workdir)

	if err := env.apply(ctx, "Create environment", "Create the environment", "", container); err != nil {
		return nil, err
	}
	environments[env.ID] = env

	if err := env.propagateToWorktree(ctx, "Init env "+name, explanation); err != nil {
		return nil, fmt.Errorf("failed to propagate to worktree: %w", err)
	}

	return env, nil
}

func Open(ctx context.Context, explanation, source, id string) (*Environment, error) {
	name, _, _ := strings.Cut(id, "/")
	env := &Environment{
		Name:   name,
		ID:     id,
		Source: source,
	}
	worktreePath, err := env.InitializeWorktree(ctx, source, false)
	if err != nil {
		return nil, fmt.Errorf("failed intializing worktree: %w", err)
	}
	env.Worktree = worktreePath

	if err := env.load(worktreePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Create(ctx, explanation, source, name)
		}
		return nil, err
	}

	initialWorktree, err := env.GetInitialWorktreePath()
	if err != nil {
		return nil, fmt.Errorf("failed to get initial worktree path: %w", err)
	}

	container, err := env.buildBase(ctx, initialWorktree)
	if err != nil {
		return nil, err
	}
	if err := env.apply(ctx, "Open environment", "Open the environment", "", container); err != nil {
		return nil, err
	}

	environments[env.ID] = env

	if err := env.loadStateFromNotes(ctx, worktreePath); err != nil {
		return nil, fmt.Errorf("failed to load state from notes: %w", err)
	}

	for _, revision := range env.History {
		revision.container = dag.LoadContainerFromID(dagger.ContainerID(revision.State))
	}
	if latest := env.History.Latest(); latest != nil {
		env.container = latest.container
	}

	return env, nil
}

func (env *Environment) buildBase(ctx context.Context, initialWorktree string) (*dagger.Container, error) {
	sourceDir := dag.Host().Directory(initialWorktree)

	container := dag.
		Container().
		From(env.BaseImage).
		WithWorkdir(env.Workdir)

	for _, secret := range env.Secrets {
		k, v, found := strings.Cut(secret, "=")
		if !found {
			return nil, fmt.Errorf("invalid secret: %s", secret)
		}
		container = container.WithSecretVariable(k, dag.Secret(v))
	}

	for _, command := range env.SetupCommands {
		var err error

		container = container.WithExec([]string{"sh", "-c", command})

		stdout, err := container.Stdout(ctx)
		if err != nil {
			var exitErr *dagger.ExecError
			if errors.As(err, &exitErr) {
				_ = env.addGitNote(ctx,
					fmt.Sprintf("$ %s\nexit %d\nstdout: %s\nstderr: %s\n\n",
						command,
						exitErr.ExitCode, exitErr.Stdout, exitErr.Stderr,
					),
				)
				return nil, fmt.Errorf("setup command failed with exit code %d.\nstdout: %s\nstderr: %s\n%w\n", exitErr.ExitCode, exitErr.Stdout, exitErr.Stderr, err)
			}

			return nil, fmt.Errorf("failed to execute setup command: %w", err)
		}

		_ = env.addGitNote(ctx, fmt.Sprintf("$ %s\n%s\n\n", command, stdout))
	}

	container = container.WithDirectory(".", sourceDir)

	return container, nil
}

func (env *Environment) Update(ctx context.Context, explanation, instructions, baseImage string, setupCommands, secrets []string) error {
	if env.isLocked(env.Source) {
		return fmt.Errorf("Environment is locked, no updates allowed. Try to make do with the current environment or ask a human to remove the lock file (%s)", path.Join(env.Source, configDir, lockFile))
	}

	env.Instructions = instructions
	env.BaseImage = baseImage
	env.SetupCommands = setupCommands
	env.Secrets = secrets

	initialWorktree, err := env.GetInitialWorktreePath()
	if err != nil {
		return err
	}

	// Re-build the base image from the worktree
	container, err := env.buildBase(ctx, initialWorktree)
	if err != nil {
		return err
	}

	if err := env.apply(ctx, "Update environment", explanation, "", container); err != nil {
		return err
	}

	return env.propagateToWorktree(ctx, "Update environment "+env.Name, explanation)
}

func Get(idOrName string) *Environment {
	if environment, ok := environments[idOrName]; ok {
		return environment
	}
	for _, environment := range environments {
		if environment.Name == idOrName {
			return environment
		}
	}
	return nil
}

func List() []*Environment {
	env := make([]*Environment, 0, len(environments))
	for _, environment := range environments {
		env = append(env, environment)
	}
	return env
}

func (env *Environment) Run(ctx context.Context, explanation, command, shell string, useEntrypoint bool) (string, error) {
	args := []string{}
	if command != "" {
		args = []string{shell, "-c", command}
	}
	newState := env.container.WithExec(args, dagger.ContainerWithExecOpts{
		UseEntrypoint: useEntrypoint,
	})
	stdout, err := newState.Stdout(ctx)
	if err != nil {
		var exitErr *dagger.ExecError
		if errors.As(err, &exitErr) {
			_ = env.addGitNote(ctx,
				fmt.Sprintf("$ %s\nexit %d\nstdout: %s\nstderr: %s\n\n",
					command,
					exitErr.ExitCode, exitErr.Stdout, exitErr.Stderr,
				),
			)
			return fmt.Sprintf("command failed with exit code %d.\nstdout: %s\nstderr: %s", exitErr.ExitCode, exitErr.Stdout, exitErr.Stderr), nil
		}
		return "", err
	}
	_ = env.addGitNote(ctx, fmt.Sprintf("$ %s\n%s\n\n", command, stdout))
	if err := env.apply(ctx, "Run "+command, explanation, stdout, newState); err != nil {
		return "", err
	}

	if err := env.propagateToWorktree(ctx, "Run "+command, explanation); err != nil {
		return "", fmt.Errorf("failed to propagate to worktree: %w", err)
	}

	return stdout, nil
}

type EndpointMapping struct {
	Internal string `json:"internal"`
	External string `json:"external"`
}

type EndpointMappings map[int]*EndpointMapping

func (env *Environment) RunBackground(ctx context.Context, explanation, command, shell string, ports []int, useEntrypoint bool) (EndpointMappings, error) {
	args := []string{}
	if command != "" {
		args = []string{shell, "-c", command}
	}
	serviceState := env.container

	// Expose ports
	for _, port := range ports {
		serviceState = serviceState.WithExposedPort(port, dagger.ContainerWithExposedPortOpts{
			Protocol:    dagger.NetworkProtocolTcp,
			Description: fmt.Sprintf("Port %d", port),
		})
	}

	// Start the service
	svc, err := serviceState.AsService(dagger.ContainerAsServiceOpts{
		Args:          args,
		UseEntrypoint: useEntrypoint,
	}).Start(ctx)
	if err != nil {
		var exitErr *dagger.ExecError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("command failed with exit code %d.\nstdout: %s\nstderr: %s", exitErr.ExitCode, exitErr.Stdout, exitErr.Stderr)
		}
		return nil, err
	}

	_ = env.addGitNote(ctx,
		fmt.Sprintf("$ %s &\n\n", command),
	)

	endpoints := EndpointMappings{}
	hostForwards := []dagger.PortForward{}

	for _, port := range ports {
		endpoints[port] = &EndpointMapping{}
		hostForwards = append(hostForwards, dagger.PortForward{
			Backend:  port,
			Frontend: rand.Intn(1000) + 5000,
			Protocol: dagger.NetworkProtocolTcp,
		})
	}

	// Expose ports on the host
	tunnel, err := dag.Host().Tunnel(svc, dagger.HostTunnelOpts{Ports: hostForwards}).Start(ctx)
	if err != nil {
		return nil, err
	}

	// Retrieve endpoints
	for _, forward := range hostForwards {
		externalEndpoint, err := tunnel.Endpoint(ctx, dagger.ServiceEndpointOpts{
			Port: forward.Frontend,
		})
		if err != nil {
			return nil, err
		}

		endpoints[forward.Backend].External = externalEndpoint
	}
	for port, endpoint := range endpoints {
		internalEndpoint, err := svc.Endpoint(ctx, dagger.ServiceEndpointOpts{
			Port: port,
		})
		if err != nil {
			return nil, err
		}
		endpoint.Internal = internalEndpoint
	}

	return endpoints, nil
}

func (env *Environment) SetEnv(ctx context.Context, explanation string, envs []string) error {
	state := env.container
	for _, env := range envs {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid environment variable: %s", env)
		}
		state = state.WithEnvVariable(parts[0], parts[1])
	}
	return env.apply(ctx, "Set env "+strings.Join(envs, ", "), explanation, "", state)
}

func (env *Environment) Revert(ctx context.Context, explanation string, version Version) error {
	revision := env.History.Get(version)
	if revision == nil {
		return errors.New("no revisions found")
	}
	if err := env.apply(ctx, "Revert to "+revision.Name, explanation, "", revision.container); err != nil {
		return err
	}
	return env.propagateToWorktree(ctx, "Revert to "+revision.Name, explanation)
}

func (env *Environment) Fork(ctx context.Context, explanation, name string, version *Version) (*Environment, error) {
	revision := env.History.Latest()
	if version != nil {
		revision = env.History.Get(*version)
	}
	if revision == nil {
		return nil, errors.New("version not found")
	}

	forkedEnvironment := &Environment{
		ID:   fmt.Sprintf("%s/%s", name, petname.Generate(2, "-")),
		Name: name,
	}
	if err := forkedEnvironment.apply(ctx, "Fork from "+env.Name, explanation, "", revision.container); err != nil {
		return nil, err
	}
	environments[forkedEnvironment.ID] = forkedEnvironment
	return forkedEnvironment, nil
}

func (env *Environment) Terminal(ctx context.Context) error {
	container := env.container
	// In case there's bash in the container, show the same pretty PS1 as for the default /bin/sh terminal in dagger
	container = container.WithNewFile("/root/.bash_aliases", `export PS1="\033[33mdagger\033[0m \033[02m\$(pwd | sed \"s|^\$HOME|~|\")\033[0m \$ "`+"\n")
	if _, err := container.Terminal(dagger.ContainerTerminalOpts{}).Sync(ctx); err != nil {
		return err
	}
	return nil
}

func (env *Environment) Checkpoint(ctx context.Context, target string) (string, error) {
	return env.container.Publish(ctx, target)
}

func (env *Environment) Delete(ctx context.Context) error {
	env.mu.Lock()
	defer env.mu.Unlock()

	if err := env.DeleteWorktree(); err != nil {
		return err
	}

	if err := env.DeleteLocalRemoteBranch(); err != nil {
		return err
	}

	// Remove from global environments map
	delete(environments, env.ID)

	return nil
}
