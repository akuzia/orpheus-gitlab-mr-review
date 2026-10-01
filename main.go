package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/app"
	"github.com/orpheus-agents/orpheus-gitlab-mr-review/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	application, err := app.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer application.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := application.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
