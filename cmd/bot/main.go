// Command bot runs the Telegram bot.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"anydloadbot/internal/bot"
	"anydloadbot/internal/config"
	"anydloadbot/internal/downloader"
	"anydloadbot/internal/downloader/pinterest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg)
	slog.SetDefault(log)

	// Cancelled on SIGINT/SIGTERM; a second signal kills the process immediately.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// To support another site, implement downloader.Provider and add it here.
	providers := downloader.NewRegistry(
		pinterest.New(),
	)

	b, err := bot.New(cfg.Token, providers, log)
	if err != nil {
		return err
	}

	b.Run(ctx)
	return nil
}

func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogJSON {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
