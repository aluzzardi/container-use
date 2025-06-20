package environment

import (
	"context"
	"fmt"
	"strings"
)

func (s *Environment) FileRead(ctx context.Context, targetFile string, shouldReadEntireFile bool, startLineOneIndexed int, endLineOneIndexedInclusive int) (string, error) {
	file, err := s.container().File(targetFile).Contents(ctx)
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
	err := s.apply(ctx, "Write "+targetFile, explanation, "", s.container().WithNewFile(targetFile, contents))
	if err != nil {
		return fmt.Errorf("failed applying file write, skipping git propogation: %w", err)
	}

	s.Notes.Add("Write file %s\n%s\n\n", targetFile, explanation)

	return nil
}

func (s *Environment) FileDelete(ctx context.Context, explanation, targetFile string) error {
	err := s.apply(ctx, "Delete "+targetFile, explanation, "", s.container().WithoutFile(targetFile))
	if err != nil {
		return err
	}

	s.Notes.Add("Delete file %s\n%s\n\n", targetFile, explanation)

	return nil
}

func (s *Environment) FileList(ctx context.Context, path string) (string, error) {
	entries, err := s.container().Directory(path).Entries(ctx)
	if err != nil {
		return "", err
	}
	out := &strings.Builder{}
	for _, entry := range entries {
		fmt.Fprintf(out, "%s\n", entry)
	}
	return out.String(), nil
}
