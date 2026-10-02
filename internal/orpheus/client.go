// Package orpheus adapts the versioned public Orpheus API to connector domain types.
package orpheus

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	api "github.com/orpheus-agents/orpheus/client"
)

const (
	pageSize        = 100
	maxResponseSize = 64 << 20
)

var errorCodePattern = regexp.MustCompile(`^[a-z0-9_]{1,128}$`)

type CreateSessionRequest = api.CreateSession
type ConfigurationInput = api.ConfigurationInput
type AgentInput = api.AgentInput
type SandboxInput = api.SandboxInput
type HooksInput = api.HooksInput
type LimitsInput = api.LimitsInput
type TextMessage = api.TextMessage

type ReviewSessionKey struct {
	Namespace         string
	MRKey             string
	ReviewFingerprint string
}

// Session is the one-shot session projection returned by the exact run search.
// The Orpheus run endpoint is used because it can filter by both the owning
// session identity and the first run's input fingerprint in one request.
type Session struct {
	ID               string
	RunID            string
	MRKey            string
	Status           string
	InputFingerprint string
	CreatedAt        time.Time
	FinishedAt       *time.Time
	ErrorCode        string
	AgentStatus      string
	AgentErrorCode   string
	Hooks            []HookResult
}

type HookResult struct {
	Name               string
	Status             string
	ExitCode           *int
	OutputCompleteness string
	TruncationReason   string
	OutputType         string
	Output             string
	ErrorCode          string
}

type Accepted struct {
	SessionID string
	RunID     string
	MessageID string
}

type Error struct {
	Status int
	Code   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("Orpheus HTTP %d: %s", e.Status, e.Code)
}

func Code(err error) string {
	if apiError, ok := errors.AsType[*Error](err); ok {
		return apiError.Code
	}

	return ""
}

type apiClient interface {
	ListAllRuns(
		ctx context.Context,
		params *api.ListAllRunsParams,
		reqEditors ...api.RequestEditorFn,
	) (*http.Response, error)
	ListSessions(
		ctx context.Context,
		params *api.ListSessionsParams,
		reqEditors ...api.RequestEditorFn,
	) (*http.Response, error)
	GetRun(
		ctx context.Context,
		sid uuid.UUID,
		rid uuid.UUID,
		reqEditors ...api.RequestEditorFn,
	) (*http.Response, error)
	CreateSession(
		ctx context.Context,
		params *api.CreateSessionParams,
		body api.CreateSessionJSONRequestBody,
		reqEditors ...api.RequestEditorFn,
	) (*http.Response, error)
	GetHistory(
		ctx context.Context,
		sid uuid.UUID,
		params *api.GetHistoryParams,
		reqEditors ...api.RequestEditorFn,
	) (*http.Response, error)
	CancelRun(
		ctx context.Context,
		sid uuid.UUID,
		rid uuid.UUID,
		reqEditors ...api.RequestEditorFn,
	) (*http.Response, error)
}

type Client struct {
	api apiClient
}

func New(baseURL, token string, timeout time.Duration) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("create Orpheus client: base URL is required")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("create Orpheus client: API key is required")
	}
	if timeout <= 0 {
		return nil, errors.New("create Orpheus client: HTTP timeout must be positive")
	}

	httpClient := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	client, err := api.NewClient(
		baseURL,
		api.WithHTTPClient(httpClient),
		api.WithRequestEditorFn(func(_ context.Context, request *http.Request) error {
			request.Header.Set("Authorization", "Bearer "+token)
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("create Orpheus client: %w", err)
	}

	return &Client{api: client}, nil
}

func (c *Client) FindSessions(ctx context.Context, key ReviewSessionKey) ([]Session, error) {
	if err := key.validate(); err != nil {
		return nil, fmt.Errorf("find Orpheus sessions: %w", err)
	}

	params := &api.ListAllRunsParams{
		Namespace:        &key.Namespace,
		ExternalKey:      &key.MRKey,
		InputFingerprint: &key.ReviewFingerprint,
		Order:            new(api.ListAllRunsParamsOrderAsc),
		Limit:            new(pageSize),
	}
	seenCursors := make(map[string]struct{})
	var sessions []Session

	for {
		response, requestErr := c.api.ListAllRuns(ctx, params)
		page, err := decode[api.RunPage](response, requestErr, http.StatusOK)
		if err != nil {
			return nil, fmt.Errorf("find Orpheus sessions: %w", err)
		}
		for _, item := range page.Items {
			session, err := sessionFromAPI(item)
			if err != nil {
				return nil, fmt.Errorf("find Orpheus sessions: %w", err)
			}
			sessions = append(sessions, session)
		}
		if page.NextCursor == nil {
			break
		}
		cursor := *page.NextCursor
		if cursor == "" {
			return nil, errors.New("find Orpheus sessions: empty pagination cursor")
		}
		if _, exists := seenCursors[cursor]; exists {
			return nil, errors.New("find Orpheus sessions: repeated pagination cursor")
		}
		seenCursors[cursor] = struct{}{}
		params.Cursor = &cursor
	}

	return sessions, nil
}

// ListActiveSessions returns the latest unfinished run for every active
// one-shot session in a workflow namespace. Session listing supplies the MR
// key, while the run read supplies the immutable input fingerprint and current
// lifecycle status required for restart recovery.
func (c *Client) ListActiveSessions(ctx context.Context, namespace string) ([]Session, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil, errors.New("list active Orpheus sessions: namespace is required")
	}
	activity := api.Active
	params := &api.ListSessionsParams{
		Activity:  &activity,
		Namespace: &namespace,
		Order:     new(api.ListSessionsParamsOrderAsc),
		Limit:     new(pageSize),
	}
	seenCursors := make(map[string]struct{})
	var sessions []Session
	for {
		response, requestErr := c.api.ListSessions(ctx, params)
		page, err := decode[api.SessionPage](response, requestErr, http.StatusOK)
		if err != nil {
			return nil, fmt.Errorf("list active Orpheus sessions: %w", err)
		}
		for _, item := range page.Items {
			if item.ExternalKey == nil || strings.TrimSpace(*item.ExternalKey) == "" {
				return nil, errors.New("list active Orpheus sessions: session has no external key")
			}
			if item.AllowMultipleRuns {
				return nil, errors.New("list active Orpheus sessions: review session allows multiple runs")
			}
			runResponse, requestErr := c.api.GetRun(ctx, item.ID, item.LastRunID)
			run, err := decode[api.Run](runResponse, requestErr, http.StatusOK)
			if err != nil {
				return nil, fmt.Errorf("list active Orpheus sessions: read latest run: %w", err)
			}
			if run.SessionID != item.ID || run.ID != item.LastRunID {
				return nil, errors.New("list active Orpheus sessions: latest run identity mismatch")
			}
			session, err := sessionFromAPI(run)
			if err != nil {
				return nil, fmt.Errorf("list active Orpheus sessions: %w", err)
			}
			active, err := classifyActivity(session.Status)
			if err != nil {
				return nil, fmt.Errorf("list active Orpheus sessions: %w", err)
			}
			if !active {
				continue
			}
			if strings.TrimSpace(session.InputFingerprint) == "" {
				return nil, errors.New("list active Orpheus sessions: latest run has no input fingerprint")
			}
			session.MRKey = *item.ExternalKey
			sessions = append(sessions, session)
		}
		if page.NextCursor == nil {
			break
		}
		cursor := *page.NextCursor
		if cursor == "" {
			return nil, errors.New("list active Orpheus sessions: empty pagination cursor")
		}
		if _, exists := seenCursors[cursor]; exists {
			return nil, errors.New("list active Orpheus sessions: repeated pagination cursor")
		}
		seenCursors[cursor] = struct{}{}
		params.Cursor = &cursor
	}

	return sessions, nil
}

func classifyActivity(status string) (bool, error) {
	switch status {
	case "accepted", "starting", "running", "cancelling", "finalizing":
		return true, nil
	case "completed", "failed", "cancelled":
		return false, nil
	default:
		return false, fmt.Errorf("unsupported run status %q", status)
	}
}

func (c *Client) FindMessageMetadata(
	ctx context.Context,
	sessionID string,
	runID string,
	externalKey string,
) (json.RawMessage, error) {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return nil, errors.New("find Orpheus message metadata: invalid session ID")
	}
	rid, err := uuid.Parse(runID)
	if err != nil {
		return nil, errors.New("find Orpheus message metadata: invalid run ID")
	}
	if strings.TrimSpace(externalKey) == "" {
		return nil, errors.New("find Orpheus message metadata: external key is required")
	}

	params := &api.GetHistoryParams{
		MessageExternalKey: &externalKey,
		RunID:              &rid,
		Limit:              new(pageSize),
	}
	seenCursors := make(map[string]struct{})
	var matches []json.RawMessage
	for {
		response, requestErr := c.api.GetHistory(ctx, sid, params)
		page, err := decode[api.HistoryPage](response, requestErr, http.StatusOK)
		if err != nil {
			return nil, fmt.Errorf("find Orpheus message metadata: %w", err)
		}
		for _, item := range page.Items {
			value, err := item.ValueByDiscriminator()
			if err != nil {
				return nil, errors.New("find Orpheus message metadata: invalid history item")
			}
			messageItem, ok := value.(api.MessageItem)
			if !ok || messageItem.Message.ExternalKey == nil || *messageItem.Message.ExternalKey != externalKey {
				continue
			}
			if messageItem.Message.Metadata == nil {
				return nil, errors.New("find Orpheus message metadata: matching message has no metadata")
			}
			matches = append(matches, append(json.RawMessage(nil), (*messageItem.Message.Metadata)...))
		}
		if page.NextCursor == nil {
			break
		}
		cursor := *page.NextCursor
		if cursor == "" {
			return nil, errors.New("find Orpheus message metadata: empty pagination cursor")
		}
		if _, exists := seenCursors[cursor]; exists {
			return nil, errors.New("find Orpheus message metadata: repeated pagination cursor")
		}
		seenCursors[cursor] = struct{}{}
		params.Cursor = &cursor
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("find Orpheus message metadata: expected one matching message, got %d", len(matches))
	}

	return matches[0], nil
}

func (c *Client) CreateSession(
	ctx context.Context,
	key ReviewSessionKey,
	reviewerID int64,
	request CreateSessionRequest,
) (Accepted, error) {
	if err := key.validate(); err != nil {
		return Accepted{}, fmt.Errorf("create Orpheus session: %w", err)
	}
	if reviewerID <= 0 {
		return Accepted{}, errors.New("create Orpheus session: reviewer ID must be positive")
	}
	if len(request.Messages) == 0 {
		return Accepted{}, errors.New("create Orpheus session: at least one message is required")
	}
	if err := bindRequestIdentity(&request, key); err != nil {
		return Accepted{}, fmt.Errorf("create Orpheus session: %w", err)
	}
	idempotencyKey := sessionIdempotencyKey(key, reviewerID)

	response, requestErr := c.api.CreateSession(
		ctx,
		&api.CreateSessionParams{IdempotencyKey: &idempotencyKey},
		request,
	)
	accepted, err := decode[api.Accepted](response, requestErr, http.StatusAccepted)
	if err != nil {
		return Accepted{}, fmt.Errorf("create Orpheus session: %w", err)
	}

	return Accepted{
		SessionID: accepted.SessionID.String(),
		RunID:     accepted.RunID.String(),
		MessageID: accepted.MessageID.String(),
	}, nil
}

func (c *Client) CancelRun(ctx context.Context, sessionID, runID string) error {
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return errors.New("cancel Orpheus run: invalid session ID")
	}
	rid, err := uuid.Parse(runID)
	if err != nil {
		return errors.New("cancel Orpheus run: invalid run ID")
	}

	response, requestErr := c.api.CancelRun(ctx, sid, rid)
	if _, err := decode[api.Cancelled](response, requestErr, http.StatusOK, http.StatusAccepted); err != nil {
		return fmt.Errorf("cancel Orpheus run: %w", err)
	}

	return nil
}

func decode[T any](response *http.Response, requestErr error, expectedStatuses ...int) (T, error) {
	var result T
	if requestErr != nil {
		return result, requestErr
	}
	if response == nil {
		return result, errors.New("empty Orpheus response")
	}
	defer func() {
		_ = response.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize+1))
	if err != nil {
		return result, err
	}
	if len(body) > maxResponseSize {
		return result, errors.New("Orpheus response exceeds size limit")
	}
	expected := false
	for _, status := range expectedStatuses {
		if response.StatusCode == status {
			expected = true
			break
		}
	}
	if !expected {
		code := "unexpected_response"
		var problem api.ErrorResponse
		if json.Unmarshal(body, &problem) == nil && errorCodePattern.MatchString(problem.Error.Code) {
			code = problem.Error.Code
		}
		return result, &Error{Status: response.StatusCode, Code: code}
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return result, errors.New("invalid Orpheus JSON response")
	}

	return result, nil
}

func (k ReviewSessionKey) validate() error {
	if strings.TrimSpace(k.Namespace) == "" {
		return errors.New("namespace is required")
	}
	if strings.TrimSpace(k.MRKey) == "" {
		return errors.New("MR key is required")
	}
	if strings.TrimSpace(k.ReviewFingerprint) == "" {
		return errors.New("review fingerprint is required")
	}

	return nil
}

func bindRequestIdentity(request *CreateSessionRequest, key ReviewSessionKey) error {
	if err := bindString("namespace", &request.Namespace, key.Namespace); err != nil {
		return err
	}
	if err := bindString("external key", &request.ExternalKey, key.MRKey); err != nil {
		return err
	}
	if err := bindString("input fingerprint", &request.InputFingerprint, key.ReviewFingerprint); err != nil {
		return err
	}
	if request.AllowMultipleRuns != nil && *request.AllowMultipleRuns {
		return errors.New("multiple runs are not allowed for review sessions")
	}
	request.AllowMultipleRuns = new(false)

	return nil
}

func bindString(name string, target **string, expected string) error {
	if *target != nil && **target != expected {
		return fmt.Errorf("request %s does not match review session identity", name)
	}
	*target = &expected

	return nil
}

func sessionIdempotencyKey(key ReviewSessionKey, reviewerID int64) string {
	payload, _ := json.Marshal(struct {
		Version           int    `json:"version"`
		Namespace         string `json:"namespace"`
		MRKey             string `json:"mr_key"`
		ReviewerID        int64  `json:"reviewer_id"`
		ReviewFingerprint string `json:"review_fingerprint"`
	}{
		Version:           1,
		Namespace:         key.Namespace,
		MRKey:             key.MRKey,
		ReviewerID:        reviewerID,
		ReviewFingerprint: key.ReviewFingerprint,
	})
	sum := sha256.Sum256(payload)

	return fmt.Sprintf("gitlab-mr-review-session-v1:%x", sum)
}

func sessionFromAPI(item api.Run) (Session, error) {
	session := Session{
		ID:         item.SessionID.String(),
		RunID:      item.ID.String(),
		Status:     string(item.Status),
		CreatedAt:  item.CreatedAt,
		FinishedAt: item.FinishedAt,
	}
	if item.InputFingerprint != nil {
		session.InputFingerprint = *item.InputFingerprint
	}
	if item.Error != nil {
		session.ErrorCode = item.Error.Code
	}
	if item.AgentStatus != nil {
		session.AgentStatus = string(*item.AgentStatus)
	}
	if item.AgentError != nil {
		session.AgentErrorCode = item.AgentError.Code
	}
	for _, item := range item.Hooks {
		hook := HookResult{
			Name:               string(item.Name),
			Status:             string(item.Status),
			ExitCode:           item.ExitCode,
			OutputCompleteness: string(item.OutputCompleteness),
		}
		if item.TruncationReason != nil {
			hook.TruncationReason = string(*item.TruncationReason)
		}
		if item.Error != nil {
			hook.ErrorCode = item.Error.Code
		}
		if item.Output != nil {
			output, err := item.Output.AsTextResult()
			if err != nil {
				return Session{}, errors.New("invalid Orpheus hook output")
			}
			hook.OutputType = string(output.Type)
			hook.Output = output.Text
		}
		session.Hooks = append(session.Hooks, hook)
	}

	return session, nil
}
