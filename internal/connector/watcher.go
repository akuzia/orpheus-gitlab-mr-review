package connector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/gitlab"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/review"

	"go.uber.org/zap"
)

type gitLabClient interface {
	CurrentUser(ctx context.Context) (gitlab.User, error)
	ListMergeRequestsForReview(ctx context.Context, reviewerID int64) ([]gitlab.MergeRequest, error)
	GetReviewInput(ctx context.Context, projectID, iid int64) (gitlab.ReviewInput, error)
}

type WatcherConfig struct {
	PollInterval time.Duration
	GitLabHost   string
}

// Watcher periodically discovers merge requests assigned to the authenticated
// GitLab reviewer. Reconciliation and admission will be performed for every
// successfully fetched review input as those parts of the connector are added.
type Watcher struct {
	logger         *zap.Logger
	gitLabClient   gitLabClient
	pollInterval   time.Duration
	gitLabHost     string
	stopOnce       sync.Once
	stop           chan struct{}
	started        chan struct{}
	done           chan struct{}
	cancelMu       sync.Mutex
	cancelRequests context.CancelFunc
}

func NewWatcher(
	logger *zap.Logger,
	gitLabClient gitLabClient,
	cfg WatcherConfig,
) *Watcher {
	return &Watcher{
		logger:       logger,
		gitLabClient: gitLabClient,
		pollInterval: cfg.PollInterval,
		gitLabHost:   cfg.GitLabHost,
		stop:         make(chan struct{}),
		started:      make(chan struct{}),
		done:         make(chan struct{}),
	}
}

func (w *Watcher) Run(ctx context.Context) error {
	requestCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	w.cancelMu.Lock()
	w.cancelRequests = cancelRequests
	w.cancelMu.Unlock()
	close(w.started)
	defer close(w.done)
	defer func() {
		cancelRequests()
		w.cancelMu.Lock()
		w.cancelRequests = nil
		w.cancelMu.Unlock()
	}()

	if w.stopping(ctx) {
		return nil
	}

	user, err := w.gitLabClient.CurrentUser(requestCtx)
	if err != nil {
		return fmt.Errorf("resolve authenticated GitLab reviewer: %w", err)
	}

	w.logger.Info("authenticated GitLab user",
		zap.Int64("gitlab_user_id", user.ID),
		zap.String("gitlab_username", user.Username),
	)

	if w.stopping(ctx) {
		return nil
	}
	w.poll(requestCtx, user)

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.stop:
			return nil
		case <-ticker.C:
			if w.stopping(ctx) {
				return nil
			}
			w.poll(requestCtx, user)
		}
	}
}

func (w *Watcher) Stop(ctx context.Context) error {
	w.stopOnce.Do(func() {
		w.logger.Debug("stopping merge request watcher")
		close(w.stop)
	})

	select {
	case <-w.started:
	case <-ctx.Done():
		w.logger.Debug("merge request watcher shutdown timed out before start",
			zap.Error(ctx.Err()),
		)
		return ctx.Err()
	}

	select {
	case <-w.done:
		w.logger.Debug("merge request watcher stopped")
		return nil
	case <-ctx.Done():
		w.logger.Debug("merge request watcher shutdown timed out; cancelling active requests",
			zap.Error(ctx.Err()),
		)
		w.cancelMu.Lock()
		cancelRequests := w.cancelRequests
		w.cancelMu.Unlock()
		if cancelRequests != nil {
			cancelRequests()
		}
		return ctx.Err()
	}
}

func (w *Watcher) stopping(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-w.stop:
		return true
	default:
		return false
	}
}

func (w *Watcher) poll(ctx context.Context, reviewer gitlab.User) {
	mergeRequests, err := w.gitLabClient.ListMergeRequestsForReview(ctx, reviewer.ID)
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return
		}
		w.logger.Error("failed to poll GitLab merge requests", zap.Error(err))
		return
	}

	w.logger.Info("found GitLab merge requests assigned for review",
		zap.Int("merge_request_count", len(mergeRequests)),
	)
	for _, candidate := range mergeRequests {
		gitLabInput, err := w.gitLabClient.GetReviewInput(ctx, candidate.ProjectID, candidate.IID)
		if err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return
			}
			w.logger.Error("failed to fetch GitLab review input",
				zap.Int64("project_id", candidate.ProjectID),
				zap.Int64("merge_request_iid", candidate.IID),
				zap.Error(err),
			)
			return
		}

		input, err := review.NewInput(w.gitLabHost, reviewer, gitLabInput)
		if err != nil {
			w.logger.Error("failed to prepare GitLab review input",
				zap.Int64("project_id", candidate.ProjectID),
				zap.Int64("merge_request_iid", candidate.IID),
				zap.Error(err),
			)
			return
		}
		eligibility := review.EvaluateInputEligibility(input)
		if !eligibility.Eligible {
			w.logger.Debug("GitLab merge request is not eligible for review",
				zap.String("merge_request_key", input.MRKey),
				zap.String("project_path", input.Project.PathWithNamespace),
				zap.Strings("reasons", ineligibilityReasons(eligibility.Reasons)),
			)
			continue
		}

		w.logger.Debug("prepared eligible GitLab review input",
			zap.String("merge_request_key", input.MRKey),
			zap.String("project_path", input.Project.PathWithNamespace),
			zap.String("diff_fingerprint", input.DiffFingerprint),
			zap.String("review_fingerprint", input.ReviewFingerprint),
		)
	}
}

func ineligibilityReasons(reasons []review.IneligibilityReason) []string {
	values := make([]string, len(reasons))
	for i, reason := range reasons {
		values[i] = string(reason)
	}

	return values
}
