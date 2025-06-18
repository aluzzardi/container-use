package main

import (
	"context"
	"dagger/container-use/internal/dagger"
)

type ContainerUse struct {
	Source *dagger.Directory
}

// dagger module for building container-use
func New(
	//+defaultPath="/"
	source *dagger.Directory,
) *ContainerUse {
	return &ContainerUse{
		Source: source,
	}
}

// Build creates a binary for the current platform
func (m *ContainerUse) Build(ctx context.Context,
	//+optional
	platform dagger.Platform,
) *dagger.File {
	return dag.Go(m.Source).Binary("./cmd/cu", dagger.GoBinaryOpts{
		Platform: platform,
	})
}

// BuildMultiPlatform builds binaries for multiple platforms using GoReleaser
func (m *ContainerUse) BuildMultiPlatform(ctx context.Context) *dagger.Directory {
	return dag.Goreleaser(m.Source).Build().WithSnapshot().All()
}

// Release creates a release using GoReleaser
func (m *ContainerUse) Release(ctx context.Context,
	// Version tag for the release
	version string,
	// GitHub token for authentication
	githubToken *dagger.Secret,
	// GitHub org name for package publishing, set only if testing release process on a personal fork
	//+default="dagger"
	githubOrgName string,
) (string, error) {
	return dag.Goreleaser(m.Source).
		WithSecretVariable("GITHUB_TOKEN", githubToken).
		WithEnvVariable("GH_ORG_NAME", githubOrgName).
		Release().
		Run(ctx)
}

// Test runs the test suite
func (m *ContainerUse) Test(ctx context.Context,
	//+optional
	//+default="."
	// Package to test
	pkg string,
	//+optional
	// Run tests with verbose output
	verboseOutput bool,
	//+optional
	// Run tests including integration tests
	integration bool,
) (string, error) {
	ctr := dag.Go(m.Source).
		Base().
		WithMountedDirectory("/src", m.Source).
		WithWorkdir("/src")
	
	args := []string{"go", "test"}
	if verboseOutput {
		args = append(args, "-v")
	}
	if !integration {
		args = append(args, "-short")
	}
	args = append(args, pkg)
	
	return ctr.
		WithExec(args).
		Stdout(ctx)
}

// TestEnvironment runs the environment package tests specifically
func (m *ContainerUse) TestEnvironment(ctx context.Context,
	//+optional
	// Run tests with verbose output
	verboseOutput bool,
	//+optional
	// Include integration tests
	integration bool,
) (string, error) {
	return m.Test(ctx, "./environment", verboseOutput, integration)
}
