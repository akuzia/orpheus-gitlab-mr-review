package review

import (
	"testing"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"

	"github.com/stretchr/testify/require"
)

func TestMergeRequestKey(t *testing.T) {
	t.Parallel()

	key, err := MergeRequestKey(" https://gitlab.example.com/root/ ", 42, 17)
	require.NoError(t, err)
	require.Equal(t, "https://gitlab.example.com/root:42!17", key)
}

func TestDiffFingerprintIsCanonical(t *testing.T) {
	t.Parallel()

	mergeRequest := fingerprintMergeRequest()
	fingerprint, err := DiffFingerprint(" https://gitlab.example.com/ ", mergeRequest)
	require.NoError(t, err)
	require.Equal(t, "b5ef596faf3809f8b1270c3bb301a5edd6e7ea9d40d3f2b4feb3de21174cbafb", fingerprint)

	same, err := DiffFingerprint("https://gitlab.example.com", mergeRequest)
	require.NoError(t, err)
	require.Equal(t, fingerprint, same)
}

func TestReviewFingerprintUsesOnlySignificantNotes(t *testing.T) {
	t.Parallel()

	reviewer := gitlab.User{ID: 123, Username: "orpheus"}
	notes := []gitlab.Note{
		{ID: 30, Body: "bot output", Author: reviewer, Resolvable: true},
		{ID: 20, Body: "ordinary note", Author: gitlab.User{ID: 7}},
		{ID: 13, Body: "resolved   this thread", Author: gitlab.User{ID: 7}, System: true},
		{ID: 10, Body: " please  fix\nthis ", Author: gitlab.User{ID: 7}, Resolvable: true, Resolved: true},
		{ID: 12, Body: "changed title", Author: gitlab.User{ID: 7}, System: true},
		{ID: 11, Body: "requested review from @orpheus", Author: gitlab.User{ID: 7}, System: true},
	}

	selected := reviewFingerprintNotes(notes, reviewer)
	require.Equal(t, []fingerprintNote{
		{ID: 10, AuthorID: 7, Body: "please fix this", Resolved: true},
		{ID: 11, AuthorID: 7, Body: "requested review from @orpheus"},
		{ID: 13, AuthorID: 7, Body: "resolved this thread"},
	}, selected)

	fingerprint, err := ReviewFingerprint("https://gitlab.example.com", fingerprintMergeRequest(), notes, reviewer)
	require.NoError(t, err)
	require.Equal(t, "8a155985653a52c968d1aeafe53b4b2a8b1722a096289ce249c9728b75de6a2b", fingerprint)

	reordered := []gitlab.Note{notes[5], notes[3], notes[2], notes[0], notes[4], notes[1]}
	same, err := ReviewFingerprint("https://gitlab.example.com/", fingerprintMergeRequest(), reordered, reviewer)
	require.NoError(t, err)
	require.Equal(t, fingerprint, same)

	withAnotherBotNote := append(notes, gitlab.Note{
		ID: 31, Body: "another bot output", Author: reviewer, Resolvable: true,
	})
	same, err = ReviewFingerprint("https://gitlab.example.com", fingerprintMergeRequest(), withAnotherBotNote, reviewer)
	require.NoError(t, err)
	require.Equal(t, fingerprint, same)

	changed := fingerprintMergeRequest()
	changed.Description = "new description"
	different, err := ReviewFingerprint("https://gitlab.example.com", changed, notes, reviewer)
	require.NoError(t, err)
	require.NotEqual(t, fingerprint, different)
}

func fingerprintMergeRequest() gitlab.MergeRequest {
	return gitlab.MergeRequest{
		ProjectID:    42,
		IID:          17,
		Description:  "Review this change",
		SourceBranch: "feature/x",
		TargetBranch: "main",
		DiffRefs: gitlab.DiffRefs{
			BaseSHA:  "base",
			StartSHA: "start",
			HeadSHA:  "head",
		},
	}
}
