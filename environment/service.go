package environment

import (
	"context"
	"errors"
	"fmt"

	"dagger.io/dagger"
)

type Service struct {
	Config    *ServiceConfig   `json:"config"`
	Endpoints EndpointMappings `json:"endpoints"`

	svc *dagger.Service
}

type EndpointMapping struct {
	Internal string `json:"internal"`
	External string `json:"external"`
}

type EndpointMappings map[int]*EndpointMapping

func (env *Environment) startServices(ctx context.Context) ([]*Service, error) {
	services := []*Service{}
	for _, cfg := range env.Config.Services {
		service, err := env.startService(ctx, cfg)
		if err != nil {
			return nil, err
		}
		services = append(services, service)
	}
	return services, nil
}

func (env *Environment) startService(ctx context.Context, cfg *ServiceConfig) (*Service, error) {
	container := dag.Container().From(cfg.Image)
	container, err := containerWithEnvAndSecrets(container, cfg.Env, cfg.Secrets)
	if err != nil {
		return nil, err
	}

	if cfg.Command != "" {
		container = container.WithExec([]string{"sh", "-c", cfg.Command})
	}

	args := []string{}
	if cfg.Command != "" {
		args = []string{"sh", "-c", cfg.Command}
	}

	// Expose ports
	for _, port := range cfg.ExposedPorts {
		container = container.WithExposedPort(port, dagger.ContainerWithExposedPortOpts{
			Protocol:    dagger.NetworkProtocolTcp,
			Description: fmt.Sprintf("Port %d", port),
		})
	}

	// Start the service
	svc, err := container.AsService(dagger.ContainerAsServiceOpts{
		Args:          args,
		UseEntrypoint: true,
	}).Start(ctx)
	if err != nil {
		var exitErr *dagger.ExecError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("command failed with exit code %d.\nstdout: %s\nstderr: %s", exitErr.ExitCode, exitErr.Stdout, exitErr.Stderr)
		}
		return nil, err
	}

	endpoints := EndpointMappings{}
	for _, port := range cfg.ExposedPorts {
		endpoint := &EndpointMapping{
			Internal: fmt.Sprintf("%s:%d", cfg.Name, port),
		}
		endpoints[port] = endpoint

		// Expose ports on the host
		tunnel, err := dag.Host().Tunnel(svc, dagger.HostTunnelOpts{
			Ports: []dagger.PortForward{
				{
					Backend:  port,
					Frontend: 0,
					Protocol: dagger.NetworkProtocolTcp,
				},
			},
		}).Start(ctx)
		if err != nil {
			return nil, err
		}

		externalEndpoint, err := tunnel.Endpoint(ctx, dagger.ServiceEndpointOpts{})
		if err != nil {
			return nil, fmt.Errorf("failed to get endpoint for service %s: %w", cfg.Name, err)
		}
		endpoint.External = externalEndpoint
	}

	return &Service{
		Config:    cfg,
		Endpoints: endpoints,
		svc:       svc,
	}, nil
}

func (env *Environment) AddService(ctx context.Context, explanation string, cfg *ServiceConfig) (*Service, error) {
	if env.Config.Services.Get(cfg.Name) != nil {
		return nil, fmt.Errorf("service %s already exists", cfg.Name)
	}
	svc, err := env.startService(ctx, cfg)
	if err != nil {
		return nil, err
	}
	env.Config.Services = append(env.Config.Services, cfg)
	env.Services = append(env.Services, svc)

	state := env.container().WithServiceBinding(cfg.Name, svc.svc)
	if err := env.apply(ctx, "Add service "+cfg.Name, explanation, "", state); err != nil {
		return nil, err
	}

	env.Notes.Add("Add service %s\n%s\n\n", cfg.Name, explanation)

	return svc, nil
}
