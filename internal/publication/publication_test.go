package publication

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/protocol"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

type publicationClientMock struct {
	mergeRequest         gitlab.MergeRequest
	diffs                []gitlab.DiffFile
	discussions          []gitlab.Discussion
	createdThreads       int
	createdNotes         int
	addedReplies         []string
	resolutionUpdates    []bool
	removedReviewer      bool
	createThreadErr      error
	createThreadAccepted bool
	createNoteErr        error
	createNoteAccepted   bool
	hideCreatedNotes     bool
	addReplyErr          error
	addReplyAccepted     bool
	resolveErr           error
	resolveAccepted      bool
	removeErr            error
	afterMutation        func()
	mrReads              int
	beforeRead           func(int)
}

func (m *publicationClientMock) GetMergeRequest(context.Context, int64, int64) (gitlab.MergeRequest, error) {
	m.mrReads++
	if m.beforeRead != nil {
		m.beforeRead(m.mrReads)
	}
	return m.mergeRequest, nil
}

func (m *publicationClientMock) ListMergeRequestDiscussions(context.Context, int64, int64) ([]gitlab.Discussion, error) {
	return append([]gitlab.Discussion(nil), m.discussions...), nil
}

func (m *publicationClientMock) ListMergeRequestDiffs(context.Context, int64, int64) ([]gitlab.DiffFile, error) {
	return m.diffs, nil
}

func (m *publicationClientMock) CreateMergeRequestDiscussion(_ context.Context, _, _ int64, body string, position gitlab.Position) error {
	m.createdThreads++
	if m.createThreadErr != nil && !m.createThreadAccepted {
		return m.createThreadErr
	}
	m.discussions = append(m.discussions, gitlab.Discussion{
		ID:         fmt.Sprintf("new-%d", m.createdThreads),
		Resolvable: true,
		Notes: []gitlab.Note{{
			ID: 1000 + int64(m.createdThreads), Body: body, Author: gitlab.User{ID: 42, Username: "orpheus"},
			Resolvable: true, Position: &position,
		}},
	})
	return m.createThreadErr
}

func (m *publicationClientMock) CreateMergeRequestNote(_ context.Context, _, _ int64, body string) error {
	m.createdNotes++
	if m.createNoteErr != nil && !m.createNoteAccepted {
		return m.createNoteErr
	}
	if m.hideCreatedNotes {
		return nil
	}
	m.discussions = append(m.discussions, gitlab.Discussion{
		ID: fmt.Sprintf("note-%d", m.createdNotes), IndividualNote: true,
		Notes: []gitlab.Note{{ID: 2000 + int64(m.createdNotes), Body: body, Author: gitlab.User{ID: 42, Username: "orpheus"}}},
	})
	if m.afterMutation != nil {
		m.afterMutation()
	}
	return m.createNoteErr
}

func (m *publicationClientMock) AddMergeRequestDiscussionNote(_ context.Context, _, _ int64, discussionID, body string) error {
	m.addedReplies = append(m.addedReplies, body)
	if m.addReplyErr != nil && !m.addReplyAccepted {
		return m.addReplyErr
	}
	for index := range m.discussions {
		if m.discussions[index].ID == discussionID {
			m.discussions[index].Notes = append(m.discussions[index].Notes, gitlab.Note{
				ID: 3000 + int64(len(m.addedReplies)), Body: body, Author: gitlab.User{ID: 42, Username: "orpheus"},
			})
		}
	}
	return m.addReplyErr
}

func (m *publicationClientMock) SetMergeRequestDiscussionResolved(_ context.Context, _, _ int64, discussionID string, resolved bool) error {
	m.resolutionUpdates = append(m.resolutionUpdates, resolved)
	if m.resolveErr != nil && !m.resolveAccepted {
		return m.resolveErr
	}
	for index := range m.discussions {
		if m.discussions[index].ID == discussionID {
			m.discussions[index].Resolved = resolved
			if !resolved {
				m.discussions[index].ResolutionCause = gitlab.ResolutionCauseNone
			}
		}
	}
	return m.resolveErr
}

func (m *publicationClientMock) RemoveMergeRequestReviewer(context.Context, int64, int64, int64, gitlab.DiffRefs) error {
	if m.removeErr != nil {
		return m.removeErr
	}
	m.removedReviewer = true
	return nil
}

func TestPublisherPublishesFindingCompletionAndRemovesReviewer(t *testing.T) {
	input, client := publicationFixture(t)
	bundle := publicationBundle(input, []protocol.Finding{publicationFinding(nil)})

	err := New(zaptest.NewLogger(t), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)

	require.NoError(t, err)
	require.Equal(t, 1, client.createdThreads)
	require.Equal(t, 1, client.createdNotes)
	require.True(t, client.removedReviewer)
	require.Contains(t, client.discussions[len(client.discussions)-1].Notes[0].Body, "Confirmed findings: 1")
}

func TestPublisherKeepsOpenRecurringFindingWithoutDuplicate(t *testing.T) {
	input, client := publicationFixture(t)
	previous := previousFinding()
	client.discussions = []gitlab.Discussion{previousDiscussion(previous, false, gitlab.ResolutionCauseNone)}
	bundle := publicationBundle(input, []protocol.Finding{publicationFinding(&previous)})

	err := New(zaptest.NewLogger(t), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)

	require.NoError(t, err)
	require.Zero(t, client.createdThreads)
	require.Empty(t, client.addedReplies)
	require.Empty(t, client.resolutionUpdates)
	require.Contains(t, client.discussions[len(client.discussions)-1].Notes[0].Body, "Confirmed findings: 1")
}

func TestPublisherReplacesRecurringFindingResolvedByPush(t *testing.T) {
	input, client := publicationFixture(t)
	previous := previousFinding()
	client.discussions = []gitlab.Discussion{previousDiscussion(previous, true, gitlab.ResolutionCauseOutdatedByPush)}
	bundle := publicationBundle(input, []protocol.Finding{publicationFinding(&previous)})

	err := New(zaptest.NewLogger(t), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)

	require.NoError(t, err)
	require.Equal(t, 1, client.createdThreads)
	require.Empty(t, client.addedReplies)
	require.Empty(t, client.resolutionUpdates, "the outdated discussion must stay closed")
	require.True(t, client.discussions[0].Resolved)
}

func TestPublisherRepliesAndReopensRecurringFindingResolvedByHuman(t *testing.T) {
	input, client := publicationFixture(t)
	previous := previousFinding()
	client.discussions = []gitlab.Discussion{previousDiscussion(previous, true, gitlab.ResolutionCauseExplicit)}
	bundle := publicationBundle(input, []protocol.Finding{publicationFinding(&previous)})

	err := New(zaptest.NewLogger(t), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)

	require.NoError(t, err)
	require.Zero(t, client.createdThreads)
	require.Len(t, client.addedReplies, 1)
	require.Contains(t, client.addedReplies[0], previous.RecurrenceComment)
	require.True(t, isFindingMarker(trailingMarker(client.addedReplies[0])))
	require.Equal(t, []bool{false}, client.resolutionUpdates)
	require.False(t, client.discussions[0].Resolved)
}

func TestPublisherReplacesRecurringFindingPreviouslyResolvedByBot(t *testing.T) {
	input, client := publicationFixture(t)
	previous := previousFinding()
	discussion := previousDiscussion(previous, true, gitlab.ResolutionCauseExplicit)
	discussion.ResolvedBy = input.Reviewer
	client.discussions = []gitlab.Discussion{discussion}
	bundle := publicationBundle(input, []protocol.Finding{publicationFinding(&previous)})

	err := New(zaptest.NewLogger(t), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)

	require.NoError(t, err)
	require.Equal(t, 1, client.createdThreads)
	require.Empty(t, client.addedReplies)
	require.Empty(t, client.resolutionUpdates)
	require.True(t, client.discussions[0].Resolved)
}

func TestPublisherRejectsUnknownRecurringFindingResolutionCause(t *testing.T) {
	input, client := publicationFixture(t)
	previous := previousFinding()
	client.discussions = []gitlab.Discussion{previousDiscussion(previous, true, gitlab.ResolutionCauseUnknown)}
	bundle := publicationBundle(input, []protocol.Finding{publicationFinding(&previous)})

	err := New(zaptest.NewLogger(t), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)

	require.Error(t, err)
	require.Equal(t, "unknown_discussion_resolution_cause", Code(err))
	require.False(t, IsRetryable(err))
	require.Zero(t, client.createdThreads)
	require.Zero(t, client.createdNotes)
}

func TestPublisherDoesNotReopenFindingOmittedAfterAcceptedRisk(t *testing.T) {
	input, client := publicationFixture(t)
	previous := previousFinding()
	client.discussions = []gitlab.Discussion{previousDiscussion(previous, true, gitlab.ResolutionCauseExplicit)}
	bundle := publicationBundle(input, nil)

	err := New(zaptest.NewLogger(t), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)

	require.NoError(t, err)
	require.Empty(t, client.addedReplies)
	require.Empty(t, client.resolutionUpdates)
	require.Contains(t, client.discussions[len(client.discussions)-1].Notes[0].Body, "no findings were confirmed")
}

func TestPublisherPublishesRecommendationAndOwnedResolution(t *testing.T) {
	input, client := publicationFixture(t)
	previous := previousFinding()
	client.discussions = []gitlab.Discussion{previousDiscussion(previous, false, gitlab.ResolutionCauseNone)}
	bundle := publicationBundle(input, nil)
	bundle.Counts.Recommendations = 1
	bundle.Counts.Resolutions = 1
	bundle.Recommendations = []protocol.Recommendation{{Name: "document-rule.md", Body: "Document the validation boundary."}}
	bundle.Resolutions = []protocol.Resolution{{
		DiscussionID: previous.DiscussionID, NoteID: previous.NoteID, Marker: previous.Marker,
		Body: "The old cause is fixed.",
	}}

	err := New(zaptest.NewLogger(t), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)

	require.NoError(t, err)
	require.Equal(t, 2, client.createdNotes, "one recommendation and one completion note")
	require.Equal(t, []bool{true}, client.resolutionUpdates)
	require.True(t, client.discussions[0].Resolved)
}

func TestPublisherFallsBackOnlyForConfirmedInvalidPosition(t *testing.T) {
	input, client := publicationFixture(t)
	client.createThreadErr = fmt.Errorf("wrapped: %w", gitlab.ErrInvalidDiscussionPosition)
	bundle := publicationBundle(input, []protocol.Finding{publicationFinding(nil)})

	err := New(zaptest.NewLogger(t), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)

	require.NoError(t, err)
	require.Equal(t, 1, client.createdThreads)
	require.Equal(t, 2, client.createdNotes, "one fallback note and one completion note")
	require.Contains(t, client.discussions[0].Notes[0].Body, "Inline fallback")
}

func TestPublisherVerifiesMarkerAfterUncertainFindingResponse(t *testing.T) {
	input, client := publicationFixture(t)
	client.createThreadErr = context.DeadlineExceeded
	client.createThreadAccepted = true
	bundle := publicationBundle(input, []protocol.Finding{publicationFinding(nil)})

	err := New(zaptest.NewLogger(t), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)

	require.NoError(t, err)
	require.Equal(t, 1, client.createdThreads)
	require.Equal(t, 1, client.createdNotes)
	require.True(t, client.removedReviewer)
}

func TestPublisherRejectsStaleDiffBeforeMutation(t *testing.T) {
	input, client := publicationFixture(t)
	client.mergeRequest.DiffRefs.HeadSHA = strings.Repeat("d", 40)
	bundle := publicationBundle(input, []protocol.Finding{publicationFinding(nil)})

	err := New(zaptest.NewLogger(t), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrStaleReview))
	require.Equal(t, "stale_diff", Code(err))
	require.Zero(t, client.createdThreads)
	require.Zero(t, client.createdNotes)
}

func publicationFixture(t *testing.T) (review.Input, *publicationClientMock) {
	t.Helper()
	reviewer := gitlab.User{ID: 42, Username: "orpheus"}
	mr := gitlab.MergeRequest{
		ID: 100, ProjectID: 74, IID: 17, State: "opened", SourceBranch: "feature", TargetBranch: "main",
		Reviewers: []gitlab.User{reviewer},
		DiffRefs: gitlab.DiffRefs{
			BaseSHA: strings.Repeat("a", 40), StartSHA: strings.Repeat("b", 40), HeadSHA: strings.Repeat("c", 40),
		},
	}
	diffFingerprint, err := review.DiffFingerprint("https://gitlab.example.test", mr)
	require.NoError(t, err)
	input := review.Input{
		MergeRequest: mr, Project: gitlab.Project{ID: 74, PathWithNamespace: "group/project"}, Reviewer: reviewer,
		MRKey: "gitlab.example.test:74!17", DiffFingerprint: diffFingerprint, ReviewFingerprint: strings.Repeat("e", 64),
	}
	client := &publicationClientMock{
		mergeRequest: mr,
		diffs: []gitlab.DiffFile{{
			OldPath: "service.go", NewPath: "service.go", Diff: "@@ -1,2 +1,2 @@\n-old\n+new\n context\n",
		}},
	}
	return input, client
}

func publicationBundle(input review.Input, findings []protocol.Finding) protocol.Bundle {
	return protocol.Bundle{
		Review: protocol.ReviewIdentityV1{
			DiffFingerprint: input.DiffFingerprint, ReviewFingerprint: input.ReviewFingerprint,
		},
		Counts:    protocol.Counts{Confirmed: len(findings)},
		Confirmed: findings,
	}
}

func publicationFinding(previous *protocol.PreviousFinding) protocol.Finding {
	return protocol.Finding{
		ID: "F-0001", Path: "service.go", Line: 1, Severity: "warning", Title: "Missing validation",
		Source: "project policy", Body: "The new value reaches the parser without validation.", Previous: previous,
	}
}

func previousFinding() protocol.PreviousFinding {
	return protocol.PreviousFinding{
		DiscussionID: "old-thread", NoteID: 101,
		Marker:            "<!-- orpheus-review-finding:" + strings.Repeat("a", 64) + " -->",
		RecurrenceComment: "The parser path was rechecked and still accepts the unvalidated value; validate it before parsing.",
	}
}

func previousDiscussion(previous protocol.PreviousFinding, resolved bool, cause gitlab.ResolutionCause) gitlab.Discussion {
	return gitlab.Discussion{
		ID: previous.DiscussionID, Resolvable: true, Resolved: resolved, ResolutionCause: cause,
		ResolvedBy: gitlab.User{ID: 7, Username: "developer"},
		Notes: []gitlab.Note{{
			ID: previous.NoteID, Body: "Old finding\n\n" + previous.Marker,
			Author: gitlab.User{ID: 42, Username: "orpheus"}, Resolvable: true, Resolved: resolved,
		}},
	}
}
