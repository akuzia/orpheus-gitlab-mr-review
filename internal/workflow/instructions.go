package workflow

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// LoadInstructions reads user-provided Markdown files in configuration order.
// The resulting text is used verbatim as Orpheus developer instructions.
func LoadInstructions(paths []string, maxBytes int) (string, error) {
	if len(paths) == 0 {
		return "", errors.New("load workflow instructions: at least one Markdown file is required")
	}
	if maxBytes <= 0 {
		return "", errors.New("load workflow instructions: maximum size must be positive")
	}

	seen := make(map[string]struct{}, len(paths))
	var combined bytes.Buffer
	for _, rawPath := range paths {
		path := filepath.Clean(strings.TrimSpace(rawPath))
		if path == "." {
			return "", errors.New("load workflow instructions: empty path")
		}
		if filepath.Ext(path) != ".md" {
			return "", fmt.Errorf("load workflow instructions %q: file must have .md extension", path)
		}
		if _, exists := seen[path]; exists {
			return "", fmt.Errorf("load workflow instructions: duplicate path %q", path)
		}
		seen[path] = struct{}{}

		separator := ""
		if combined.Len() > 0 {
			separator = "\n\n"
		}
		remainingBytes := maxBytes - combined.Len() - len(separator)
		content, err := readInstructionFile(path, remainingBytes, maxBytes)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(content) == "" {
			return "", fmt.Errorf("load workflow instructions %q: file is empty", path)
		}
		combined.WriteString(separator)
		combined.WriteString(content)
	}

	return combined.String(), nil
}

func readInstructionFile(path string, remainingBytes, maxBytes int) (string, error) {
	if remainingBytes <= 0 {
		return "", fmt.Errorf("load workflow instructions: content exceeds %d byte limit", maxBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("load workflow instructions %q: %w", path, err)
	}
	defer func() {
		_ = file.Close()
	}()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("load workflow instructions %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("load workflow instructions %q: not a regular file", path)
	}
	content, err := io.ReadAll(io.LimitReader(file, int64(remainingBytes)+1))
	if err != nil {
		return "", fmt.Errorf("load workflow instructions %q: %w", path, err)
	}
	if len(content) > remainingBytes {
		return "", fmt.Errorf("load workflow instructions: content exceeds %d byte limit", maxBytes)
	}
	if !utf8.Valid(content) {
		return "", fmt.Errorf("load workflow instructions %q: file is not UTF-8", path)
	}

	return string(content), nil
}
