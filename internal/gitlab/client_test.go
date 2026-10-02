package gitlab

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/config"

	"github.com/stretchr/testify/require"
	gitlabapi "gitlab.com/gitlab-org/api/client-go/v3"
)

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	require.True(t, IsRetryable(context.DeadlineExceeded))
	require.True(t, IsRetryable(ErrReviewInputChanged))
	require.True(t, IsRetryable(&gitlabapi.ErrorResponse{StatusCode: http.StatusTooManyRequests}))
	require.True(t, IsRetryable(&gitlabapi.ErrorResponse{StatusCode: http.StatusServiceUnavailable}))
	require.False(t, IsRetryable(&gitlabapi.ErrorResponse{StatusCode: http.StatusNotFound}))
	require.False(t, IsRetryable(context.Canceled))
}

func TestGetReviewInputLoadsPaginatedNotesWithoutDiscussions(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v4/projects/42/merge_requests/17":
			_, _ = fmt.Fprint(response, mergeRequestJSON("2026-10-01T10:00:00Z"))
		case "/api/v4/projects/42":
			_, _ = fmt.Fprint(response, `{
				"id":42,
				"path_with_namespace":"team/project",
				"http_url_to_repo":"https://gitlab.example.com/team/project.git",
				"ssh_url_to_repo":"git@gitlab.example.com:team/project.git",
				"web_url":"https://gitlab.example.com/team/project"
			}`)
		case "/api/v4/projects/42/merge_requests/17/notes":
			if request.URL.Query().Get("page") == "1" {
				response.Header().Set("X-Next-Page", "2")
				_, _ = fmt.Fprint(response, `[{
					"id":10,"body":"please fix","author":{"id":7,"username":"author"},
					"resolvable":true,"resolved":false,
					"position":{"base_sha":"base","start_sha":"start","head_sha":"head","position_type":"text","new_path":"main.go","new_line":5}
				}]`)
				return
			}
			_, _ = fmt.Fprint(response, `[{"id":11,"body":"requested review from @orpheus","author":{"id":7},"system":true}]`)
		case "/api/v4/projects/42/merge_requests/17/discussions":
			t.Error("admission input must not fetch discussions")
			http.Error(response, "unexpected discussions request", http.StatusInternalServerError)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	input, err := client.GetReviewInput(context.Background(), 42, 17)

	require.NoError(t, err)
	require.Equal(t, int64(42), input.Project.ID)
	require.Equal(t, "team/project", input.Project.PathWithNamespace)
	require.Equal(t, "https://gitlab.example.com/team/project.git", input.Project.HTTPURLToRepo)
	require.Equal(t, DiffRefs{BaseSHA: "base", StartSHA: "start", HeadSHA: "head"}, input.MergeRequest.DiffRefs)
	require.Equal(t, []User{{ID: 123, Username: "orpheus"}}, input.MergeRequest.Reviewers)
	require.Len(t, input.Notes, 2)
	require.Equal(t, int64(5), input.Notes[0].Position.NewLine)
}

func TestGetReviewInputRejectsChangedMergeRequest(t *testing.T) {
	t.Parallel()

	mrCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v4/projects/42/merge_requests/17":
			mrCalls++
			updatedAt := "2026-10-01T10:00:00Z"
			if mrCalls == 2 {
				updatedAt = "2026-10-01T10:00:01Z"
			}
			_, _ = fmt.Fprint(response, mergeRequestJSON(updatedAt))
		case "/api/v4/projects/42":
			_, _ = fmt.Fprint(response, `{"id":42,"path_with_namespace":"team/project"}`)
		case "/api/v4/projects/42/merge_requests/17/notes":
			_, _ = fmt.Fprint(response, `[]`)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	_, err := client.GetReviewInput(context.Background(), 42, 17)

	require.ErrorIs(t, err, ErrReviewInputChanged)
}

func TestListMergeRequestDiscussionsClassifiesResolutionCause(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		require.Equal(t, "/api/v4/projects/42/merge_requests/17/discussions", request.URL.Path)
		_, _ = fmt.Fprint(response, `[
			{
				"id":"outdated","resolvable":true,"resolved":true,"resolved_by_push":true,
				"notes":[{
					"id":10,"body":"finding","author":{"id":123,"username":"orpheus"},
					"resolvable":true,"resolved":true,"resolved_by_push":true,
					"resolved_at":"2026-10-01T10:00:00Z",
					"position":{"base_sha":"base","start_sha":"start","head_sha":"old-head","position_type":"text","new_path":"main.go","new_line":5}
				}]
			},
			{
				"id":"human","resolvable":true,"resolved":true,
				"resolved_by":{"id":7,"username":"developer"},"resolved_by_push":false,
				"notes":[{
					"id":11,"body":"finding","author":{"id":123,"username":"orpheus"},
					"resolvable":true,"resolved":true,"resolved_by":{"id":7,"username":"developer"}
				}]
			},
			{
				"id":"unknown","resolvable":true,"resolved":true,
				"notes":[{"id":12,"body":"finding","author":{"id":123},"resolvable":true,"resolved":true}]
			}
		]`)
	}))
	t.Cleanup(server.Close)

	discussions, err := newTestClient(t, server.URL).ListMergeRequestDiscussions(t.Context(), 42, 17)

	require.NoError(t, err)
	require.Len(t, discussions, 3)
	require.Equal(t, ResolutionCauseOutdatedByPush, discussions[0].ResolutionCause)
	require.True(t, discussions[0].Notes[0].ResolvedByPush)
	require.Equal(t, int64(5), discussions[0].Notes[0].Position.NewLine)
	require.Equal(t, ResolutionCauseExplicit, discussions[1].ResolutionCause)
	require.Equal(t, int64(7), discussions[1].ResolvedBy.ID)
	require.Equal(t, ResolutionCauseUnknown, discussions[2].ResolutionCause)
}

func TestPublicationHTTPContract(t *testing.T) {
	t.Parallel()

	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		requests = append(requests, request.Method+" "+request.URL.Path+" "+string(body))
		switch request.Method + " " + request.URL.Path {
		case "GET /api/v4/projects/42/merge_requests/17/diffs":
			_, _ = fmt.Fprint(response, `[{
				"old_path":"main.go","new_path":"main.go","diff":"@@ -1 +1 @@\n-old\n+new\n",
				"new_file":false,"renamed_file":false,"deleted_file":false
			}]`)
		case "POST /api/v4/projects/42/merge_requests/17/discussions":
			_, _ = fmt.Fprint(response, `{"id":"new-thread","notes":[]}`)
		case "POST /api/v4/projects/42/merge_requests/17/notes":
			_, _ = fmt.Fprint(response, `{"id":101,"body":"note"}`)
		case "POST /api/v4/projects/42/merge_requests/17/discussions/thread-1/notes":
			_, _ = fmt.Fprint(response, `{"id":102,"body":"reply"}`)
		case "PUT /api/v4/projects/42/merge_requests/17/discussions/thread-1":
			_, _ = fmt.Fprint(response, `{"id":"thread-1","notes":[]}`)
		case "GET /api/v4/projects/42/merge_requests/17":
			_, _ = fmt.Fprint(response, mergeRequestJSON("2026-10-01T10:00:00Z"))
		case "PUT /api/v4/projects/42/merge_requests/17":
			_, _ = fmt.Fprint(response, mergeRequestJSON("2026-10-01T10:00:00Z"))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL)

	diffs, err := client.ListMergeRequestDiffs(t.Context(), 42, 17)
	require.NoError(t, err)
	require.Equal(t, "main.go", diffs[0].NewPath)
	require.NoError(t, client.CreateMergeRequestDiscussion(t.Context(), 42, 17, "finding", Position{
		BaseSHA: "base", StartSHA: "start", HeadSHA: "head", PositionType: "text",
		OldPath: "main.go", NewPath: "main.go", NewLine: 1,
	}))
	require.NoError(t, client.CreateMergeRequestNote(t.Context(), 42, 17, "completion"))
	require.NoError(t, client.AddMergeRequestDiscussionNote(t.Context(), 42, 17, "thread-1", "still reproducible"))
	require.NoError(t, client.SetMergeRequestDiscussionResolved(t.Context(), 42, 17, "thread-1", false))
	require.NoError(t, client.RemoveMergeRequestReviewer(t.Context(), 42, 17, 123))

	require.Len(t, requests, 7)
	require.Contains(t, requests[1], `"new_line":1`)
	require.Contains(t, requests[4], `"resolved":false`)
	require.Contains(t, requests[6], `"reviewer_ids":[]`)
}

func TestCreateMergeRequestDiscussionClassifiesInvalidPosition(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(response, `{"message":"position is invalid"}`)
	}))
	t.Cleanup(server.Close)

	err := newTestClient(t, server.URL).CreateMergeRequestDiscussion(t.Context(), 42, 17, "finding", Position{
		BaseSHA: "base", StartSHA: "start", HeadSHA: "head", PositionType: "text",
		OldPath: "main.go", NewPath: "main.go", NewLine: 1,
	})

	require.ErrorIs(t, err, ErrInvalidDiscussionPosition)
	require.False(t, IsRetryable(err))
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := New(config.Config{
		HTTPTimeout: time.Second,
		GitLab: config.GitLab{
			BaseURL: baseURL,
			Token:   "token",
		},
	})
	require.NoError(t, err)

	return client
}

func mergeRequestJSON(updatedAt string) string {
	return fmt.Sprintf(`{
		"id":100,"iid":17,"project_id":42,"title":"Change","description":"Review this",
		"state":"opened","source_branch":"feature/x","target_branch":"main",
		"web_url":"https://gitlab.example.com/team/project/-/merge_requests/17",
		"updated_at":%q,
		"reviewers":[{"id":123,"username":"orpheus"}],
		"diff_refs":{"base_sha":"base","start_sha":"start","head_sha":"head"}
	}`, updatedAt)
}
