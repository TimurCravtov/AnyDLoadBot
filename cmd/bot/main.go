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
	"anydloadbot/internal/downloader/youtube"
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

	b, err := bot.New(cfg.Token, newProviders(cfg, log), log, bot.WithStorageChat(cfg.StorageChatID))
	if err != nil {
		return err
	}

	b.Run(ctx)
	return nil
}

// newProviders builds the registry of supported sites. To support another site,
// implement downloader.Provider and add it here.
func newProviders(cfg config.Config, log *slog.Logger) *downloader.Registry {
	providers := []downloader.Provider{pinterest.New()}

	// YouTube needs external tools; if they are missing, run without it.
	yt, err := youtube.New(youtube.Options{YtDlp: cfg.YtDlpPath})
	if err != nil {
		log.Warn("YouTube support disabled", "err", err)
	} else {
		providers = append(providers, yt)
	}

	return downloader.NewRegistry(providers...)
}

func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogJSON {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
