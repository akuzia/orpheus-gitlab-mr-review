package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/config"
	gitlabapi "gitlab.com/gitlab-org/api/client-go/v3"
)

const userAgent = "orpheus-gitlab-mr-review"

const pageSize int64 = 100

var ErrReviewInputChanged = errors.New("merge request changed while loading review input")
var ErrInvalidDiscussionPosition = errors.New("GitLab rejected inline discussion position")

func IsRetryable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrReviewInputChanged) {
		return true
	}
	var responseError *gitlabapi.ErrorResponse
	if errors.As(err, &responseError) {
		return responseError.StatusCode == http.StatusRequestTimeout ||
			responseError.StatusCode == http.StatusTooManyRequests ||
			responseError.StatusCode >= http.StatusInternalServerError
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

type Client struct {
	api *gitlabapi.Client
}

func New(cfg config.Config) (*Client, error) {
	httpClient := &http.Client{
		Timeout: cfg.HTTPTimeout,
	}

	api, err := gitlabapi.NewClient(
		cfg.GitLab.Token,
		gitlabapi.WithBaseURL(cfg.GitLab.BaseURL),
		gitlabapi.WithHTTPClient(httpClient),
		gitlabapi.WithOnlyIdempotentRetries(),
		gitlabapi.WithUserAgent(userAgent),
	)
	if err != nil {
		return nil, fmt.Errorf("create GitLab client: %w", err)
	}

	return &Client{api: api}, nil
}

func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	user, _, err := c.api.Users.CurrentUser(gitlabapi.WithContext(ctx))
	if err != nil {
		return User{}, fmt.Errorf("get current GitLab user: %w", err)
	}
	if user == nil {
		return User{}, fmt.Errorf("get current GitLab user: empty response")
	}
	if user.ID <= 0 {
		return User{}, fmt.Errorf("get current GitLab user: invalid user ID %d", user.ID)
	}

	return User{
		ID:       user.ID,
		Username: user.Username,
	}, nil
}

func (c *Client) ListMergeRequestsForReview(ctx context.Context, reviewerID int64) ([]MergeRequest, error) {
	if reviewerID <= 0 {
		return nil, fmt.Errorf("list GitLab merge requests for review: invalid reviewer ID %d", reviewerID)
	}

	var mergeRequests []MergeRequest
	page := int64(1)

	for {
		items, response, err := c.api.MergeRequests.ListMergeRequests(
			&gitlabapi.ListMergeRequestsOptions{
				Page:       page,
				PerPage:    pageSize,
				State:      new("opened"),
				Scope:      new("all"),
				ReviewerID: gitlabapi.ReviewerID(reviewerID),
			},
			gitlabapi.WithContext(ctx),
		)
		if err != nil {
			return nil, fmt.Errorf("list GitLab merge requests for review on page %d: %w", page, err)
		}

		for _, item := range items {
			if item == nil {
				continue
			}

			mergeRequests = append(mergeRequests, MergeRequest{
				ID:           item.ID,
				ProjectID:    item.ProjectID,
				IID:          item.IID,
				Title:        item.Title,
				Description:  item.Description,
				State:        item.State,
				SourceBranch: item.SourceBranch,
				TargetBranch: item.TargetBranch,
				WebURL:       item.WebURL,
				Draft:        item.Draft,
			})
		}

		if response == nil || response.NextPage == 0 {
			break
		}
		page = response.NextPage
	}

	return mergeRequests, nil
}

func (c *Client) GetMergeRequest(ctx context.Context, projectID, iid int64) (MergeRequest, error) {
	item, _, err := c.api.MergeRequests.GetMergeRequest(
		projectID,
		iid,
		&gitlabapi.GetMergeRequestsOptions{},
		gitlabapi.WithContext(ctx),
	)
	if err != nil {
		return MergeRequest{}, fmt.Errorf("get GitLab merge request %d!%d: %w", projectID, iid, err)
	}
	if item == nil {
		return MergeRequest{}, fmt.Errorf("get GitLab merge request %d!%d: empty response", projectID, iid)
	}

	return mergeRequestFromAPI(item), nil
}

func (c *Client) GetProject(ctx context.Context, projectID int64) (Project, error) {
	item, _, err := c.api.Projects.GetProject(
		projectID,
		&gitlabapi.GetProjectOptions{},
		gitlabapi.WithContext(ctx),
	)
	if err != nil {
		return Project{}, fmt.Errorf("get GitLab project %d: %w", projectID, err)
	}
	if item == nil {
		return Project{}, fmt.Errorf("get GitLab project %d: empty response", projectID)
	}

	return Project{
		ID:                item.ID,
		PathWithNamespace: item.PathWithNamespace,
		HTTPURLToRepo:     item.HTTPURLToRepo,
		SSHURLToRepo:      item.SSHURLToRepo,
		WebURL:            item.WebURL,
		Archived:          item.Archived,
	}, nil
}

func (c *Client) ListMergeRequestNotes(ctx context.Context, projectID, iid int64) ([]Note, error) {
	var notes []Note
	page := int64(1)

	for {
		items, response, err := c.api.Notes.ListMergeRequestNotes(
			projectID,
			iid,
			&gitlabapi.ListMergeRequestNotesOptions{
				ListOptions: gitlabapi.ListOptions{Page: page, PerPage: pageSize},
			},
			gitlabapi.WithContext(ctx),
		)
		if err != nil {
			return nil, fmt.Errorf("list GitLab merge request %d!%d notes on page %d: %w", projectID, iid, page, err)
		}
		for _, item := range items {
			if item != nil {
				notes = append(notes, noteFromAPI(item))
			}
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		page = response.NextPage
	}

	return notes, nil
}

func (c *Client) ListMergeRequestDiscussions(ctx context.Context, projectID, iid int64) ([]Discussion, error) {
	var discussions []Discussion
	page := int64(1)

	for {
		request, err := c.api.NewRequest(
			http.MethodGet,
			fmt.Sprintf("projects/%d/merge_requests/%d/discussions", projectID, iid),
			&gitlabapi.ListOptions{Page: page, PerPage: pageSize},
			[]gitlabapi.RequestOptionFunc{gitlabapi.WithContext(ctx)},
		)
		if err != nil {
			return nil, fmt.Errorf("build GitLab merge request %d!%d discussions request: %w", projectID, iid, err)
		}
		var items []discussionFromAPI
		response, err := c.api.Do(request, &items)
		if err != nil {
			return nil, fmt.Errorf("list GitLab merge request %d!%d discussions on page %d: %w", projectID, iid, page, err)
		}
		for _, item := range items {
			discussion := Discussion{
				ID:             item.ID,
				IndividualNote: item.IndividualNote,
				Resolvable:     item.Resolvable,
			}
			for _, note := range item.Notes {
				discussion.Notes = append(discussion.Notes, noteFromDiscussionAPI(note))
			}
			classifyDiscussionResolution(&discussion, item)
			discussions = append(discussions, discussion)
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		page = response.NextPage
	}

	return discussions, nil
}

func (c *Client) ListMergeRequestDiffs(ctx context.Context, projectID, iid int64) ([]DiffFile, error) {
	var diffs []DiffFile
	page := int64(1)
	for {
		items, response, err := c.api.MergeRequests.ListMergeRequestDiffs(
			projectID,
			iid,
			&gitlabapi.ListMergeRequestDiffsOptions{
				ListOptions: gitlabapi.ListOptions{Page: page, PerPage: pageSize},
			},
			gitlabapi.WithContext(ctx),
		)
		if err != nil {
			return nil, fmt.Errorf("list GitLab merge request %d!%d diffs on page %d: %w", projectID, iid, page, err)
		}
		for _, item := range items {
			if item == nil {
				continue
			}
			diffs = append(diffs, DiffFile{
				OldPath: item.OldPath, NewPath: item.NewPath, Diff: item.Diff,
				NewFile: item.NewFile, RenamedFile: item.RenamedFile, DeletedFile: item.DeletedFile,
				Collapsed: item.Collapsed, TooLarge: item.TooLarge,
			})
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		page = response.NextPage
	}
	return diffs, nil
}

func (c *Client) CreateMergeRequestDiscussion(
	ctx context.Context,
	projectID, iid int64,
	body string,
	position Position,
) error {
	options := &gitlabapi.CreateMergeRequestDiscussionOptions{
		Body: &body,
		Position: &gitlabapi.PositionOptions{
			BaseSHA: &position.BaseSHA, HeadSHA: &position.HeadSHA, StartSHA: &position.StartSHA,
			NewPath: &position.NewPath, OldPath: &position.OldPath, PositionType: &position.PositionType,
		},
	}
	if position.NewLine > 0 {
		options.Position.NewLine = &position.NewLine
	}
	if position.OldLine > 0 {
		options.Position.OldLine = &position.OldLine
	}
	_, _, err := c.api.Discussions.CreateMergeRequestDiscussion(
		projectID, iid, options, gitlabapi.WithContext(ctx),
	)
	if err == nil {
		return nil
	}
	if isInvalidDiscussionPosition(err) {
		return fmt.Errorf("create GitLab merge request %d!%d inline discussion: %w: %v", projectID, iid, ErrInvalidDiscussionPosition, err)
	}
	return fmt.Errorf("create GitLab merge request %d!%d inline discussion: %w", projectID, iid, err)
}

func (c *Client) CreateMergeRequestNote(ctx context.Context, projectID, iid int64, body string) error {
	_, _, err := c.api.Notes.CreateMergeRequestNote(
		projectID, iid, &gitlabapi.CreateMergeRequestNoteOptions{Body: &body}, gitlabapi.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("create GitLab merge request %d!%d note: %w", projectID, iid, err)
	}
	return nil
}

func (c *Client) AddMergeRequestDiscussionNote(
	ctx context.Context,
	projectID, iid int64,
	discussionID, body string,
) error {
	_, _, err := c.api.Discussions.AddMergeRequestDiscussionNote(
		projectID,
		iid,
		discussionID,
		&gitlabapi.AddMergeRequestDiscussionNoteOptions{Body: &body},
		gitlabapi.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("add note to GitLab merge request %d!%d discussion %q: %w", projectID, iid, discussionID, err)
	}
	return nil
}

func (c *Client) SetMergeRequestDiscussionResolved(
	ctx context.Context,
	projectID, iid int64,
	discussionID string,
	resolved bool,
) error {
	_, _, err := c.api.Discussions.ResolveMergeRequestDiscussion(
		projectID,
		iid,
		discussionID,
		&gitlabapi.ResolveMergeRequestDiscussionOptions{Resolved: &resolved},
		gitlabapi.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("set GitLab merge request %d!%d discussion %q resolved=%t: %w", projectID, iid, discussionID, resolved, err)
	}
	return nil
}

func (c *Client) RemoveMergeRequestReviewer(ctx context.Context, projectID, iid, reviewerID int64, expected DiffRefs) error {
	mergeRequest, err := c.GetMergeRequest(ctx, projectID, iid)
	if err != nil {
		return err
	}
	if mergeRequest.State != "opened" {
		return nil
	}
	if mergeRequest.DiffRefs != expected {
		return ErrReviewInputChanged
	}
	reviewerIDs := make([]int64, 0, len(mergeRequest.Reviewers))
	found := false
	for _, reviewer := range mergeRequest.Reviewers {
		if reviewer.ID == reviewerID {
			found = true
			continue
		}
		reviewerIDs = append(reviewerIDs, reviewer.ID)
	}
	if !found {
		return nil
	}
	_, _, err = c.api.MergeRequests.UpdateMergeRequest(
		projectID,
		iid,
		&gitlabapi.UpdateMergeRequestOptions{ReviewerIDs: &reviewerIDs},
		gitlabapi.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("remove reviewer %d from GitLab merge request %d!%d: %w", reviewerID, projectID, iid, err)
	}
	return nil
}

func isInvalidDiscussionPosition(err error) bool {
	var responseError *gitlabapi.ErrorResponse
	if !errors.As(err, &responseError) ||
		(responseError.StatusCode != http.StatusBadRequest && responseError.StatusCode != http.StatusUnprocessableEntity) {
		return false
	}
	message := strings.ToLower(responseError.Message + " " + string(responseError.Body))
	return strings.Contains(message, "position is invalid") ||
		strings.Contains(message, "position does not exist") ||
		strings.Contains(message, "position[") ||
		strings.Contains(message, "line_code") ||
		strings.Contains(message, "must be part of the diff")
}

type discussionFromAPI struct {
	ID             string                   `json:"id"`
	IndividualNote bool                     `json:"individual_note"`
	Resolvable     bool                     `json:"resolvable"`
	Resolved       *bool                    `json:"resolved"`
	ResolvedAt     *time.Time               `json:"resolved_at"`
	ResolvedBy     gitlabapi.NoteResolvedBy `json:"resolved_by"`
	ResolvedByPush *bool                    `json:"resolved_by_push"`
	Notes          []discussionNoteFromAPI  `json:"notes"`
}

type discussionNoteFromAPI struct {
	ID             int64                    `json:"id"`
	Body           string                   `json:"body"`
	Author         gitlabapi.NoteAuthor     `json:"author"`
	System         bool                     `json:"system"`
	Resolvable     bool                     `json:"resolvable"`
	Resolved       bool                     `json:"resolved"`
	ResolvedAt     *time.Time               `json:"resolved_at"`
	ResolvedBy     gitlabapi.NoteResolvedBy `json:"resolved_by"`
	ResolvedByPush *bool                    `json:"resolved_by_push"`
	Position       *gitlabapi.NotePosition  `json:"position"`
}

func noteFromDiscussionAPI(item discussionNoteFromAPI) Note {
	note := Note{
		ID:             item.ID,
		Body:           item.Body,
		Author:         User{ID: item.Author.ID, Username: item.Author.Username},
		System:         item.System,
		Resolvable:     item.Resolvable,
		Resolved:       item.Resolved,
		ResolvedAt:     item.ResolvedAt,
		ResolvedBy:     User{ID: item.ResolvedBy.ID, Username: item.ResolvedBy.Username},
		ResolvedByPush: item.ResolvedByPush != nil && *item.ResolvedByPush,
	}
	if item.Position != nil {
		note.Position = &Position{
			BaseSHA:      item.Position.BaseSHA,
			StartSHA:     item.Position.StartSHA,
			HeadSHA:      item.Position.HeadSHA,
			PositionType: item.Position.PositionType,
			NewPath:      item.Position.NewPath,
			NewLine:      item.Position.NewLine,
			OldPath:      item.Position.OldPath,
			OldLine:      item.Position.OldLine,
		}
	}
	return note
}

func classifyDiscussionResolution(discussion *Discussion, item discussionFromAPI) {
	if item.Resolved != nil {
		discussion.Resolved = *item.Resolved
	}
	discussion.ResolvedAt = item.ResolvedAt
	discussion.ResolvedBy = User{ID: item.ResolvedBy.ID, Username: item.ResolvedBy.Username}
	resolvedByPush := item.ResolvedByPush
	resolutionCauseKnown := item.ResolvedByPush != nil

	for i := len(discussion.Notes) - 1; i >= 0; i-- {
		note := discussion.Notes[i]
		if !note.Resolved {
			continue
		}
		discussion.Resolved = true
		if discussion.ResolvedAt == nil {
			discussion.ResolvedAt = note.ResolvedAt
		}
		if discussion.ResolvedBy.ID == 0 {
			discussion.ResolvedBy = note.ResolvedBy
		}
		if resolvedByPush == nil && i < len(item.Notes) && item.Notes[i].ResolvedByPush != nil {
			resolvedByPush = item.Notes[i].ResolvedByPush
			resolutionCauseKnown = true
		}
		break
	}
	if !discussion.Resolved {
		discussion.ResolutionCause = ResolutionCauseNone
		return
	}
	if resolvedByPush != nil && *resolvedByPush {
		discussion.ResolutionCause = ResolutionCauseOutdatedByPush
		return
	}
	if resolutionCauseKnown && discussion.ResolvedBy.ID > 0 {
		discussion.ResolutionCause = ResolutionCauseExplicit
		return
	}
	discussion.ResolutionCause = ResolutionCauseUnknown
}

// GetReviewInput bounds the multi-request read with two reads of the merge
// request. If its review-relevant version changes, the caller retries on a
// later poll instead of using mixed control-plane input.
func (c *Client) GetReviewInput(ctx context.Context, projectID, iid int64) (ReviewInput, error) {
	before, err := c.GetMergeRequest(ctx, projectID, iid)
	if err != nil {
		return ReviewInput{}, err
	}
	project, err := c.GetProject(ctx, projectID)
	if err != nil {
		return ReviewInput{}, err
	}
	notes, err := c.ListMergeRequestNotes(ctx, projectID, iid)
	if err != nil {
		return ReviewInput{}, err
	}
	after, err := c.GetMergeRequest(ctx, projectID, iid)
	if err != nil {
		return ReviewInput{}, err
	}
	if !sameMergeRequestVersion(before, after) {
		return ReviewInput{}, fmt.Errorf("get GitLab merge request %d!%d review input: %w", projectID, iid, ErrReviewInputChanged)
	}

	return ReviewInput{
		MergeRequest: after,
		Project:      project,
		Notes:        notes,
	}, nil
}

func mergeRequestFromAPI(item *gitlabapi.MergeRequest) MergeRequest {
	mergeRequest := MergeRequest{
		ID:           item.ID,
		ProjectID:    item.ProjectID,
		IID:          item.IID,
		Title:        item.Title,
		Description:  item.Description,
		State:        item.State,
		SourceBranch: item.SourceBranch,
		TargetBranch: item.TargetBranch,
		WebURL:       item.WebURL,
		Draft:        item.Draft,
		DiffRefs: DiffRefs{
			BaseSHA:  item.DiffRefs.BaseSha,
			StartSHA: item.DiffRefs.StartSha,
			HeadSHA:  item.DiffRefs.HeadSha,
		},
	}
	if item.UpdatedAt != nil {
		mergeRequest.UpdatedAt = *item.UpdatedAt
	}
	for _, reviewer := range item.Reviewers {
		if reviewer != nil {
			mergeRequest.Reviewers = append(mergeRequest.Reviewers, User{ID: reviewer.ID, Username: reviewer.Username})
		}
	}

	return mergeRequest
}

func noteFromAPI(item *gitlabapi.Note) Note {
	note := Note{
		ID:         item.ID,
		Body:       item.Body,
		Author:     User{ID: item.Author.ID, Username: item.Author.Username},
		System:     item.System,
		Resolvable: item.Resolvable,
		Resolved:   item.Resolved,
		ResolvedAt: item.ResolvedAt,
		ResolvedBy: User{ID: item.ResolvedBy.ID, Username: item.ResolvedBy.Username},
	}
	if item.CreatedAt != nil {
		note.CreatedAt = *item.CreatedAt
	}
	if item.Position != nil {
		note.Position = &Position{
			BaseSHA:      item.Position.BaseSHA,
			StartSHA:     item.Position.StartSHA,
			HeadSHA:      item.Position.HeadSHA,
			PositionType: item.Position.PositionType,
			NewPath:      item.Position.NewPath,
			NewLine:      item.Position.NewLine,
			OldPath:      item.Position.OldPath,
			OldLine:      item.Position.OldLine,
		}
	}

	return note
}

func sameMergeRequestVersion(left, right MergeRequest) bool {
	return left.ID == right.ID &&
		left.ProjectID == right.ProjectID &&
		left.IID == right.IID &&
		left.Title == right.Title &&
		left.Description == right.Description &&
		left.State == right.State &&
		left.SourceBranch == right.SourceBranch &&
		left.TargetBranch == right.TargetBranch &&
		left.WebURL == right.WebURL &&
		left.Draft == right.Draft &&
		left.UpdatedAt.Equal(right.UpdatedAt) &&
		left.DiffRefs == right.DiffRefs &&
		sameReviewers(left.Reviewers, right.Reviewers)
}

func sameReviewers(left, right []User) bool {
	if len(left) != len(right) {
		return false
	}

	rightByID := make(map[int64]string, len(right))
	for _, reviewer := range right {
		rightByID[reviewer.ID] = reviewer.Username
	}
	for _, reviewer := range left {
		if username, exists := rightByID[reviewer.ID]; !exists || username != reviewer.Username {
			return false
		}
	}

	return true
}
