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
	"anydloadbot/internal/downloader/instagram"
	"anydloadbot/internal/downloader/pinterest"
	"anydloadbot/internal/downloader/twitter"
	"anydloadbot/internal/downloader/youtube"
	"anydloadbot/internal/downloader/ytdlp"
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
	// These sites need nothing but HTTP.
	providers := []downloader.Provider{pinterest.New(), twitter.New()}

	// YouTube and Instagram videos need yt-dlp and ffmpeg; without them, run
	// without those sites.
	yt, err := ytdlp.New(cfg.YtDlpPath, ytdlp.DefaultConcurrency)
	if err != nil {
		log.Warn("YouTube and Instagram support disabled", "err", err)
	} else {
		providers = append(providers,
			youtube.New(yt, youtube.Options{}),
			instagram.New(yt, instagram.Options{Cookies: cfg.InstagramCookies}),
		)
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
