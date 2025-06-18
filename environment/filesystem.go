package environment

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"dagger.io/dagger"
)

func (s *Environment) FileRead(ctx context.Context, targetFile string, shouldReadEntireFile bool, startLineOneIndexed int, endLineOneIndexedInclusive int) (string, error) {
	file, err := s.container.File(targetFile).Contents(ctx)
	if err != nil {
		return "", err
	}
	if shouldReadEntireFile {
		return string(file), err
	}

	lines := strings.Split(string(file), "\n")
	start := startLineOneIndexed - 1
	start = max(start, 0)
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

func (s *Environment) FileWrite(ctx context.Context, explanation, targetFile, contents string) error {
	err := s.apply(ctx, "Write "+targetFile, explanation, "", s.container.WithNewFile(targetFile, contents))
	if err != nil {
		return fmt.Errorf("failed applying file write, skipping git propogation: %w", err)
	}

	return s.propagateToWorktree(ctx, "Write "+targetFile, explanation)
}

func (s *Environment) FileDelete(ctx context.Context, explanation, targetFile string) error {
	err := s.apply(ctx, "Delete "+targetFile, explanation, "", s.container.WithoutFile(targetFile))
	if err != nil {
		return err
	}

	return s.propagateToWorktree(ctx, "Delete "+targetFile, explanation)
}

func (s *Environment) FileList(ctx context.Context, path string) (string, error) {
	entries, err := s.container.Directory(path).Entries(ctx)
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
		return dag.Host().Directory(url[len("file://"):], dagger.HostDirectoryOpts{
			NoCache: true,
		})
	case strings.HasPrefix(url, "git://"):
		return dag.Git(url[len("git://"):]).Head().Tree()
	case strings.HasPrefix(url, "https://"):
		return dag.Git(url[len("https://"):]).Head().Tree()
	default:
		return dag.Host().Directory(url, dagger.HostDirectoryOpts{
			NoCache: true,
		})
	}
}

func (s *Environment) Upload(ctx context.Context, explanation, source string, target string) error {
	err := s.apply(ctx, "Upload "+source+" to "+target, explanation, "", s.container.WithDirectory(target, urlToDirectory(source)))
	if err != nil {
		return err
	}

	return s.propagateToWorktree(ctx, "Upload "+source+" to "+target, explanation)
}

func (s *Environment) Download(ctx context.Context, source string, target string) error {
	if _, err := s.container.Directory(source).Export(ctx, target); err != nil {
		if strings.Contains(err.Error(), "not a directory") {
			if _, err := s.container.File(source).Export(ctx, target); err != nil {
				return err
			}
			return nil
		}
		return err
	}

	return nil
}

func (s *Environment) RemoteDiff(ctx context.Context, source string, target string) (string, error) {
	sourceDir := urlToDirectory(source)
	targetDir := s.container.Directory(target)

	diff, err := dag.Container().From(alpineImage).
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

func (s *Environment) RevisionDiff(ctx context.Context, path string, fromVersion, toVersion Version) (string, error) {
	revisionDiff, err := s.revisionDiff(ctx, path, fromVersion, toVersion, true)
	if err != nil {
		if strings.Contains(err.Error(), "not a directory") {
			return s.revisionDiff(ctx, path, fromVersion, toVersion, false)
		}
		return "", err
	}
	return revisionDiff, nil
}

func (s *Environment) revisionDiff(ctx context.Context, path string, fromVersion, toVersion Version, directory bool) (string, error) {
	if path == "" {
		path = s.Config.Workdir
	}
	diffCtr := dag.Container().
		From(alpineImage).
		WithWorkdir("/diffs")
	if directory {
		diffCtr = diffCtr.
			WithMountedDirectory(
				filepath.Join("versions", fmt.Sprintf("%d", fromVersion)),
				s.History.Get(fromVersion).container.Directory(path)).
			WithMountedDirectory(
				filepath.Join("versions", fmt.Sprintf("%d", toVersion)),
				s.History.Get(toVersion).container.Directory(path))
	} else {
		diffCtr = diffCtr.
			WithMountedFile(
				filepath.Join("versions", fmt.Sprintf("%d", fromVersion)),
				s.History.Get(fromVersion).container.File(path)).
			WithMountedFile(
				filepath.Join("versions", fmt.Sprintf("%d", toVersion)),
				s.History.Get(toVersion).container.File(path))
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
