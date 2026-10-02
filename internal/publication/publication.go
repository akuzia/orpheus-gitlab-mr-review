package publication

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/protocol"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"

	"go.uber.org/zap"
)

var ErrStaleReview = errors.New("review diff is stale")

type Client interface {
	GetMergeRequest(ctx context.Context, projectID, iid int64) (gitlab.MergeRequest, error)
	ListMergeRequestDiscussions(ctx context.Context, projectID, iid int64) ([]gitlab.Discussion, error)
	ListMergeRequestDiffs(ctx context.Context, projectID, iid int64) ([]gitlab.DiffFile, error)
	CreateMergeRequestDiscussion(ctx context.Context, projectID, iid int64, body string, position gitlab.Position) error
	CreateMergeRequestNote(ctx context.Context, projectID, iid int64, body string) error
	AddMergeRequestDiscussionNote(ctx context.Context, projectID, iid int64, discussionID, body string) error
	SetMergeRequestDiscussionResolved(ctx context.Context, projectID, iid int64, discussionID string, resolved bool) error
	RemoveMergeRequestReviewer(ctx context.Context, projectID, iid, reviewerID int64) error
}

type Error struct {
	Code      string
	Retryable bool
	Err       error
}

func (e *Error) Error() string {
	return fmt.Sprintf("GitLab publication failed (%s): %v", e.Code, e.Err)
}
func (e *Error) Unwrap() error { return e.Err }

func IsRetryable(err error) bool {
	var publicationError *Error
	return errors.As(err, &publicationError) && publicationError.Retryable
}

func Code(err error) string {
	var publicationError *Error
	if errors.As(err, &publicationError) {
		return publicationError.Code
	}
	return ""
}

type Publisher struct {
	logger *zap.Logger
	client Client
	host   string
}

func New(logger *zap.Logger, client Client, gitLabHost string) *Publisher {
	return &Publisher{logger: logger, client: client, host: gitLabHost}
}

func (p *Publisher) Publish(ctx context.Context, input review.Input, bundle protocol.Bundle) error {
	if p.client == nil {
		return permanent("publisher_not_configured", errors.New("GitLab publication client is missing"))
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return err
	}

	projectID := input.MergeRequest.ProjectID
	iid := input.MergeRequest.IID
	discussions, err := p.client.ListMergeRequestDiscussions(ctx, projectID, iid)
	if err != nil {
		return classify("list_discussions", err)
	}
	completionMarker := CompletionMarker(bundle.Review.ReviewFingerprint)
	if hasOwnedMarker(discussions, input.Reviewer.ID, completionMarker) {
		return p.removeReviewer(ctx, input)
	}
	diffs, err := p.client.ListMergeRequestDiffs(ctx, projectID, iid)
	if err != nil {
		return classify("list_diffs", err)
	}

	for _, finding := range bundle.Confirmed {
		discussions, err = p.publishFinding(ctx, input, finding, diffs, discussions)
		if err != nil {
			return err
		}
	}
	for _, recommendation := range bundle.Recommendations {
		discussions, err = p.publishRecommendation(ctx, input, bundle, recommendation, discussions)
		if err != nil {
			return err
		}
	}
	for _, resolution := range bundle.Resolutions {
		discussions, err = p.applyResolution(ctx, input, resolution, discussions)
		if err != nil {
			return err
		}
	}

	if err := p.ensureCurrent(ctx, input); err != nil {
		return err
	}
	completionBody := completionBody(len(bundle.Confirmed), completionMarker)
	if err := p.client.CreateMergeRequestNote(ctx, projectID, iid, completionBody); err != nil {
		refreshed, fetchErr := p.client.ListMergeRequestDiscussions(ctx, projectID, iid)
		if fetchErr != nil {
			return classify("verify_completion", errors.Join(err, fetchErr))
		}
		if !hasOwnedMarker(refreshed, input.Reviewer.ID, completionMarker) {
			return classify("create_completion", err)
		}
		discussions = refreshed
	} else {
		refreshed, fetchErr := p.client.ListMergeRequestDiscussions(ctx, projectID, iid)
		if fetchErr != nil {
			return classify("verify_completion", fetchErr)
		}
		discussions = refreshed
	}
	if !hasOwnedMarker(discussions, input.Reviewer.ID, completionMarker) {
		return permanent("completion_marker_missing", errors.New("created completion note has no owned marker"))
	}

	p.logger.Info("published GitLab review",
		zap.String("merge_request_key", input.MRKey),
		zap.Int("confirmed_findings", len(bundle.Confirmed)),
		zap.Int("recommendations", len(bundle.Recommendations)),
		zap.Int("resolutions", len(bundle.Resolutions)),
	)
	return p.removeReviewer(ctx, input)
}

func (p *Publisher) publishFinding(
	ctx context.Context,
	input review.Input,
	finding protocol.Finding,
	diffs []gitlab.DiffFile,
	discussions []gitlab.Discussion,
) ([]gitlab.Discussion, error) {
	marker := FindingMarker(input.DiffFingerprint, finding)
	if finding.Previous != nil {
		return p.publishRecurringFinding(ctx, input, finding, marker, diffs, discussions)
	}
	if hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
		return discussions, nil
	}
	return p.publishNewFinding(ctx, input, finding, marker, diffs, discussions)
}

func (p *Publisher) publishRecurringFinding(
	ctx context.Context,
	input review.Input,
	finding protocol.Finding,
	marker string,
	diffs []gitlab.DiffFile,
	discussions []gitlab.Discussion,
) ([]gitlab.Discussion, error) {
	previous := *finding.Previous
	threadIndex, _, ok := ownedMarkerTarget(
		discussions, input.Reviewer.ID, previous.DiscussionID, previous.NoteID, previous.Marker,
	)
	if !ok {
		return discussions, permanent("previous_finding_not_owned", fmt.Errorf("previous finding target %q/%d is not an owned Orpheus note", previous.DiscussionID, previous.NoteID))
	}
	thread := discussions[threadIndex]
	if !thread.Resolved {
		return discussions, nil
	}

	switch thread.ResolutionCause {
	case gitlab.ResolutionCauseOutdatedByPush:
		if hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
			return discussions, nil
		}
		return p.publishNewFinding(ctx, input, finding, marker, diffs, discussions)
	case gitlab.ResolutionCauseExplicit:
		if thread.ResolvedBy.ID == input.Reviewer.ID {
			if hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
				return discussions, nil
			}
			return p.publishNewFinding(ctx, input, finding, marker, diffs, discussions)
		}
		if !hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
			if err := p.ensureCurrent(ctx, input); err != nil {
				return discussions, err
			}
			body := strings.TrimSpace(previous.RecurrenceComment) + "\n\n" + marker
			if err := p.client.AddMergeRequestDiscussionNote(
				ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, previous.DiscussionID, body,
			); err != nil {
				refreshed, fetchErr := p.refreshDiscussions(ctx, input)
				if fetchErr != nil {
					return discussions, classify("verify_recurrence_reply", errors.Join(err, fetchErr))
				}
				discussions = refreshed
				if !hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
					return discussions, classify("create_recurrence_reply", err)
				}
			} else {
				var refreshErr error
				discussions, refreshErr = p.refreshDiscussions(ctx, input)
				if refreshErr != nil {
					return discussions, classify("verify_recurrence_reply", refreshErr)
				}
			}
		}
		threadIndex, _, ok = ownedMarkerTarget(
			discussions, input.Reviewer.ID, previous.DiscussionID, previous.NoteID, previous.Marker,
		)
		if !ok {
			return discussions, permanent("previous_finding_disappeared", errors.New("previous finding target disappeared while publishing"))
		}
		if !discussions[threadIndex].Resolved {
			return discussions, nil
		}
		if err := p.ensureCurrent(ctx, input); err != nil {
			return discussions, err
		}
		if err := p.client.SetMergeRequestDiscussionResolved(
			ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, previous.DiscussionID, false,
		); err != nil {
			refreshed, fetchErr := p.refreshDiscussions(ctx, input)
			if fetchErr != nil {
				return discussions, classify("verify_reopen_discussion", errors.Join(err, fetchErr))
			}
			discussions = refreshed
			threadIndex, _, ok = ownedMarkerTarget(
				discussions, input.Reviewer.ID, previous.DiscussionID, previous.NoteID, previous.Marker,
			)
			if !ok || discussions[threadIndex].Resolved {
				return discussions, classify("reopen_discussion", err)
			}
			return discussions, nil
		}
		return p.refreshDiscussions(ctx, input)
	case gitlab.ResolutionCauseUnknown:
		return discussions, permanent("unknown_discussion_resolution_cause", fmt.Errorf("cannot safely classify how discussion %q was resolved", previous.DiscussionID))
	default:
		return discussions, permanent("invalid_discussion_resolution_state", fmt.Errorf("discussion %q is resolved without a resolution cause", previous.DiscussionID))
	}
}

func (p *Publisher) publishNewFinding(
	ctx context.Context,
	input review.Input,
	finding protocol.Finding,
	marker string,
	diffs []gitlab.DiffFile,
	discussions []gitlab.Discussion,
) ([]gitlab.Discussion, error) {
	body := findingBody(finding, marker)
	diff, ok := findDiff(diffs, finding.Path)
	if !ok || diff.Collapsed || diff.TooLarge {
		reason := "path is not available in the current GitLab diff"
		if ok {
			reason = "GitLab omitted the file diff because it is collapsed or too large"
		}
		return p.publishFindingFallback(ctx, input, finding, body, marker, reason, discussions)
	}
	position, err := diffPosition(input.MergeRequest.DiffRefs, diff, finding.Line)
	if err != nil {
		return p.publishFindingFallback(ctx, input, finding, body, marker, err.Error(), discussions)
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return discussions, err
	}
	err = p.client.CreateMergeRequestDiscussion(
		ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, body, position,
	)
	if err == nil {
		return p.refreshDiscussions(ctx, input)
	}
	refreshed, fetchErr := p.refreshDiscussions(ctx, input)
	if fetchErr != nil {
		return discussions, classify("verify_finding", errors.Join(err, fetchErr))
	}
	if hasOwnedMarker(refreshed, input.Reviewer.ID, marker) {
		return refreshed, nil
	}
	if errors.Is(err, gitlab.ErrInvalidDiscussionPosition) {
		return p.publishFindingFallback(ctx, input, finding, body, marker, "GitLab rejected the current inline position", refreshed)
	}
	return refreshed, classify("create_finding", err)
}

func (p *Publisher) publishFindingFallback(
	ctx context.Context,
	input review.Input,
	finding protocol.Finding,
	findingText, marker, reason string,
	discussions []gitlab.Discussion,
) ([]gitlab.Discussion, error) {
	if hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
		return discussions, nil
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return discussions, err
	}
	body := fmt.Sprintf("**Inline review finding for `%s:%d`**\n\n%s\n\nInline fallback: %s", finding.Path, finding.Line, findingText, reason)
	if err := p.client.CreateMergeRequestNote(ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, body); err != nil {
		refreshed, fetchErr := p.refreshDiscussions(ctx, input)
		if fetchErr != nil {
			return discussions, classify("verify_finding_fallback", errors.Join(err, fetchErr))
		}
		if !hasOwnedMarker(refreshed, input.Reviewer.ID, marker) {
			return refreshed, classify("create_finding_fallback", err)
		}
		return refreshed, nil
	}
	return p.refreshDiscussions(ctx, input)
}

func (p *Publisher) publishRecommendation(
	ctx context.Context,
	input review.Input,
	bundle protocol.Bundle,
	recommendation protocol.Recommendation,
	discussions []gitlab.Discussion,
) ([]gitlab.Discussion, error) {
	marker := RecommendationMarker(bundle.Review.DiffFingerprint, recommendation)
	if hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
		return discussions, nil
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return discussions, err
	}
	body := "**Orpheus project-rule recommendation**\n\n" + strings.TrimSpace(recommendation.Body) + "\n\n" + marker
	if err := p.client.CreateMergeRequestNote(ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, body); err != nil {
		refreshed, fetchErr := p.refreshDiscussions(ctx, input)
		if fetchErr != nil {
			return discussions, classify("verify_recommendation", errors.Join(err, fetchErr))
		}
		if !hasOwnedMarker(refreshed, input.Reviewer.ID, marker) {
			return refreshed, classify("create_recommendation", err)
		}
		return refreshed, nil
	}
	return p.refreshDiscussions(ctx, input)
}

func (p *Publisher) applyResolution(
	ctx context.Context,
	input review.Input,
	resolution protocol.Resolution,
	discussions []gitlab.Discussion,
) ([]gitlab.Discussion, error) {
	threadIndex, _, ok := ownedMarkerTarget(
		discussions, input.Reviewer.ID, resolution.DiscussionID, resolution.NoteID, resolution.Marker,
	)
	if !ok || discussions[threadIndex].Resolved {
		return discussions, nil
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return discussions, err
	}
	if err := p.client.SetMergeRequestDiscussionResolved(
		ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, resolution.DiscussionID, true,
	); err != nil {
		refreshed, fetchErr := p.refreshDiscussions(ctx, input)
		if fetchErr != nil {
			return discussions, classify("verify_resolution", errors.Join(err, fetchErr))
		}
		threadIndex, _, ok = ownedMarkerTarget(
			refreshed, input.Reviewer.ID, resolution.DiscussionID, resolution.NoteID, resolution.Marker,
		)
		if !ok || !refreshed[threadIndex].Resolved {
			return refreshed, classify("resolve_discussion", err)
		}
		return refreshed, nil
	}
	return p.refreshDiscussions(ctx, input)
}

func (p *Publisher) ensureCurrent(ctx context.Context, input review.Input) error {
	current, err := p.client.GetMergeRequest(ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID)
	if err != nil {
		return classify("read_current_merge_request", err)
	}
	if current.State != "opened" {
		return permanent("merge_request_not_open", fmt.Errorf("merge request state is %q", current.State))
	}
	if !hasReviewer(current.Reviewers, input.Reviewer.ID) {
		return permanent("reviewer_not_assigned", errors.New("reviewer is no longer assigned"))
	}
	fingerprint, err := review.DiffFingerprint(p.host, current)
	if err != nil {
		return permanent("diff_fingerprint_failed", err)
	}
	if fingerprint != input.DiffFingerprint {
		return permanent("stale_diff", ErrStaleReview)
	}
	return nil
}

func (p *Publisher) refreshDiscussions(ctx context.Context, input review.Input) ([]gitlab.Discussion, error) {
	discussions, err := p.client.ListMergeRequestDiscussions(ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID)
	if err != nil {
		return nil, classify("list_discussions", err)
	}
	return discussions, nil
}

func (p *Publisher) removeReviewer(ctx context.Context, input review.Input) error {
	if err := p.client.RemoveMergeRequestReviewer(
		ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, input.Reviewer.ID,
	); err != nil {
		return classify("remove_reviewer", err)
	}
	p.logger.Info("removed GitLab review request",
		zap.String("merge_request_key", input.MRKey),
		zap.Int64("reviewer_user_id", input.Reviewer.ID),
	)
	return nil
}

func classify(code string, err error) error {
	var publicationError *Error
	retryable := gitlab.IsRetryable(err)
	if errors.As(err, &publicationError) {
		retryable = retryable || publicationError.Retryable
	}
	return &Error{Code: code, Retryable: retryable, Err: err}
}

func permanent(code string, err error) error {
	return &Error{Code: code, Retryable: false, Err: err}
}
