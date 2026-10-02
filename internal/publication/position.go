package publication

import (
	"bufio"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
)

func findDiff(diffs []gitlab.DiffFile, path string) (gitlab.DiffFile, bool) {
	for _, diff := range diffs {
		if diff.NewPath == path || diff.OldPath == path {
			return diff, true
		}
	}
	return gitlab.DiffFile{}, false
}

func diffPosition(refs gitlab.DiffRefs, diff gitlab.DiffFile, targetNewLine int) (gitlab.Position, error) {
	oldLine, newLine := 0, 0
	inHunk := false
	scanner := bufio.NewScanner(strings.NewReader(diff.Diff))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "@@"):
			var err error
			oldLine, newLine, err = parseHunkHeader(line)
			if err != nil {
				return gitlab.Position{}, err
			}
			inHunk = true
			continue
		case !inHunk, strings.HasPrefix(line, "\\ No newline at end of file"):
			continue
		}
		if line == "" {
			continue
		}
		position := gitlab.Position{
			BaseSHA: refs.BaseSHA, StartSHA: refs.StartSHA, HeadSHA: refs.HeadSHA,
			PositionType: "text", OldPath: diff.OldPath, NewPath: diff.NewPath,
		}
		switch line[0] {
		case ' ':
			if newLine == targetNewLine {
				position.OldLine, position.NewLine = int64(oldLine), int64(newLine)
				return position, nil
			}
			oldLine++
			newLine++
		case '+':
			if newLine == targetNewLine {
				position.NewLine = int64(newLine)
				return position, nil
			}
			newLine++
		case '-':
			oldLine++
		}
	}
	if err := scanner.Err(); err != nil {
		return gitlab.Position{}, err
	}
	return gitlab.Position{}, fmt.Errorf("new line %d is not present in the diff hunk", targetNewLine)
}

func parseHunkHeader(header string) (int, int, error) {
	parts := strings.Fields(header)
	if len(parts) < 3 || !strings.HasPrefix(parts[1], "-") || !strings.HasPrefix(parts[2], "+") {
		return 0, 0, fmt.Errorf("invalid diff hunk header %q", header)
	}
	oldLine, err := parseHunkStart(parts[1][1:])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid old range in hunk header %q: %w", header, err)
	}
	newLine, err := parseHunkStart(parts[2][1:])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid new range in hunk header %q: %w", header, err)
	}
	return oldLine, newLine, nil
}

func parseHunkStart(value string) (int, error) {
	if index := strings.IndexByte(value, ','); index >= 0 {
		value = value[:index]
	}
	line, err := strconv.Atoi(value)
	if err != nil || line < 0 {
		return 0, errors.New("line number must be non-negative")
	}
	return line, nil
}
