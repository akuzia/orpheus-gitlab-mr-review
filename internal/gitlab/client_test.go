package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/config"

	"github.com/stretchr/testify/require"
)

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
