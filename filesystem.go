package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"dagger.io/dagger"
)

func (s *Container) FileRead(ctx context.Context, targetFile string, shouldReadEntireFile bool, startLineOneIndexed int, endLineOneIndexedInclusive int) (string, error) {
	file, err := s.state.File(targetFile).Contents(ctx)
	if err != nil {
		return "", err
	}
	if shouldReadEntireFile {
		return string(file), err
	}

	lines := strings.Split(string(file), "\n")
	start := startLineOneIndexed - 1
	if start < 0 {
		start = 0
	}
	if start >= len(lines) {
		start = len(lines) - 1
	}
	end := endLineOneIndexedInclusive
	if end >= len(lines) {
		end = len(lines) - 1
	}
	if end < 0 {
		end = 0
	}
	return strings.Join(lines[start:end], "\n"), nil
}

func (s *Container) FileWrite(ctx context.Context, explanation, targetFile, contents string) error {
	newState := s.state.WithNewFile(targetFile, contents)
	
	var err error
	newState, err = s.withGitCommit(ctx, newState, fmt.Sprintf("Write %s: %s", targetFile, explanation))
	if err != nil {
		return err
	}
	
	return s.apply(ctx, "Write "+targetFile, explanation, newState)
}

func (s *Container) FileDelete(ctx context.Context, explanation, targetFile string) error {
	newState := s.state.WithoutFile(targetFile)
	
	var err error
	newState, err = s.withGitCommit(ctx, newState, fmt.Sprintf("Delete %s: %s", targetFile, explanation))
	if err != nil {
		return err
	}
	
	return s.apply(ctx, "Delete "+targetFile, explanation, newState)
}

func (s *Container) FileList(ctx context.Context, path string) (string, error) {
	entries, err := s.state.Directory(path).Entries(ctx)
	if err != nil {
		return "", err
	}
	out := &strings.Builder{}
	for _, entry := range entries {
		fmt.Fprintf(out, "%s\n", entry)
	}
	return out.String(), nil
}

func urlToDirectory(url string) *dagger.Directory {
	switch {
	case strings.HasPrefix(url, "file://"):
		return dag.Host().Directory(url[len("file://"):])
	case strings.HasPrefix(url, "git://"):
		return dag.Git(url[len("git://"):]).Head().Tree()
	case strings.HasPrefix(url, "https://"):
		return dag.Git(url[len("https://"):]).Head().Tree()
	default:
		return dag.Host().Directory(url)
	}
}

func (s *Container) Upload(ctx context.Context, explanation, source string, target string) error {
	newState := s.state.WithDirectory(target, urlToDirectory(source))
	
	var err error
	newState, err = s.withGitCommit(ctx, newState, fmt.Sprintf("Upload %s to %s: %s", source, target, explanation))
	if err != nil {
		return err
	}
	
	return s.apply(ctx, "Upload "+source+" to "+target, explanation, newState)
}

func (s *Container) Download(ctx context.Context, source string, target string) error {
	if _, err := s.state.Directory(source).Export(ctx, target); err != nil {
		if strings.Contains(err.Error(), "not a directory") {
			if _, err := s.state.File(source).Export(ctx, target); err != nil {
				return err
			}
			return nil
		}
		return err
	}

	return nil
}

func (s *Container) RemoteDiff(ctx context.Context, source string, target string) (string, error) {
	sourceDir := urlToDirectory(source)
	targetDir := s.state.Directory(target)

	diff, err := dag.Container().From(AlpineImage).
		WithMountedDirectory("/source", sourceDir).
		WithMountedDirectory("/target", targetDir).
		WithExec([]string{"diff", "-burN", "/source", "/target"}, dagger.ContainerWithExecOpts{
			Expect: dagger.ReturnTypeAny,
		}).
		Stdout(ctx)
	if err != nil {
		var exitErr *dagger.ExecError
		if errors.As(err, &exitErr) {
			return fmt.Sprintf("command failed with exit code %d.\nstdout: %s\nstderr: %s", exitErr.ExitCode, exitErr.Stdout, exitErr.Stderr), nil
		}
		return "", err
	}
	return diff, nil
}

func (s *Container) RevisionDiff(ctx context.Context, path string, fromVersion, toVersion Version) (string, error) {
	revisionDiff, err := s.revisionDiff(ctx, path, fromVersion, toVersion, true)
	if err != nil {
		if strings.Contains(err.Error(), "not a directory") {
			return s.revisionDiff(ctx, path, fromVersion, toVersion, false)
		}
		return "", err
	}
	return revisionDiff, nil
}

func (s *Container) revisionDiff(ctx context.Context, path string, fromVersion, toVersion Version, directory bool) (string, error) {
	if path == "" {
		path = s.Workdir
	}
	diffCtr := dag.Container().
		From(AlpineImage).
		WithWorkdir("/diffs")
	if directory {
		diffCtr = diffCtr.
			WithMountedDirectory(
				filepath.Join("versions", fmt.Sprintf("%d", fromVersion)),
				s.History.Get(fromVersion).state.Directory(path)).
			WithMountedDirectory(
				filepath.Join("versions", fmt.Sprintf("%d", toVersion)),
				s.History.Get(toVersion).state.Directory(path))
	} else {
		diffCtr = diffCtr.
			WithMountedFile(
				filepath.Join("versions", fmt.Sprintf("%d", fromVersion)),
				s.History.Get(fromVersion).state.File(path)).
			WithMountedFile(
				filepath.Join("versions", fmt.Sprintf("%d", toVersion)),
				s.History.Get(toVersion).state.File(path))
	}

	diffCmd := []string{"diff", "-burN",
		filepath.Join("versions", fmt.Sprintf("%d", fromVersion)),
		filepath.Join("versions", fmt.Sprintf("%d", toVersion)),
	}
	diff, err := diffCtr.
		WithExec(diffCmd, dagger.ContainerWithExecOpts{
			Expect: dagger.ReturnTypeAny,
		}).
		Stdout(ctx)
	if err != nil {
		var exitErr *dagger.ExecError
		if errors.As(err, &exitErr) {
			return fmt.Sprintf("command failed with exit code %d.\nstdout: %s\nstderr: %s", exitErr.ExitCode, exitErr.Stdout, exitErr.Stderr), nil
		}
		return "", err
	}
	return diff, nil
}
