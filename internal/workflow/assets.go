package workflow

import (
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
)

//go:embed assets/prompt.md
var promptAsset string

//go:embed assets/review_pack.py
var helperAsset string

const revisionFormatVersion = "workflow-revision-v2"

func Revision(instructions string) (string, error) {
	if strings.TrimSpace(instructions) == "" {
		return "", errors.New("build workflow revision: instructions are required")
	}

	return revisionForParts(
		revisionFormatVersion,
		instructions,
		promptAsset,
		helperAsset,
		beforeRunTemplate,
		afterRunTemplate,
	), nil
}

func revisionForParts(parts ...string) string {
	digest := sha256.New()
	for _, part := range parts {
		writeRevisionPart(digest, part)
	}

	return fmt.Sprintf("sha256:%x", digest.Sum(nil))
}

func writeRevisionPart(digest hash.Hash, value string) {
	_, _ = fmt.Fprintf(digest, "%d:", len(value))
	_, _ = io.WriteString(digest, value)
}
