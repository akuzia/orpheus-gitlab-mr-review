package workflow

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/orpheus"
)

const (
	helperRoot        = ".orpheus/gitlab-review/code"
	beforeRunTemplate = `#!/bin/sh
set -eu
umask 077

for command_name in git python3 base64 gzip sha256sum; do
  command -v "$command_name" >/dev/null 2>&1 || {
    echo "before_run: required command is unavailable: $command_name" >&2
    exit 2
  }
done

if [ ! -d .git ]; then
  git clone --no-checkout -- "$ORPHEUS_GITLAB_PROJECT_CLONE_URL" . 1>&2
else
  test "$(git remote get-url origin)" = "$ORPHEUS_GITLAB_PROJECT_CLONE_URL" || {
    echo "before_run: existing checkout origin does not match contract" >&2
    exit 2
  }
fi

git fetch --no-tags origin 1>&2
git fetch --no-tags origin "+refs/merge-requests/$ORPHEUS_GITLAB_MR_IID/head:refs/remotes/origin/orpheus-mr-$ORPHEUS_GITLAB_MR_IID" 1>&2
test "$(git rev-parse "refs/remotes/origin/orpheus-mr-$ORPHEUS_GITLAB_MR_IID^{commit}")" = "$ORPHEUS_GITLAB_DIFF_HEAD_SHA" || {
  echo "before_run: merge request ref does not match pinned head SHA" >&2
  exit 2
}

git cat-file -e "$ORPHEUS_GITLAB_DIFF_BASE_SHA^{commit}"
git cat-file -e "$ORPHEUS_GITLAB_DIFF_HEAD_SHA^{commit}"
git checkout --detach "$ORPHEUS_GITLAB_DIFF_HEAD_SHA" 1>&2

helper_path='%s'
helper_dir=${helper_path%%/review-pack}
mkdir -p "$helper_dir" "$ORPHEUS_GITLAB_REVIEW_DIR/findings" "$ORPHEUS_GITLAB_REVIEW_DIR/confirmed" "$ORPHEUS_GITLAB_REVIEW_DIR/rejected" "$ORPHEUS_GITLAB_REVIEW_DIR/recommendations" "$ORPHEUS_GITLAB_REVIEW_DIR/resolutions" "$ORPHEUS_GITLAB_REVIEW_DIR/tmp"
if [ ! -f "$helper_path" ]; then
  helper_tmp="$helper_path.tmp.$$"
  trap 'rm -f "$helper_tmp"' EXIT HUP INT TERM
  printf '%%s' '%s' | base64 -d | gzip -d >"$helper_tmp"
  test "$(sha256sum "$helper_tmp" | cut -d ' ' -f 1)" = '%s' || {
    echo "before_run: installed helper digest mismatch" >&2
    exit 2
  }
  chmod 0500 "$helper_tmp"
  mv "$helper_tmp" "$helper_path"
  trap - EXIT HUP INT TERM
fi
test "$(sha256sum "$helper_path" | cut -d ' ' -f 1)" = '%s' || {
  echo "before_run: helper digest mismatch" >&2
  exit 2
}

contract_tmp="$ORPHEUS_GITLAB_REVIEW_DIR/contract.json.tmp.$$"
trap 'rm -f "$contract_tmp"' EXIT HUP INT TERM
printf '%%s' '%s' | base64 -d >"$contract_tmp"
mv "$contract_tmp" "$ORPHEUS_GITLAB_REVIEW_DIR/contract.json"
trap - EXIT HUP INT TERM
`
	afterRunTemplate = `#!/bin/sh
set -eu
helper_path='%s'
test -f "$helper_path" || {
  echo "after_run: review helper is missing" >&2
  exit 2
}
test "$(sha256sum "$helper_path" | cut -d ' ' -f 1)" = '%s' || {
  echo "after_run: review helper digest mismatch" >&2
  exit 2
}
exec python3 "$helper_path" "$ORPHEUS_GITLAB_REVIEW_DIR"
`
)

type renderedHooks struct {
	Hooks        orpheus.HooksInput
	Environment  map[string]string
	HelperSHA256 string
}

func renderHooks(metadata MetadataV1, cloneURL string, mergeRequestIID int64, timeoutSeconds int) (renderedHooks, error) {
	contractJSON, err := json.Marshal(metadata)
	if err != nil {
		return renderedHooks{}, fmt.Errorf("encode hook contract: %w", err)
	}
	helperDigest := sha256.Sum256([]byte(helperAsset))
	helperHex := hex.EncodeToString(helperDigest[:])
	helperSHA256 := "sha256:" + helperHex
	if metadata.Protocol.HelperSHA256 != helperSHA256 {
		return renderedHooks{}, fmt.Errorf("render hooks: helper digest does not match metadata")
	}
	helperPayload, err := compressedBase64(helperAsset)
	if err != nil {
		return renderedHooks{}, fmt.Errorf("compress review helper: %w", err)
	}

	helperPath := helperRoot + "/" + helperHex + "/review-pack"
	beforeRun := fmt.Sprintf(
		beforeRunTemplate,
		helperPath,
		helperPayload,
		helperHex,
		helperHex,
		base64.StdEncoding.EncodeToString(contractJSON),
	)
	afterRun := fmt.Sprintf(afterRunTemplate, helperPath, helperHex)

	return renderedHooks{
		Hooks: orpheus.HooksInput{
			BeforeRun:      new(beforeRun),
			AfterRun:       new(afterRun),
			TimeoutSeconds: new(timeoutSeconds),
		},
		Environment: map[string]string{
			"ORPHEUS_GITLAB_PROJECT_CLONE_URL": cloneURL,
			"ORPHEUS_GITLAB_MR_IID":            fmt.Sprintf("%d", mergeRequestIID),
			"ORPHEUS_GITLAB_DIFF_BASE_SHA":     metadata.DiffRefs.BaseSHA,
			"ORPHEUS_GITLAB_DIFF_START_SHA":    metadata.DiffRefs.StartSHA,
			"ORPHEUS_GITLAB_DIFF_HEAD_SHA":     metadata.DiffRefs.HeadSHA,
			"ORPHEUS_GITLAB_REVIEW_DIR":        metadata.Review.ArtifactsPath,
		},
		HelperSHA256: helperSHA256,
	}, nil
}

func helperDigest() string {
	digest := sha256.Sum256([]byte(helperAsset))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func compressedBase64(value string) (string, error) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(value)); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(compressed.Bytes()), nil
}

func projectCloneURL(httpURL, sshURL string) string {
	if value := strings.TrimSpace(sshURL); value != "" {
		return value
	}

	return strings.TrimSpace(httpURL)
}
