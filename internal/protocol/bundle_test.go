package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBundleRoundTrip(t *testing.T) {
	t.Parallel()

	bundle, expected := validBundle()
	encoded, err := Encode(bundle, DefaultLimits())
	require.NoError(t, err)

	decoded, err := Decode(encoded, DefaultLimits(), expected)

	require.NoError(t, err)
	require.Equal(t, bundle, decoded)
}

func TestDecodeRejectsEnvelopeWithTrailingOutput(t *testing.T) {
	t.Parallel()

	bundle, expected := validBundle()
	encoded, err := Encode(bundle, DefaultLimits())
	require.NoError(t, err)
	encoded = append(encoded, []byte("\nunexpected output")...)

	_, err = Decode(encoded, DefaultLimits(), expected)

	require.Equal(t, "invalid_envelope_json", ErrorCode(err))
}

func TestDecodeRejectsTamperedPayload(t *testing.T) {
	t.Parallel()

	bundle, expected := validBundle()
	encoded, err := Encode(bundle, DefaultLimits())
	require.NoError(t, err)
	var envelope Envelope
	require.NoError(t, json.Unmarshal(encoded, &envelope))
	envelope.PayloadSHA256 = "sha256:" + strings.Repeat("0", 64)
	encoded, err = json.Marshal(envelope)
	require.NoError(t, err)

	_, err = Decode(encoded, DefaultLimits(), expected)

	require.Equal(t, "payload_digest_mismatch", ErrorCode(err))
}

func TestValidateRejectsUnsafeOrInconsistentBundle(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		change func(*Bundle)
		code   string
	}{
		"pending finding": {
			change: func(bundle *Bundle) { bundle.Counts.Pending = 1 },
			code:   "pending_findings",
		},
		"count mismatch": {
			change: func(bundle *Bundle) { bundle.Counts.Confirmed = 0 },
			code:   "count_mismatch",
		},
		"identity mismatch": {
			change: func(bundle *Bundle) { bundle.Identity.ProjectID++ },
			code:   "review_identity_mismatch",
		},
		"path traversal": {
			change: func(bundle *Bundle) { bundle.Confirmed[0].Path = "../secret" },
			code:   "invalid_finding_path",
		},
		"negative rejected count": {
			change: func(bundle *Bundle) { bundle.Counts.Rejected = -1 },
			code:   "count_mismatch",
		},
		"incomplete previous finding": {
			change: func(bundle *Bundle) {
				bundle.Confirmed[0].Previous = &PreviousFinding{DiscussionID: "discussion-1"}
			},
			code: "invalid_previous_finding",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bundle, expected := validBundle()
			test.change(&bundle)

			err := Validate(bundle, DefaultLimits(), expected)

			require.Equal(t, test.code, ErrorCode(err))
		})
	}
}

func TestDecodeAppliesEnvelopeAndPayloadLimits(t *testing.T) {
	t.Parallel()

	bundle, expected := validBundle()
	encoded, err := Encode(bundle, DefaultLimits())
	require.NoError(t, err)

	limits := DefaultLimits()
	limits.MaxEnvelopeBytes = len(encoded) - 1
	_, err = Decode(encoded, limits, expected)
	require.Equal(t, "envelope_too_large", ErrorCode(err))

	limits = DefaultLimits()
	limits.MaxPayloadBytes = 1
	_, err = Decode(encoded, limits, expected)
	require.Equal(t, "declared_payload_size_out_of_bounds", ErrorCode(err))
}

func validBundle() (Bundle, Expected) {
	workflow := WorkflowIdentity{
		ID:           "gitlab-mr-review",
		Revision:     "sha256:" + strings.Repeat("a", 64),
		HelperSHA256: "sha256:" + strings.Repeat("b", 64),
	}
	identity := ReviewIdentity{
		GitLabHost:      "https://gitlab.example.com",
		ProjectID:       74,
		MergeRequestIID: 2989,
		ReviewerUserID:  42,
	}
	review := ReviewIdentityV1{
		DiffFingerprint:   strings.Repeat("d", 64),
		ReviewFingerprint: strings.Repeat("e", 64),
	}
	bundle := Bundle{
		SchemaVersion: SchemaVersion,
		Stage:         StageReady,
		Workflow:      workflow,
		Identity:      identity,
		Review:        review,
		Counts: Counts{
			Confirmed:       1,
			Recommendations: 1,
			Resolutions:     1,
		},
		Confirmed: []Finding{{
			ID:       "F-0001",
			Path:     "internal/service.go",
			Line:     42,
			Severity: "warning",
			Title:    "Incomplete validation",
			Source:   "AGENTS.md",
			Body:     "The input can bypass validation.\n",
		}},
		Recommendations: []Recommendation{{
			Name: "document-validation.md",
			Body: "Document the validation invariant.\n",
		}},
		Resolutions: []Resolution{{
			DiscussionID: "discussion-1",
			NoteID:       123,
			Marker:       "<!-- orpheus-review-finding:abc -->",
			Body:         "The pinned diff fixes the original cause.\n",
		}},
	}

	return bundle, Expected{Workflow: workflow, Identity: identity, Review: review}
}
