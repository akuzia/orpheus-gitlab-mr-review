package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/app"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/config"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/observability"

	"go.uber.org/zap"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := observability.NewStartupLogger()
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load configuration", zap.Error(err))
		return 1
	}

	application, err := app.New(cfg)
	if err != nil {
		logger.Error("failed to initialize application", zap.Error(err))
		return 1
	}
	defer application.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := application.Run(ctx); err != nil {
		logger.Error("application failed", zap.Error(err))
		return 1
	}
	return 0
}
