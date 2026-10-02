package review

import (
	"fmt"
	"strings"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
)

// Input is the connector's immutable control-plane input. It is not a copy of
// all GitLab data available to the agent; the agent can read live MR context
// through glab while the code under review stays pinned by DiffRefs.
type Input struct {
	MergeRequest      gitlab.MergeRequest
	Project           gitlab.Project
	Notes             []gitlab.Note
	Reviewer          gitlab.User
	MRKey             string
	DiffFingerprint   string
	ReviewFingerprint string
}

// Snapshot is the complete set of review inputs observed by one successful
// GitLab poll. An empty snapshot is meaningful: it means that no merge request
// is currently assigned to the authenticated reviewer.
type Snapshot struct {
	Reviewer gitlab.User
	Inputs   []Input
}

func NewInput(host string, reviewer gitlab.User, source gitlab.ReviewInput) (Input, error) {
	mrKey, err := MergeRequestKey(host, source.MergeRequest.ProjectID, source.MergeRequest.IID)
	if err != nil {
		return Input{}, err
	}
	diffFingerprint, err := DiffFingerprint(host, source.MergeRequest)
	if err != nil {
		return Input{}, err
	}
	reviewFingerprint, err := ReviewFingerprint(host, source.MergeRequest, source.Notes, reviewer)
	if err != nil {
		return Input{}, err
	}

	return Input{
		MergeRequest:      source.MergeRequest,
		Project:           source.Project,
		Notes:             source.Notes,
		Reviewer:          reviewer,
		MRKey:             mrKey,
		DiffFingerprint:   diffFingerprint,
		ReviewFingerprint: reviewFingerprint,
	}, nil
}

func MergeRequestKey(host string, projectID, iid int64) (string, error) {
	host = normalizeHost(host)
	if host == "" {
		return "", fmt.Errorf("build merge request key: GitLab host is empty")
	}
	if projectID <= 0 {
		return "", fmt.Errorf("build merge request key: invalid project ID %d", projectID)
	}
	if iid <= 0 {
		return "", fmt.Errorf("build merge request key: invalid merge request IID %d", iid)
	}

	return fmt.Sprintf("%s:%d!%d", host, projectID, iid), nil
}

func normalizeHost(host string) string {
	return strings.TrimRight(strings.TrimSpace(host), "/")
}
