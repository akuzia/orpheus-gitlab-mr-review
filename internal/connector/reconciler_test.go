package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/orpheus"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/protocol"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/workflow"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type orpheusMock struct {
	find          func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error)
	cancel        func(context.Context, string, string) error
	create        func(context.Context, orpheus.ReviewSessionKey, int64, orpheus.CreateSessionRequest) (orpheus.Accepted, error)
	findCalls     int
	cancelCalls   int
	createCalls   int
	lastKey       orpheus.ReviewSessionKey
	lastReviewer  int64
	lastRequest   orpheus.CreateSessionRequest
	metadata      func(context.Context, string, string, string) (json.RawMessage, error)
	metadataCalls int
}

type concurrentOrpheusStub struct {
	find   func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error)
	cancel func(context.Context, string, string) error
}

func (s concurrentOrpheusStub) FindSessions(ctx context.Context, key orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
	return s.find(ctx, key)
}

func (s concurrentOrpheusStub) CancelRun(ctx context.Context, sessionID, runID string) error {
	if s.cancel == nil {
		return errors.New("unexpected cancel run")
	}
	return s.cancel(ctx, sessionID, runID)
}

func (concurrentOrpheusStub) CreateSession(context.Context, orpheus.ReviewSessionKey, int64, orpheus.CreateSessionRequest) (orpheus.Accepted, error) {
	return orpheus.Accepted{}, errors.New("unexpected create session")
}

func (concurrentOrpheusStub) FindMessageMetadata(context.Context, string, string, string) (json.RawMessage, error) {
	return nil, errors.New("unexpected metadata request")
}

func (m *orpheusMock) FindMessageMetadata(ctx context.Context, sessionID, runID, externalKey string) (json.RawMessage, error) {
	m.metadataCalls++
	if m.metadata == nil {
		return nil, errors.New("unexpected metadata request")
	}
	return m.metadata(ctx, sessionID, runID, externalKey)
}

func (m *orpheusMock) FindSessions(ctx context.Context, key orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
	m.findCalls++
	m.lastKey = key
	if m.find == nil {
		return nil, nil
	}
	return m.find(ctx, key)
}

func (m *orpheusMock) CancelRun(ctx context.Context, sessionID, runID string) error {
	m.cancelCalls++
	if m.cancel == nil {
		return errors.New("unexpected cancel run")
	}
	return m.cancel(ctx, sessionID, runID)
}

func (m *orpheusMock) CreateSession(ctx context.Context, key orpheus.ReviewSessionKey, reviewerID int64, request orpheus.CreateSessionRequest) (orpheus.Accepted, error) {
	m.createCalls++
	m.lastKey = key
	m.lastReviewer = reviewerID
	m.lastRequest = request
	if m.create == nil {
		return orpheus.Accepted{}, nil
	}
	return m.create(ctx, key, reviewerID, request)
}

func TestReconcilerCreatesSessionForNewEligibleInput(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zap.DebugLevel)
	client := &orpheusMock{create: func(context.Context, orpheus.ReviewSessionKey, int64, orpheus.CreateSessionRequest) (orpheus.Accepted, error) {
		return orpheus.Accepted{SessionID: "session-1", RunID: "run-1"}, nil
	}}
	input := eligibleReviewInput()
	contract := testSessionContract(input)
	reconciler := NewReconciler(zap.New(core), client, func(got review.Input) (workflow.SessionContract, error) {
		require.Equal(t, input, got)
		return contract, nil
	})

	err := reconciler.Reconcile(context.Background(), input)

	require.NoError(t, err)
	require.Equal(t, 1, client.findCalls)
	require.Equal(t, 1, client.createCalls)
	require.Equal(t, contract.Key, client.lastKey)
	require.Equal(t, contract.ReviewerID, client.lastReviewer)
	require.Equal(t, contract.Request, client.lastRequest)
	require.Equal(t, 1, logs.FilterMessage("created Orpheus review session").Len())
}

func TestReconcilerDoesNotCreateSecondAnalysisForExistingSession(t *testing.T) {
	t.Parallel()

	statuses := []string{"accepted", "starting", "running", "cancelling", "finalizing", "failed", "cancelled"}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			client := &orpheusMock{find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
				return []orpheus.Session{{ID: "session-1", RunID: "run-1", Status: status}}, nil
			}}
			buildCalls := 0
			reconciler := NewReconciler(zap.NewNop(), client, func(review.Input) (workflow.SessionContract, error) {
				buildCalls++
				return workflow.SessionContract{}, nil
			})

			err := reconciler.Reconcile(context.Background(), eligibleReviewInput())

			require.NoError(t, err)
			require.Equal(t, 1, client.findCalls)
			require.Zero(t, client.createCalls)
			require.Zero(t, buildCalls)
		})
	}
}

func TestReconcilerValidatesCompletedBundleAgainstImmutableMetadata(t *testing.T) {
	t.Parallel()

	input := eligibleReviewInput()
	input.MergeRequest.WebURL = "https://gitlab.example.com/team/project/-/merge_requests/2989"
	contract, err := workflow.BuildSessionContract(input, workflow.Options{
		GitLabHost:         "https://gitlab.example.com",
		Instructions:       "# Policy\n\nReview the pinned diff.\n",
		AgentProfile:       "review-profile",
		SandboxTemplate:    "review-sandbox",
		RunTimeoutSeconds:  3600,
		HookTimeoutSeconds: 120,
		MaxRequestBytes:    1 << 20,
	})
	require.NoError(t, err)
	bundle := protocol.Bundle{
		SchemaVersion:   protocol.SchemaVersion,
		Stage:           protocol.StageReady,
		Workflow:        workflow.ProtocolExpected(contract.Metadata).Workflow,
		Identity:        workflow.ProtocolExpected(contract.Metadata).Identity,
		Review:          workflow.ProtocolExpected(contract.Metadata).Review,
		Counts:          protocol.Counts{},
		Confirmed:       []protocol.Finding{},
		Recommendations: []protocol.Recommendation{},
		Resolutions:     []protocol.Resolution{},
	}
	output, err := protocol.Encode(bundle, protocol.DefaultLimits())
	require.NoError(t, err)
	metadata, err := json.Marshal(contract.Metadata)
	require.NoError(t, err)
	exitCode := 0
	client := &orpheusMock{
		find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
			return []orpheus.Session{{
				ID:          "session-1",
				RunID:       "run-1",
				Status:      "completed",
				AgentStatus: "completed",
				Hooks: []orpheus.HookResult{{
					Name:               "after_run",
					Status:             "completed",
					ExitCode:           &exitCode,
					OutputCompleteness: "complete",
					OutputType:         "text",
					Output:             string(output),
				}},
			}}, nil
		},
		metadata: func(_ context.Context, sessionID, runID, externalKey string) (json.RawMessage, error) {
			require.Equal(t, "session-1", sessionID)
			require.Equal(t, "run-1", runID)
			require.Equal(t, workflow.MessageExternalKey(input.ReviewFingerprint), externalKey)
			return metadata, nil
		},
	}
	core, logs := observer.New(zap.DebugLevel)
	reconciler := NewReconciler(zap.New(core), client, func(review.Input) (workflow.SessionContract, error) {
		t.Fatal("completed session must not build a new contract")
		return workflow.SessionContract{}, nil
	})

	err = reconciler.Reconcile(context.Background(), input)

	require.NoError(t, err)
	require.Equal(t, 1, client.metadataCalls)
	require.Zero(t, client.createCalls)
	require.Equal(t, 1, logs.FilterMessage("validated Orpheus review bundle").Len())
}

func TestReconcilerRejectsCompletedRunWithoutSuccessfulAgentAndHook(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		session orpheus.Session
		code    string
	}{
		"agent failed": {
			session: orpheus.Session{Status: "completed", AgentStatus: "failed"},
			code:    "agent_not_completed",
		},
		"hook missing": {
			session: orpheus.Session{Status: "completed", AgentStatus: "completed"},
			code:    "after_run_missing",
		},
		"output truncated": {
			session: orpheus.Session{
				Status:      "completed",
				AgentStatus: "completed",
				Hooks: []orpheus.HookResult{{
					Name:               "after_run",
					Status:             "completed",
					ExitCode:           new(0),
					OutputCompleteness: "truncated",
					OutputType:         "text",
				}},
			},
			code: "after_run_output_incomplete",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := &orpheusMock{find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
				return []orpheus.Session{test.session}, nil
			}}
			reconciler := NewReconciler(zap.NewNop(), client, func(review.Input) (workflow.SessionContract, error) {
				return workflow.SessionContract{}, errors.New("must not build")
			})

			err := reconciler.Reconcile(context.Background(), eligibleReviewInput())

			var lifecycleError *LifecycleError
			require.ErrorAs(t, err, &lifecycleError)
			require.Equal(t, test.code, lifecycleError.Code)
			require.False(t, lifecycleError.Retryable)
			require.Zero(t, client.metadataCalls)
			require.Zero(t, client.createCalls)
		})
	}
}

func TestReconcilerSkipsIneligibleInputBeforeOrpheus(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zap.DebugLevel)
	client := &orpheusMock{}
	reconciler := NewReconciler(zap.New(core), client, func(review.Input) (workflow.SessionContract, error) {
		t.Fatal("contract builder must not be called")
		return workflow.SessionContract{}, nil
	})
	input := eligibleReviewInput()
	input.MergeRequest.State = "closed"
	input.MergeRequest.Reviewers = nil

	err := reconciler.Reconcile(context.Background(), input)

	require.NoError(t, err)
	require.Zero(t, client.findCalls)
	require.Zero(t, client.createCalls)
	entries := logs.FilterMessage("GitLab merge request is not eligible for review").All()
	require.Len(t, entries, 1)
	require.Equal(t, []any{"mr_not_open", "reviewer_not_assigned"}, entries[0].ContextMap()["reasons"])
}

func TestReconcilerClassifiesRetryableLifecycleErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err       error
		retryable bool
		code      string
	}{
		"rate limit": {err: &orpheus.Error{Status: 429, Code: "rate_limited"}, retryable: true, code: "rate_limited"},
		"server":     {err: &orpheus.Error{Status: 503, Code: "unavailable"}, retryable: true, code: "unavailable"},
		"request":    {err: &orpheus.Error{Status: 400, Code: "invalid_request"}, retryable: false, code: "invalid_request"},
		"cancelled":  {err: context.Canceled, retryable: false, code: "request_cancelled"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := &orpheusMock{find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
				return nil, test.err
			}}
			reconciler := NewReconciler(zap.NewNop(), client, func(review.Input) (workflow.SessionContract, error) {
				return workflow.SessionContract{}, nil
			})

			err := reconciler.Reconcile(context.Background(), eligibleReviewInput())

			var lifecycleError *LifecycleError
			require.ErrorAs(t, err, &lifecycleError)
			require.Equal(t, LifecyclePhaseFind, lifecycleError.Phase)
			require.Equal(t, test.code, lifecycleError.Code)
			require.Equal(t, test.retryable, lifecycleError.Retryable)
			require.Equal(t, test.retryable, IsRetryable(err))
		})
	}
}

func TestReconcilerRecoversUncertainCreateWithoutSecondAnalysis(t *testing.T) {
	t.Parallel()

	client := &orpheusMock{}
	client.find = func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
		if client.findCalls == 1 {
			return nil, nil
		}
		return []orpheus.Session{{ID: "session-1", RunID: "run-1", Status: "running"}}, nil
	}
	client.create = func(context.Context, orpheus.ReviewSessionKey, int64, orpheus.CreateSessionRequest) (orpheus.Accepted, error) {
		return orpheus.Accepted{}, &orpheus.Error{Status: 503, Code: "response_lost"}
	}
	input := eligibleReviewInput()
	contract := testSessionContract(input)
	reconciler := NewReconciler(zap.NewNop(), client, func(review.Input) (workflow.SessionContract, error) {
		return contract, nil
	})

	firstErr := reconciler.Reconcile(context.Background(), input)
	secondErr := reconciler.Reconcile(context.Background(), input)

	require.Error(t, firstErr)
	require.True(t, IsRetryable(firstErr))
	require.NoError(t, secondErr)
	require.Equal(t, 2, client.findCalls)
	require.Equal(t, 1, client.createCalls)
}

func TestReconcilerDoesNotRetryInvalidContractOrDuplicateSessions(t *testing.T) {
	t.Parallel()

	input := eligibleReviewInput()
	client := &orpheusMock{}
	reconciler := NewReconciler(zap.NewNop(), client, func(review.Input) (workflow.SessionContract, error) {
		return workflow.SessionContract{}, &workflow.RequestTooLargeError{Size: 2, Limit: 1}
	})

	err := reconciler.Reconcile(context.Background(), input)

	var lifecycleError *LifecycleError
	require.ErrorAs(t, err, &lifecycleError)
	require.Equal(t, LifecyclePhaseBuild, lifecycleError.Phase)
	require.Equal(t, "session_request_too_large", lifecycleError.Code)
	require.False(t, IsRetryable(err))
	require.Zero(t, client.createCalls)

	client.find = func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
		return []orpheus.Session{{Status: "running"}, {Status: "running"}}, nil
	}
	err = reconciler.Reconcile(context.Background(), input)
	require.ErrorAs(t, err, &lifecycleError)
	require.Equal(t, "multiple_matching_sessions", lifecycleError.Code)
}

func TestReconcilerReturnsPermanentErrorForUnknownStatus(t *testing.T) {
	t.Parallel()

	client := &orpheusMock{find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
		return []orpheus.Session{{Status: "new-status"}}, nil
	}}
	reconciler := NewReconciler(zap.NewNop(), client, func(review.Input) (workflow.SessionContract, error) {
		return workflow.SessionContract{}, errors.New("unused")
	})

	err := reconciler.Reconcile(context.Background(), eligibleReviewInput())

	var lifecycleError *LifecycleError
	require.ErrorAs(t, err, &lifecycleError)
	require.Equal(t, "unknown_run_status", lifecycleError.Code)
	require.False(t, lifecycleError.Retryable)
}

func TestReconcilerAndAdapterHTTPContract(t *testing.T) {
	t.Parallel()

	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestCount++
		require.Equal(t, "Bearer secret", request.Header.Get("Authorization"))
		response.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case http.MethodGet + " /api/v1/runs":
			require.Equal(t, workflow.ID, request.URL.Query().Get("namespace"))
			require.Equal(t, "https://gitlab.example.com:74!2989", request.URL.Query().Get("external_key"))
			require.Equal(t, strings.Repeat("e", 64), request.URL.Query().Get("input_fingerprint"))
			_, _ = response.Write([]byte(`{"items":[],"next_cursor":null}`))
		case http.MethodPost + " /api/v1/sessions":
			require.NotEmpty(t, request.Header.Get("Idempotency-Key"))
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			require.Equal(t, workflow.ID, body["namespace"])
			require.Equal(t, false, body["allow_multiple_runs"])
			configuration := body["configuration"].(map[string]any)
			hooks := configuration["hooks"].(map[string]any)
			require.Contains(t, hooks["before_run"], "git checkout --detach")
			require.Contains(t, hooks["after_run"], "exec python3")
			messages := body["messages"].([]any)
			message := messages[0].(map[string]any)
			metadata := message["metadata"].(map[string]any)
			require.Equal(t, float64(1), metadata["schema_version"])
			response.WriteHeader(http.StatusAccepted)
			_, _ = response.Write([]byte(`{
				"session_id":"00000000-0000-4000-8000-000000000001",
				"run_id":"00000000-0000-4000-8000-000000000002",
				"message_id":"00000000-0000-4000-8000-000000000003"
			}`))
		default:
			http.Error(response, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := orpheus.New(server.URL, "secret", time.Second)
	require.NoError(t, err)
	input := eligibleReviewInput()
	input.MergeRequest.WebURL = "https://gitlab.example.com/team/project/-/merge_requests/2989"
	reconciler := NewReconciler(zap.NewNop(), client, func(input review.Input) (workflow.SessionContract, error) {
		return workflow.BuildSessionContract(input, workflow.Options{
			GitLabHost:         "https://gitlab.example.com",
			Instructions:       "# Project policy\n\nReview the pinned diff.\n",
			AgentProfile:       "review-profile",
			SandboxTemplate:    "review-sandbox",
			RunTimeoutSeconds:  3600,
			HookTimeoutSeconds: 120,
			MaxRequestBytes:    1 << 20,
		})
	})

	err = reconciler.Reconcile(context.Background(), input)

	require.NoError(t, err)
	require.Equal(t, 2, requestCount)
}

func TestReconcilerWorkersProcessInputsConcurrently(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	client := concurrentOrpheusStub{find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
		started <- struct{}{}
		<-release
		return []orpheus.Session{{Status: "running"}}, nil
	}}
	reconciler := NewReconciler(
		zap.NewNop(),
		client,
		func(review.Input) (workflow.SessionContract, error) { return workflow.SessionContract{}, nil },
		ReconcilerConfig{WorkerCount: 2, QueueCapacity: 2},
	)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- reconciler.Run(ctx) }()
	<-reconciler.started
	first := eligibleReviewInput()
	second := anotherEligibleReviewInput()
	require.NoError(t, reconciler.Submit(context.Background(), review.Snapshot{Inputs: []review.Input{first, second}}))

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("reconcile inputs did not start concurrently")
		}
	}
	close(release)
	cancel()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	require.NoError(t, reconciler.Stop(shutdownCtx))
	require.NoError(t, <-runDone)
}

func TestReconcilerCancelsActiveRunRemovedBetweenSnapshots(t *testing.T) {
	finds := make(chan struct{}, 2)
	cancelled := make(chan struct{})
	client := concurrentOrpheusStub{
		find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
			finds <- struct{}{}
			return []orpheus.Session{{ID: "session-1", RunID: "run-1", Status: "running"}}, nil
		},
		cancel: func(_ context.Context, sessionID, runID string) error {
			require.Equal(t, "session-1", sessionID)
			require.Equal(t, "run-1", runID)
			close(cancelled)
			return nil
		},
	}
	core, logs := observer.New(zap.DebugLevel)
	reconciler := NewReconciler(
		zap.New(core),
		client,
		func(review.Input) (workflow.SessionContract, error) { return workflow.SessionContract{}, nil },
		ReconcilerConfig{WorkerCount: 2, QueueCapacity: 2},
	)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- reconciler.Run(ctx) }()
	<-reconciler.started

	require.NoError(t, reconciler.Submit(context.Background(), review.Snapshot{Inputs: []review.Input{eligibleReviewInput()}}))
	<-finds
	require.NoError(t, reconciler.Submit(context.Background(), review.Snapshot{}))
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("removed review run was not cancelled")
	}

	cancel()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	require.NoError(t, reconciler.Stop(shutdownCtx))
	require.NoError(t, <-runDone)
	require.Equal(t, 1, logs.FilterMessage("cancelled Orpheus review after merge request left reviewer snapshot").Len())
}

func TestReconcilerRetriesTransientCancellationOnNextSnapshot(t *testing.T) {
	cancelCalls := 0
	client := &orpheusMock{
		find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
			return []orpheus.Session{{ID: "session-1", RunID: "run-1", Status: "running"}}, nil
		},
		cancel: func(context.Context, string, string) error {
			cancelCalls++
			if cancelCalls == 1 {
				return &orpheus.Error{Status: http.StatusServiceUnavailable, Code: "unavailable"}
			}
			return nil
		},
	}
	reconciler := NewReconciler(zap.NewNop(), client, func(review.Input) (workflow.SessionContract, error) {
		return workflow.SessionContract{}, nil
	})

	reconciler.processSnapshot(context.Background(), review.Snapshot{Inputs: []review.Input{eligibleReviewInput()}})
	reconciler.processSnapshot(context.Background(), review.Snapshot{})
	require.Equal(t, 1, cancelCalls)
	reconciler.processSnapshot(context.Background(), review.Snapshot{})

	require.Equal(t, 2, cancelCalls)
	require.Empty(t, reconciler.removed)
}

func TestReconcilerSubmitRejectsDuplicateKeysAndDoesNotBlockWhenFull(t *testing.T) {
	reconciler := NewReconciler(
		zap.NewNop(),
		concurrentOrpheusStub{},
		func(review.Input) (workflow.SessionContract, error) { return workflow.SessionContract{}, nil },
		ReconcilerConfig{WorkerCount: 1, QueueCapacity: 1},
	)
	input := eligibleReviewInput()

	require.Error(t, reconciler.Submit(context.Background(), review.Snapshot{Inputs: []review.Input{input, input}}))
	require.NoError(t, reconciler.Submit(context.Background(), review.Snapshot{Inputs: []review.Input{input}}))
	require.Len(t, reconciler.queue, 1)
	require.ErrorIs(t, reconciler.Submit(context.Background(), review.Snapshot{Inputs: []review.Input{anotherEligibleReviewInput()}}), ErrReconcileQueueFull)
	require.Len(t, reconciler.queue, 1)
}

func TestReconcilerGracefulShutdownDrainsAcceptedInputs(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	client := concurrentOrpheusStub{find: func(context.Context, orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
		close(started)
		<-release
		return []orpheus.Session{{Status: "running"}}, nil
	}}
	core, logs := observer.New(zap.DebugLevel)
	reconciler := NewReconciler(
		zap.New(core),
		client,
		func(review.Input) (workflow.SessionContract, error) { return workflow.SessionContract{}, nil },
		ReconcilerConfig{WorkerCount: 1, QueueCapacity: 1},
	)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- reconciler.Run(ctx) }()
	<-reconciler.started
	require.NoError(t, reconciler.Submit(context.Background(), review.Snapshot{Inputs: []review.Input{eligibleReviewInput()}}))
	<-started
	cancel()
	stopDone := make(chan error, 1)
	go func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
		defer cancelShutdown()
		stopDone <- reconciler.Stop(shutdownCtx)
	}()
	select {
	case <-stopDone:
		t.Fatal("reconciler stopped before the accepted input completed")
	default:
	}
	close(release)

	require.NoError(t, <-stopDone)
	require.NoError(t, <-runDone)
	require.Equal(t, 1, logs.FilterMessage("stopping review reconciler").Len())
	require.Equal(t, 1, logs.FilterMessage("review reconciler stopped").Len())
	require.ErrorIs(t, reconciler.Submit(context.Background(), review.Snapshot{Inputs: []review.Input{anotherEligibleReviewInput()}}), ErrReconcilerStopping)
}

func TestReconcilerCancelsActiveRequestsAfterShutdownTimeout(t *testing.T) {
	started := make(chan struct{})
	requestCancelled := make(chan struct{})
	client := concurrentOrpheusStub{find: func(ctx context.Context, _ orpheus.ReviewSessionKey) ([]orpheus.Session, error) {
		close(started)
		<-ctx.Done()
		close(requestCancelled)
		return nil, ctx.Err()
	}}
	core, logs := observer.New(zap.DebugLevel)
	reconciler := NewReconciler(
		zap.New(core),
		client,
		func(review.Input) (workflow.SessionContract, error) { return workflow.SessionContract{}, nil },
		ReconcilerConfig{WorkerCount: 1, QueueCapacity: 1},
	)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- reconciler.Run(ctx) }()
	<-reconciler.started
	require.NoError(t, reconciler.Submit(context.Background(), review.Snapshot{Inputs: []review.Input{eligibleReviewInput()}}))
	<-started
	cancel()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelShutdown()
	require.ErrorIs(t, reconciler.Stop(shutdownCtx), context.DeadlineExceeded)

	select {
	case <-requestCancelled:
	case <-time.After(time.Second):
		t.Fatal("active Orpheus request was not cancelled")
	}
	require.NoError(t, <-runDone)
	require.Equal(t, 1, logs.FilterMessage("review reconciler shutdown timed out; cancelling active requests").Len())
}

func testSessionContract(input review.Input) workflow.SessionContract {
	allowMultiple := false
	key := orpheus.ReviewSessionKey{
		Namespace:         workflow.ID,
		MRKey:             input.MRKey,
		ReviewFingerprint: input.ReviewFingerprint,
	}
	return workflow.SessionContract{
		Key:        key,
		ReviewerID: input.Reviewer.ID,
		Request: orpheus.CreateSessionRequest{
			AllowMultipleRuns: &allowMultiple,
			Messages:          []orpheus.TextMessage{{Text: "review"}},
		},
	}
}

func eligibleReviewInput() review.Input {
	reviewer := gitlab.User{ID: 42, Username: "reviewer"}
	return review.Input{
		MRKey:             "https://gitlab.example.com:74!2989",
		DiffFingerprint:   strings.Repeat("d", 64),
		ReviewFingerprint: strings.Repeat("e", 64),
		Reviewer:          reviewer,
		Project: gitlab.Project{
			ID:                74,
			PathWithNamespace: "team/project",
			SSHURLToRepo:      "git@gitlab.example.com:team/project.git",
		},
		MergeRequest: gitlab.MergeRequest{
			ProjectID: 74,
			IID:       2989,
			State:     "opened",
			Reviewers: []gitlab.User{reviewer},
			DiffRefs: gitlab.DiffRefs{
				BaseSHA:  "base",
				StartSHA: "start",
				HeadSHA:  "head",
			},
		},
	}
}

func anotherEligibleReviewInput() review.Input {
	input := eligibleReviewInput()
	input.MRKey = "https://gitlab.example.com:74!2990"
	input.ReviewFingerprint = strings.Repeat("f", 64)
	input.MergeRequest.IID = 2990
	return input
}
