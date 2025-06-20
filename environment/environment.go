package environment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"dagger.io/dagger"
)

var dag *dagger.Client

func Initialize(client *dagger.Client) error {
	dag = client
	return nil
}

type Environment struct {
	Config *EnvironmentConfig
	State  *State

	ID       string
	Worktree string

	Services []*Service
	Notes    Notes

	mu sync.RWMutex
}

func New(ctx context.Context, id, description, worktree string) (*Environment, error) {
	env := &Environment{
		Worktree: worktree,
		Config:   DefaultConfig(),
		State: &State{
			Description: description,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		},
	}

	if err := env.Config.Load(worktree); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	container, err := env.buildBase(ctx)
	if err != nil {
		return nil, err
	}

	slog.Info("Creating environment", "id", env.ID, "workdir", env.Config.Workdir)

	if err := env.apply(ctx, "Create environment", "Create the environment", "", container); err != nil {
		return nil, err
	}

	return env, nil
}

func (env *Environment) Export(ctx context.Context) (rerr error) {
	_, err := env.container().Directory(env.Config.Workdir).Export(
		ctx,
		env.Worktree,
		dagger.DirectoryExportOpts{Wipe: true},
	)
	if err != nil {
		return err
	}

	slog.Info("Saving environment")
	if err := env.Config.Save(env.Worktree); err != nil {
		return err
	}
	return nil

}

func (env *Environment) container() *dagger.Container {
	env.mu.RLock()
	defer env.mu.RUnlock()

	return dag.LoadContainerFromID(dagger.ContainerID(env.State.Container))
}

func Load(ctx context.Context, id string, state []byte, worktree string) (*Environment, error) {
	env := &Environment{
		ID:       id,
		Worktree: worktree,
		Config:   DefaultConfig(),
		State:    &State{},
	}
	if err := env.Config.Load(worktree); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	if err := env.State.Unmarshal(state); err != nil {
		return nil, err
	}

	return env, nil
}

func (env *Environment) apply(ctx context.Context, name, explanation, output string, newState *dagger.Container) error {
	if _, err := newState.Sync(ctx); err != nil {
		return err
	}

	containerID, err := newState.ID(ctx)
	if err != nil {
		return err
	}

	env.mu.Lock()
	defer env.mu.Unlock()
	env.State.UpdatedAt = time.Now()
	env.State.Container = string(containerID)

	return nil
}

func containerWithEnvAndSecrets(container *dagger.Container, envs, secrets []string) (*dagger.Container, error) {
	for _, env := range envs {
		k, v, found := strings.Cut(env, "=")
		if !found {
			return nil, fmt.Errorf("invalid env variable: %s", env)
		}
		if !found {
			return nil, fmt.Errorf("invalid environment variable: %s", env)
		}
		container = container.WithEnvVariable(k, v)
	}

	for _, secret := range secrets {
		k, v, found := strings.Cut(secret, "=")
		if !found {
			return nil, fmt.Errorf("invalid secret: %s", secret)
		}
		container = container.WithSecretVariable(k, dag.Secret(v))
	}

	return container, nil
}

func (env *Environment) buildBase(ctx context.Context) (*dagger.Container, error) {
	sourceDir := dag.Host().Directory(env.Worktree, dagger.HostDirectoryOpts{
		NoCache: true,
	})

	container := dag.
		Container().
		From(env.Config.BaseImage).
		WithWorkdir(env.Config.Workdir)

	container, err := containerWithEnvAndSecrets(container, env.Config.Env, env.Config.Secrets)
	if err != nil {
		return nil, err
	}

	for _, command := range env.Config.SetupCommands {
		var err error

		container = container.WithExec([]string{"sh", "-c", command})

		stdout, err := container.Stdout(ctx)
		if err != nil {
			var exitErr *dagger.ExecError
			if errors.As(err, &exitErr) {
				env.Notes.Add("$ %s\nexit %d\nstdout: %s\nstderr: %s\n\n", command, exitErr.ExitCode, exitErr.Stdout, exitErr.Stderr)
				return nil, fmt.Errorf("setup command failed with exit code %d.\nstdout: %s\nstderr: %s\n%w\n", exitErr.ExitCode, exitErr.Stdout, exitErr.Stderr, err)
			}

			return nil, fmt.Errorf("failed to execute setup command: %w", err)
		}

		env.Notes.Add("$ %s\n%s\n\n", command, stdout)
	}

	env.Services, err = env.startServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to start services: %w", err)
	}
	for _, service := range env.Services {
		container = container.WithServiceBinding(service.Config.Name, service.svc)
	}

	container = container.WithDirectory(".", sourceDir)

	return container, nil
}

func (env *Environment) UpdateConfig(ctx context.Context, explanation string, newConfig *EnvironmentConfig) error {
	if env.Config.Locked(env.Worktree) {
		return fmt.Errorf("Environment is locked, no updates allowed. Try to make do with the current environment or ask a human to remove the lock file (%s)", path.Join(env.Worktree, configDir, lockFile))
	}

	env.Config = newConfig

	// Re-build the base image from the worktree
	container, err := env.buildBase(ctx)
	if err != nil {
		return err
	}

	if err := env.apply(ctx, "Update environment", explanation, "", container); err != nil {
		return err
	}

	return nil
}

func (env *Environment) Run(ctx context.Context, explanation, command, shell string, useEntrypoint bool) (string, error) {
	args := []string{}
	if command != "" {
		args = []string{shell, "-c", command}
	}
	newState := env.container().WithExec(args, dagger.ContainerWithExecOpts{
		UseEntrypoint: useEntrypoint,
	})
	stdout, err := newState.Stdout(ctx)
	if err != nil {
		var exitErr *dagger.ExecError
		if errors.As(err, &exitErr) {
			env.Notes.Add("$ %s\nexit %d\nstdout: %s\nstderr: %s\n\n", command, exitErr.ExitCode, exitErr.Stdout, exitErr.Stderr)
			return fmt.Sprintf("command failed with exit code %d.\nstdout: %s\nstderr: %s", exitErr.ExitCode, exitErr.Stdout, exitErr.Stderr), nil
		}
		return "", err
	}
	env.Notes.Add("$ %s\n%s\n\n", command, stdout)

	return stdout, nil
}

func (env *Environment) RunBackground(ctx context.Context, explanation, command, shell string, ports []int, useEntrypoint bool) (EndpointMappings, error) {
	args := []string{}
	if command != "" {
		args = []string{shell, "-c", command}
	}
	serviceState := env.container()

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

	env.Notes.Add("$ %s &\n\n", command)

	endpoints := EndpointMappings{}
	for _, port := range ports {
		endpoint := &EndpointMapping{}
		endpoints[port] = endpoint

		// Expose port on the host
		tunnel, err := dag.Host().Tunnel(svc, dagger.HostTunnelOpts{
			Ports: []dagger.PortForward{
				{
					Backend:  port,
					Protocol: dagger.NetworkProtocolTcp,
				},
			},
		}).Start(ctx)
		if err != nil {
			return nil, err
		}

		externalEndpoint, err := tunnel.Endpoint(ctx, dagger.ServiceEndpointOpts{})
		if err != nil {
			return nil, err
		}
		endpoint.External = externalEndpoint

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

func (env *Environment) Terminal(ctx context.Context) error {
	container := env.container()
	var cmd []string
	var sourceRC string
	if shells, err := container.File("/etc/shells").Contents(ctx); err == nil {
		for shell := range strings.Lines(shells) {
			if shell[0] == '#' {
				continue
			}
			shell = strings.TrimRight(shell, "\n")
			if strings.HasSuffix(shell, "/bash") {
				sourceRC = fmt.Sprintf("[ -f ~/.bashrc ] && . ~/.bashrc; %q --version | head -4; ", shell)
				cmd = []string{shell, "--rcfile", "/cu/rc.sh", "-i"}
				break
			}
		}
	}
	// Try to show the same pretty PS1 as for the default /bin/sh terminal in dagger
	container = container.WithNewFile("/cu/rc.sh", sourceRC+`export PS1="\033[33mcu\033[0m \033[02m\$(pwd | sed \"s|^\$HOME|~|\")\033[0m \$ "`+"\n")
	if cmd == nil {
		// If bash not available, assume POSIX shell
		container = container.WithEnvVariable("ENV", "/cu/rc.sh")
		cmd = []string{"sh"}
	}
	if _, err := container.Terminal(dagger.ContainerTerminalOpts{
		Cmd: cmd,
	}).Sync(ctx); err != nil {
		return err
	}
	return nil
}

func (env *Environment) Checkpoint(ctx context.Context, target string) (string, error) {
	return env.container().Publish(ctx, target)
}
