package review

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
)

const fingerprintVersion = 1

type diffFingerprintPayload struct {
	Version      int      `json:"version"`
	GitLabHost   string   `json:"gitlab_host"`
	ProjectID    int64    `json:"project_id"`
	IID          int64    `json:"iid"`
	SourceBranch string   `json:"source_branch"`
	TargetBranch string   `json:"target_branch"`
	DiffRefs     diffRefs `json:"diff_refs"`
}

type diffRefs struct {
	BaseSHA  string `json:"base_sha"`
	StartSHA string `json:"start_sha"`
	HeadSHA  string `json:"head_sha"`
}

type reviewFingerprintPayload struct {
	Version     int                    `json:"version"`
	Diff        diffFingerprintPayload `json:"diff"`
	Description string                 `json:"description"`
	Notes       []fingerprintNote      `json:"notes"`
}

type fingerprintNote struct {
	ID       int64  `json:"id"`
	AuthorID int64  `json:"author_id"`
	Body     string `json:"body"`
	Resolved bool   `json:"resolved,omitempty"`
}

func DiffFingerprint(host string, mergeRequest gitlab.MergeRequest) (string, error) {
	return hashJSON(newDiffFingerprintPayload(host, mergeRequest))
}

func ReviewFingerprint(
	host string,
	mergeRequest gitlab.MergeRequest,
	notes []gitlab.Note,
	reviewer gitlab.User,
) (string, error) {
	payload := reviewFingerprintPayload{
		Version:     fingerprintVersion,
		Diff:        newDiffFingerprintPayload(host, mergeRequest),
		Description: mergeRequest.Description,
		Notes:       reviewFingerprintNotes(notes, reviewer),
	}

	return hashJSON(payload)
}

func reviewFingerprintNotes(notes []gitlab.Note, reviewer gitlab.User) []fingerprintNote {
	selected := make([]fingerprintNote, 0, len(notes))
	for _, note := range notes {
		if note.Author.ID == reviewer.ID {
			continue
		}

		body := normalizeBody(note.Body)
		if !includeInReviewFingerprint(note, body, reviewer.Username) {
			continue
		}
		selected = append(selected, fingerprintNote{
			ID:       note.ID,
			AuthorID: note.Author.ID,
			Body:     body,
			Resolved: note.Resolved,
		})
	}

	slices.SortFunc(selected, func(left, right fingerprintNote) int {
		return cmp.Compare(left.ID, right.ID)
	})

	return selected
}

func newDiffFingerprintPayload(host string, mergeRequest gitlab.MergeRequest) diffFingerprintPayload {
	return diffFingerprintPayload{
		Version:      fingerprintVersion,
		GitLabHost:   normalizeHost(host),
		ProjectID:    mergeRequest.ProjectID,
		IID:          mergeRequest.IID,
		SourceBranch: mergeRequest.SourceBranch,
		TargetBranch: mergeRequest.TargetBranch,
		DiffRefs: diffRefs{
			BaseSHA:  mergeRequest.DiffRefs.BaseSHA,
			StartSHA: mergeRequest.DiffRefs.StartSHA,
			HeadSHA:  mergeRequest.DiffRefs.HeadSHA,
		},
	}
}

func includeInReviewFingerprint(note gitlab.Note, normalizedBody, reviewerUsername string) bool {
	if !note.System {
		return note.Resolvable
	}

	body := strings.ToLower(normalizedBody)
	username := strings.ToLower(strings.TrimSpace(reviewerUsername))
	if username != "" && strings.Contains(body, "@"+username) &&
		(strings.Contains(body, "review") || strings.Contains(body, "reviewer")) {
		return true
	}
	if (strings.Contains(body, "resolved") || strings.Contains(body, "unresolved")) &&
		(strings.Contains(body, "discussion") || strings.Contains(body, "thread")) {
		return true
	}

	return false
}

func normalizeBody(body string) string {
	return strings.Join(strings.Fields(body), " ")
}

func hashJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)

	return hex.EncodeToString(sum[:]), nil
}
