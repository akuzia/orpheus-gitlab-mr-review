package publication

import (
	"context"
	"errors"
	"html"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"
)

// Failure contains a classification, an optional public Orpheus error message,
// and a public session identity. Message must never contain raw application
// errors, HTTP responses, credentials, or hook output.
type Failure struct {
	Notes     review.NoteTemplates
	Code      string
	Message   string
	SessionID string
}

// Finalize recovers a terminal publication before any new analysis admission.
// A marker is durable evidence; removing the reviewer is independently retried.
func (p *Publisher) Finalize(ctx context.Context, input review.Input) (bool, error) {
	if err := p.ensureCurrent(ctx, input); err != nil {
		if errors.Is(err, ErrReviewInactive) {
			return true, nil
		}
		return false, err
	}
	discussions, err := p.refreshDiscussions(ctx, input)
	if err != nil {
		return false, err
	}
	if !hasOwnedMarker(discussions, input.Reviewer.ID, CompletionMarker(input.ReviewFingerprint)) &&
		!hasOwnedMarker(discussions, input.Reviewer.ID, ErrorMarker(input.ReviewFingerprint)) &&
		!hasOwnedMarker(discussions, input.Reviewer.ID, SkippedMarker(review.AssignmentKey(input))) {
		return false, nil
	}
	return true, p.removeReviewer(ctx, input)
}

func (p *Publisher) PublishError(ctx context.Context, input review.Input, failure Failure) error {
	if finalized, err := p.Finalize(ctx, input); err != nil || finalized {
		return err
	}
	discussions, err := p.refreshDiscussions(ctx, input)
	if err != nil {
		return err
	}
	marker := ErrorMarker(input.ReviewFingerprint)
	reason := strings.TrimSpace(failure.Message)
	if reason == "" {
		reason = safeFailureReason(failure.Code)
	}
	reason = escapeFailureMessage(reason)
	noteTemplate := failure.Notes.Fail
	if noteTemplate == "" {
		noteTemplate = "⚠️ Automated review could not be completed: {{ .Reason }}."
	}
	body, err := review.RenderNote(noteTemplate, review.NoteData{Reason: reason, SessionURL: p.sessionURL(failure.SessionID)})
	if err != nil {
		return permanent("invalid_failure_note", err)
	}
	discussions, err = p.ensureNote(ctx, input, body+"\n\n"+marker, marker, discussions)
	if err != nil {
		return err
	}
	if !hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
		return unconfirmed("error_marker_missing")
	}
	return p.removeReviewer(ctx, input)
}

// Display public error messages literally, without interpreting Markdown, HTML,
// mentions, or embedded service markers from an upstream provider.
func escapeFailureMessage(message string) string {
	message = strings.ReplaceAll(strings.ReplaceAll(message, "\r\n", "\n"), "\r", "\n")
	message = html.EscapeString(message)
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "~", "\\~", "@", "&#64;").Replace(message)
}

// Discard never publishes a result. For a changed or incomplete diff it removes
// only the bot from the freshly fetched version, guarded again in the adapter.
// Closed MRs and requests already removed by a human require no mutation.
func (p *Publisher) Discard(ctx context.Context, input review.Input) error {
	current, err := p.client.GetMergeRequest(ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID)
	if err != nil {
		return classify("read_discard_state", err)
	}
	if current.State != "opened" || !hasReviewer(current.Reviewers, input.Reviewer.ID) {
		return nil
	}
	if err := p.client.RemoveMergeRequestReviewer(ctx, current.ProjectID, current.IID, input.Reviewer.ID, current.DiffRefs); err != nil {
		return classify("discard_review_request", err)
	}
	return nil
}

func (p *Publisher) ensureNote(ctx context.Context, input review.Input, body, marker string, discussions []gitlab.Discussion) ([]gitlab.Discussion, error) {
	if hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
		return discussions, nil
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return discussions, err
	}
	mutationErr := p.client.CreateMergeRequestNote(ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, body)
	refreshed, err := p.refreshDiscussions(ctx, input)
	if err != nil {
		return discussions, classify("verify_note", errors.Join(mutationErr, err))
	}
	if hasOwnedMarker(refreshed, input.Reviewer.ID, marker) {
		return refreshed, nil
	}
	if mutationErr != nil {
		return refreshed, classify("create_note", mutationErr)
	}
	return refreshed, unconfirmed("note_marker_missing")
}

func (p *Publisher) ensureReply(ctx context.Context, input review.Input, discussionID, body, marker string, discussions []gitlab.Discussion) ([]gitlab.Discussion, error) {
	for _, discussion := range discussions {
		if discussion.ID == discussionID && hasOwnedMarker([]gitlab.Discussion{discussion}, input.Reviewer.ID, marker) {
			return discussions, nil
		}
	}
	if err := p.ensureCurrent(ctx, input); err != nil {
		return discussions, err
	}
	mutationErr := p.client.AddMergeRequestDiscussionNote(ctx, input.MergeRequest.ProjectID, input.MergeRequest.IID, discussionID, body)
	refreshed, err := p.refreshDiscussions(ctx, input)
	if err != nil {
		return discussions, classify("verify_resolution_reply", errors.Join(mutationErr, err))
	}
	for _, discussion := range refreshed {
		if discussion.ID == discussionID && hasOwnedMarker([]gitlab.Discussion{discussion}, input.Reviewer.ID, marker) {
			return refreshed, nil
		}
	}
	if mutationErr != nil {
		return refreshed, classify("create_resolution_reply", mutationErr)
	}
	return refreshed, unconfirmed("resolution_marker_missing")
}

func unconfirmed(code string) error {
	return &Error{Code: code, Retryable: true, Err: errors.New("GitLab mutation is not yet confirmed")}
}

func (p *Publisher) sessionURL(sessionID string) string {
	if _, err := uuid.Parse(sessionID); err != nil {
		return ""
	}
	base, err := url.Parse(p.webBaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return ""
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/sessions/" + sessionID
	base.RawPath = ""
	// Escape Markdown delimiters as well as URL delimiters.
	return strings.NewReplacer("(", "%28", ")", "%29").Replace(base.String())
}

func safeFailureReason(code string) string {
	switch code {
	case "session_request_too_large":
		return "the review request exceeds the configured size limit"
	case "invalid_session_contract":
		return "the review environment could not be prepared"
	case "run_failed", "run_cancelled", "agent_not_completed":
		return "the review run did not finish successfully"
	case "after_run_missing", "after_run_failed", "after_run_output_incomplete", "multiple_after_run_results":
		return "the review result could not be collected and verified (" + code + ")"
	case "unknown_discussion_resolution_cause":
		return "cannot determine how a GitLab discussion was resolved (unknown_discussion_resolution_cause)"
	case "invalid_discussion_resolution_state":
		return "a GitLab discussion is resolved without a resolution cause (invalid_discussion_resolution_state)"
	case "previous_finding_not_owned", "resolution_not_owned":
		return "the target discussion is not an Orpheus finding (" + code + ")"
	case "previous_finding_disappeared", "resolution_target_disappeared":
		return "the target GitLab discussion disappeared during publication (" + code + ")"
	case "invalid_session_metadata", "session_metadata_mismatch":
		return "the saved session metadata is invalid or does not match this review (" + code + ")"
	case "publisher_not_configured", "invalid_success_note", "invalid_failure_note":
		return "the review publication configuration is invalid (" + code + ")"
	case "list_discussions", "list_diffs", "read_current_merge_request", "create_finding",
		"reopen_discussion", "resolve_discussion", "create_resolution_reply", "create_note", "remove_reviewer":
		return "a GitLab operation failed during review publication (" + code + ")"
	case "envelope_too_large", "invalid_envelope_json", "unsupported_envelope_schema", "unsupported_envelope_encoding",
		"declared_payload_size_out_of_bounds", "invalid_payload_base64", "payload_size_mismatch", "payload_digest_mismatch",
		"invalid_payload_json", "payload_too_large", "unsupported_bundle_schema", "bundle_not_ready", "pending_findings",
		"count_mismatch", "too_many_items", "workflow_identity_mismatch", "review_identity_mismatch", "review_fingerprint_mismatch",
		"findings_not_sorted", "recommendations_not_sorted", "resolutions_not_sorted", "invalid_recommendation", "invalid_resolution",
		"invalid_finding_id", "duplicate_finding_id", "invalid_finding_path", "invalid_finding_line", "invalid_finding_severity",
		"invalid_finding_content", "invalid_previous_finding", "invalid_payload_compression":
		return "the review result failed validation (" + code + ")"
	default:
		return "the review result could not be safely validated or published"
	}
}

// PublishSkipped records the decision durably before removing the bot reviewer.
func (p *Publisher) PublishSkipped(ctx context.Context, input review.Input) error {
	if finalized, err := p.Finalize(ctx, input); err != nil || finalized {
		return err
	}
	discussions, err := p.refreshDiscussions(ctx, input)
	if err != nil {
		return err
	}
	marker := SkippedMarker(review.AssignmentKey(input))
	body := "⏭️ Automated review was skipped: no workflow is configured for this project.\n\n" + marker
	discussions, err = p.ensureNote(ctx, input, body, marker, discussions)
	if err != nil {
		return err
	}
	if !hasOwnedMarker(discussions, input.Reviewer.ID, marker) {
		return unconfirmed("skipped_marker_missing")
	}
	return p.removeReviewer(ctx, input)
}
