package publication

import (
	"context"
	"strings"
	"testing"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/protocol"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestResolutionResumesAfterReplyWithoutDuplicatingIt(t *testing.T) {
	input, client := publicationFixture(t)
	previous := previousFinding()
	client.discussions = []gitlab.Discussion{previousDiscussion(previous, false, gitlab.ResolutionCauseNone)}
	resolution := protocol.Resolution{DiscussionID: previous.DiscussionID, NoteID: previous.NoteID, Marker: previous.Marker, Body: "Validation now rejects the invalid value before parsing."}
	bundle := publicationBundle(input, nil)
	bundle.Resolutions = []protocol.Resolution{resolution}
	client.resolveErr = context.DeadlineExceeded

	err := New(zap.NewNop(), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)
	require.True(t, IsRetryable(err))
	require.Len(t, client.addedReplies, 1)
	require.Equal(t, ResolutionMarker(input.ReviewFingerprint, resolution), trailingMarker(client.addedReplies[0]))
	require.Zero(t, client.createdNotes)
	require.False(t, client.removedReviewer)

	client.resolveErr = nil
	err = New(zap.NewNop(), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)
	require.NoError(t, err)
	require.Len(t, client.addedReplies, 1)
	require.True(t, client.discussions[0].Resolved)
	require.True(t, client.removedReviewer)
}

func TestResolutionVerifiesUncertainReplyAndResolve(t *testing.T) {
	input, client := publicationFixture(t)
	previous := previousFinding()
	client.discussions = []gitlab.Discussion{previousDiscussion(previous, false, gitlab.ResolutionCauseNone)}
	client.addReplyErr, client.resolveErr = context.DeadlineExceeded, context.DeadlineExceeded
	client.addReplyAccepted, client.resolveAccepted = true, true
	bundle := publicationBundle(input, nil)
	bundle.Resolutions = []protocol.Resolution{{DiscussionID: previous.DiscussionID, NoteID: previous.NoteID, Marker: previous.Marker, Body: "The cause is fixed."}}

	require.NoError(t, New(zap.NewNop(), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle))
	require.Len(t, client.addedReplies, 1)
	require.Len(t, client.resolutionUpdates, 1)
	require.True(t, client.removedReviewer)
}

func TestRecurringFindingVerifiesUncertainReplyAndReopen(t *testing.T) {
	input, client := publicationFixture(t)
	previous := previousFinding()
	client.discussions = []gitlab.Discussion{previousDiscussion(previous, true, gitlab.ResolutionCauseExplicit)}
	client.addReplyErr, client.resolveErr = context.DeadlineExceeded, context.DeadlineExceeded
	client.addReplyAccepted, client.resolveAccepted = true, true
	bundle := publicationBundle(input, []protocol.Finding{publicationFinding(&previous)})

	require.NoError(t, New(zap.NewNop(), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle))
	require.Len(t, client.addedReplies, 1)
	require.Equal(t, FindingMarker(input.DiffFingerprint, bundle.Confirmed[0]), trailingMarker(client.addedReplies[0]))
	require.Equal(t, []bool{false}, client.resolutionUpdates)
	require.False(t, client.discussions[0].Resolved)
	require.True(t, client.removedReviewer)
}

func TestPublisherRejectsResolutionOfAnotherAuthorsFinding(t *testing.T) {
	input, client := publicationFixture(t)
	previous := previousFinding()
	thread := previousDiscussion(previous, false, gitlab.ResolutionCauseNone)
	thread.Notes[0].Author.ID = 7
	client.discussions = []gitlab.Discussion{thread}
	bundle := publicationBundle(input, nil)
	bundle.Resolutions = []protocol.Resolution{{DiscussionID: previous.DiscussionID, NoteID: previous.NoteID, Marker: previous.Marker, Body: "Fixed."}}

	err := New(zap.NewNop(), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)
	require.Equal(t, "resolution_not_owned", Code(err))
	require.Empty(t, client.addedReplies)
	require.Empty(t, client.resolutionUpdates)
	require.Zero(t, client.createdNotes)
}

func TestErrorPublicationIsSafeAndRecoversRemovalAfterRestart(t *testing.T) {
	input, client := publicationFixture(t)
	client.removeErr = context.DeadlineExceeded
	failure := Failure{Code: "secret token raw output http://internal/stack", SessionID: "00000000-0000-4000-8000-000000000001"}
	publisher := New(zap.NewNop(), client, "https://gitlab.example.test", "https://orpheus.example.test/review")

	err := publisher.PublishError(t.Context(), input, failure)
	require.True(t, IsRetryable(err))
	require.Equal(t, 1, client.createdNotes)
	require.False(t, client.removedReviewer)
	body := client.discussions[0].Notes[0].Body
	require.NotContains(t, body, failure.Code)
	require.Contains(t, body, "[Review details in Orpheus](https://orpheus.example.test/review/sessions/"+failure.SessionID+")")
	require.Equal(t, ErrorMarker(input.ReviewFingerprint), trailingMarker(body))

	client.removeErr = nil
	finalized, err := New(zap.NewNop(), client, "https://gitlab.example.test").Finalize(t.Context(), input)
	require.NoError(t, err)
	require.True(t, finalized)
	require.Equal(t, 1, client.createdNotes)
	require.True(t, client.removedReviewer)
}

func TestErrorPublicationWithoutSessionAndAfterUncertainResponse(t *testing.T) {
	input, client := publicationFixture(t)
	client.createNoteErr, client.createNoteAccepted = context.DeadlineExceeded, true
	publisher := New(zap.NewNop(), client, "https://gitlab.example.test")
	require.NoError(t, publisher.PublishError(t.Context(), input, Failure{Code: "session_request_too_large"}))
	require.NoError(t, publisher.PublishError(t.Context(), input, Failure{Code: "run_failed"}))
	require.Equal(t, 1, client.createdNotes)
	require.NotContains(t, client.discussions[0].Notes[0].Body, "Orpheus](")
	require.True(t, client.removedReviewer)
}

func TestTerminalMarkersRequireOwnedTrailingNote(t *testing.T) {
	for _, markerKind := range []string{"completion", "error"} {
		for _, forged := range []string{"other_author", "not_trailing"} {
			t.Run(markerKind+"/"+forged, func(t *testing.T) {
				input, client := publicationFixture(t)
				marker := CompletionMarker(input.ReviewFingerprint)
				if markerKind == "error" {
					marker = ErrorMarker(input.ReviewFingerprint)
				}
				note := gitlab.Note{Author: input.Reviewer, Body: marker}
				if forged == "other_author" {
					note.Author.ID = 7
				} else {
					note.Body += "\nadditional text"
				}
				client.discussions = []gitlab.Discussion{{Notes: []gitlab.Note{note}}}
				finalized, err := New(zap.NewNop(), client, "https://gitlab.example.test").Finalize(t.Context(), input)
				require.NoError(t, err)
				require.False(t, finalized)
				require.False(t, client.removedReviewer)
			})
		}
	}
}

func TestUnconfirmedCompletionOrErrorNeverRemovesReviewer(t *testing.T) {
	for _, errorNote := range []bool{false, true} {
		input, client := publicationFixture(t)
		client.hideCreatedNotes = true
		publisher := New(zap.NewNop(), client, "https://gitlab.example.test")
		var err error
		if errorNote {
			err = publisher.PublishError(t.Context(), input, Failure{Code: "run_failed"})
		} else {
			err = publisher.Publish(t.Context(), input, publicationBundle(input, nil))
		}
		require.Error(t, err)
		require.True(t, IsRetryable(err))
		require.False(t, client.removedReviewer)
	}
}

func TestPublisherChecksDiffAfterCompletionBeforeRemovingReviewer(t *testing.T) {
	input, client := publicationFixture(t)
	client.afterMutation = func() { client.mergeRequest.DiffRefs.HeadSHA = strings.Repeat("d", 40) }
	err := New(zap.NewNop(), client, "https://gitlab.example.test").Publish(t.Context(), input, publicationBundle(input, nil))
	require.ErrorIs(t, err, ErrStaleReview)
	require.Equal(t, 1, client.createdNotes)
	require.False(t, client.removedReviewer)
}

func TestPublisherRejectsIncompleteDiffRefsAndInactiveMR(t *testing.T) {
	for _, state := range []string{"incomplete", "closed", "unassigned"} {
		t.Run(state, func(t *testing.T) {
			input, client := publicationFixture(t)
			switch state {
			case "incomplete":
				client.mergeRequest.DiffRefs.BaseSHA = ""
			case "closed":
				client.mergeRequest.State = "closed"
			case "unassigned":
				client.mergeRequest.Reviewers = nil
			}
			err := New(zap.NewNop(), client, "https://gitlab.example.test").Publish(t.Context(), input, publicationBundle(input, nil))
			require.Error(t, err)
			require.Zero(t, client.createdNotes)
			require.False(t, client.removedReviewer)
		})
	}
}

func TestFallbackMarkerSurvivesRetryBeforeCompletion(t *testing.T) {
	input, client := publicationFixture(t)
	client.diffs = nil
	bundle := publicationBundle(input, []protocol.Finding{publicationFinding(nil)})
	client.afterMutation = func() { client.createNoteErr = context.DeadlineExceeded }
	err := New(zap.NewNop(), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle)
	require.True(t, IsRetryable(err))
	require.Equal(t, FindingMarker(input.DiffFingerprint, bundle.Confirmed[0]), trailingMarker(client.discussions[0].Notes[0].Body))
	client.afterMutation, client.createNoteErr = nil, nil
	require.NoError(t, New(zap.NewNop(), client, "https://gitlab.example.test").Publish(t.Context(), input, bundle))
	require.Len(t, client.discussions, 2, "one fallback and one completion")
}
