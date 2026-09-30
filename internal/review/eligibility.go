package review

import (
	"strings"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
)

type IneligibilityReason string

const (
	ReasonMRNotOpen               IneligibilityReason = "mr_not_open"
	ReasonReviewerIdentityInvalid IneligibilityReason = "reviewer_identity_invalid"
	ReasonReviewerNotAssigned     IneligibilityReason = "reviewer_not_assigned"
	ReasonProjectMismatch         IneligibilityReason = "project_mismatch"
	ReasonDiffRefsIncomplete      IneligibilityReason = "diff_refs_incomplete"
)

type Eligibility struct {
	Eligible bool
	Reasons  []IneligibilityReason
}

// EvaluateInputEligibility evaluates only gates that can be decided from the
// GitLab review input. Session history, unfinished lifecycle work, and concurrency
// capacity are admission gates evaluated later by the connector.
func EvaluateInputEligibility(input Input) Eligibility {
	var reasons []IneligibilityReason
	mr := input.MergeRequest

	if mr.State != "opened" {
		reasons = append(reasons, ReasonMRNotOpen)
	}
	if input.Reviewer.ID <= 0 {
		reasons = append(reasons, ReasonReviewerIdentityInvalid)
	} else if !hasReviewer(mr.Reviewers, input.Reviewer.ID) {
		reasons = append(reasons, ReasonReviewerNotAssigned)
	}
	if input.Project.ID != mr.ProjectID {
		reasons = append(reasons, ReasonProjectMismatch)
	}
	if strings.TrimSpace(mr.DiffRefs.BaseSHA) == "" ||
		strings.TrimSpace(mr.DiffRefs.StartSHA) == "" ||
		strings.TrimSpace(mr.DiffRefs.HeadSHA) == "" {
		reasons = append(reasons, ReasonDiffRefsIncomplete)
	}

	return Eligibility{Eligible: len(reasons) == 0, Reasons: reasons}
}

func hasReviewer(reviewers []gitlab.User, reviewerID int64) bool {
	for _, reviewer := range reviewers {
		if reviewer.ID == reviewerID {
			return true
		}
	}

	return false
}
