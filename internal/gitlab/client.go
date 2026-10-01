package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/config"
	gitlabapi "gitlab.com/gitlab-org/api/client-go/v3"
)

const userAgent = "orpheus-gitlab-mr-review"

const pageSize int64 = 100

var ErrReviewInputChanged = errors.New("merge request changed while loading review input")

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
		items, response, err := c.api.Discussions.ListMergeRequestDiscussions(
			projectID,
			iid,
			&gitlabapi.ListMergeRequestDiscussionsOptions{
				ListOptions: gitlabapi.ListOptions{Page: page, PerPage: pageSize},
			},
			gitlabapi.WithContext(ctx),
		)
		if err != nil {
			return nil, fmt.Errorf("list GitLab merge request %d!%d discussions on page %d: %w", projectID, iid, page, err)
		}
		for _, item := range items {
			if item == nil {
				continue
			}
			discussion := Discussion{ID: item.ID, IndividualNote: item.IndividualNote}
			for _, note := range item.Notes {
				if note != nil {
					discussion.Notes = append(discussion.Notes, noteFromAPI(note))
				}
			}
			discussions = append(discussions, discussion)
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		page = response.NextPage
	}

	return discussions, nil
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
