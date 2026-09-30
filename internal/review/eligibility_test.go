package review

import (
	"testing"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"

	"github.com/stretchr/testify/require"
)

func TestEvaluateInputEligibility(t *testing.T) {
	t.Parallel()

	base := eligibleInput()
	tests := map[string]struct {
		mutate func(*Input)
		want   []IneligibilityReason
	}{
		"eligible": {},
		"closed": {
			mutate: func(input *Input) { input.MergeRequest.State = "closed" },
			want:   []IneligibilityReason{ReasonMRNotOpen},
		},
		"draft assigned to reviewer": {
			mutate: func(input *Input) { input.MergeRequest.Draft = true },
		},
		"reviewer missing": {
			mutate: func(input *Input) { input.MergeRequest.Reviewers = nil },
			want:   []IneligibilityReason{ReasonReviewerNotAssigned},
		},
		"reviewer identity invalid": {
			mutate: func(input *Input) { input.Reviewer.ID = 0 },
			want:   []IneligibilityReason{ReasonReviewerIdentityInvalid},
		},
		"project mismatch": {
			mutate: func(input *Input) { input.Project.ID = 99 },
			want:   []IneligibilityReason{ReasonProjectMismatch},
		},
		"diff refs incomplete": {
			mutate: func(input *Input) { input.MergeRequest.DiffRefs.HeadSHA = "" },
			want:   []IneligibilityReason{ReasonDiffRefsIncomplete},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			input := base
			if test.mutate != nil {
				test.mutate(&input)
			}

			decision := EvaluateInputEligibility(input)

			require.Equal(t, len(test.want) == 0, decision.Eligible)
			require.Equal(t, test.want, decision.Reasons)
		})
	}
}

func eligibleInput() Input {
	reviewer := gitlab.User{ID: 123, Username: "orpheus"}
	return Input{
		Reviewer: reviewer,
		Project: gitlab.Project{
			ID:                42,
			PathWithNamespace: "team/project",
		},
		MergeRequest: gitlab.MergeRequest{
			ProjectID: 42,
			IID:       17,
			State:     "opened",
			Reviewers: []gitlab.User{reviewer},
			DiffRefs: gitlab.DiffRefs{
				BaseSHA:  "base",
				StartSHA: "start",
				HeadSHA:  "head",
			},
		},
	}
}
