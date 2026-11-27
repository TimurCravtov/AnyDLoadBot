// Command bot runs the Telegram bot.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"anydloadbot/internal/bot"
	"anydloadbot/internal/config"
	"anydloadbot/internal/downloader"
	"anydloadbot/internal/downloader/instagram"
	"anydloadbot/internal/downloader/pinterest"
	"anydloadbot/internal/downloader/spotify"
	"anydloadbot/internal/downloader/twitter"
	"anydloadbot/internal/downloader/youtube"
	"anydloadbot/internal/downloader/ytdlp"
	"anydloadbot/internal/downloader/ytdlpsite"
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

	var proxy *url.URL
	if cfg.TelegramProxy != "" {
		if proxy, err = url.Parse(cfg.TelegramProxy); err != nil { // already validated by config.Load
			return fmt.Errorf("TELEGRAM_PROXY: %w", err)
		}
	}

	// "bot logout-cloud": the one-time step before using a local Bot API server.
	if len(os.Args) > 1 && os.Args[1] == "logout-cloud" {
		if err := bot.LogOutCloud(ctx, cfg.Token, proxy); err != nil {
			return err
		}
		fmt.Println("Logged out of api.telegram.org. The bot can now use the local Bot API server " +
			"(and can't log back in to api.telegram.org for 10 minutes).")
		return nil
	}

	botOpts := []bot.Option{bot.WithStorageChat(cfg.StorageChatID), bot.WithLimits(cfg.MaxUserDownloads, cfg.MaxTotalDownloads)}
	switch {
	case cfg.TelegramAPIURL != "":
		// The local server is on the compose network; a proxy couldn't reach it.
		botOpts = append(botOpts, bot.WithAPIServer(cfg.TelegramAPIURL))
		u, _ := url.Parse(cfg.TelegramAPIURL)
		log.Info("Telegram API requests go to a local Bot API server", "server", u.Host)
		if proxy != nil {
			log.Info("TELEGRAM_PROXY is not used with a local Bot API server")
		}
	case proxy != nil:
		botOpts = append(botOpts, bot.WithAPIProxy(proxy))
		log.Info("Telegram API requests go through a proxy", "proxy", proxy.Host) // host only: no credentials
	}

	providers, maxFileSize := newProviders(cfg, log)
	botOpts = append(botOpts, bot.WithMaxFileSize(maxFileSize))
	b, err := bot.New(cfg.Token, providers, log, botOpts...)
	if err != nil {
		return err
	}

	return b.Run(ctx)
}

// newProviders builds the registry of supported sites, and says how big a file
// they may return. To support another site, implement downloader.Provider and
// add it here.
func newProviders(cfg config.Config, log *slog.Logger) (*downloader.Registry, int64) {
	maxFileSize := int64(downloader.MaxSize)
	// Must happen before any provider (and so any Fetcher) is created.
	if cfg.FetchProxy != "" {
		if err := downloader.SetProxy(cfg.FetchProxy); err != nil {
			log.Error("FETCH_PROXY not applied", "err", err)
		} else if u, err := url.Parse(cfg.FetchProxy); err == nil {
			log.Info("web requests go through a proxy", "proxy", u.Host) // host only: no credentials
		}
	}

	// YouTube, Instagram and Spotify need yt-dlp and ffmpeg; without them, run
	// without those sites.
	yt, err := ytdlp.New(cfg.YtDlpPath, ytdlp.DefaultConcurrency, ytdlp.Options{
		Cookies: cfg.YtDlpCookies, Proxy: cfg.YtDlpProxy, POTServer: cfg.YtDlpPOTServer, Server: cfg.YtDlpServer,
	})

	// With a local Bot API server sharing MEDIA_DIR, yt-dlp's downloads stay on
	// disk and the server reads them itself, so they can be much bigger.
	switch {
	case err != nil:
	case cfg.MediaDir == "":
	case cfg.TelegramAPIURL == "":
		log.Info("MEDIA_DIR is only used with a local Bot API server (TELEGRAM_API_URL)")
	default:
		if kerr := yt.KeepFiles(cfg.MediaDir, downloader.MaxLocalSize); kerr != nil {
			log.Error("MEDIA_DIR not usable, files stay limited to 50 MB", "err", kerr)
		} else {
			maxFileSize = downloader.MaxLocalSize
			log.Info("downloads are sent from disk through the local Bot API server",
				"dir", cfg.MediaDir, "max_mb", maxFileSize>>20)
		}
	}

	// YouTube (and Spotify, which is fetched from YouTube) can use its own proxy
	// setting: with cookies and a PO token it often works without WARP, which
	// saves a detour on every request.
	var ytExtra []string
	switch cfg.YouTubeProxy {
	case "":
	case "direct":
		ytExtra = ytdlp.DirectArgs
		log.Info("YouTube is reached directly, without the yt-dlp proxy")
	default:
		ytExtra = []string{"--proxy", cfg.YouTubeProxy}
		if u, err := url.Parse(cfg.YouTubeProxy); err == nil {
			log.Info("YouTube goes through its own proxy", "proxy", u.Host)
		}
	}

	// These sites need nothing but HTTP; Pinterest can also offer MP3 when yt-dlp is there.
	var pin downloader.Provider = pinterest.New()
	if err == nil {
		pin = pinterest.WithAudio(pinterest.New(), yt)
	}
	providers := []downloader.Provider{pin, twitter.New()}

	if err != nil {
		log.Warn("YouTube, Instagram and Spotify support disabled", "err", err)
	} else {
		providers = append(providers,
			youtube.New(yt, youtube.Options{Extra: ytExtra}),
			instagram.New(yt, instagram.Options{Cookies: cfg.InstagramCookies}),
			spotify.New(yt, spotify.Options{Extra: ytExtra}),
			ytdlpsite.WithInline(ytdlpsite.New(yt, ytdlpsite.TikTok(), ytdlpsite.Options{})),
		)

		// Reddit refuses anonymous requests, so it is only enabled with a logged-in cookies.txt.
		if cfg.RedditCookies != "" {
			providers = append(providers, ytdlpsite.WithInline(ytdlpsite.New(yt, ytdlpsite.Reddit(), ytdlpsite.Options{Cookies: cfg.RedditCookies})))
		} else {
			log.Info("Reddit support disabled: set REDDIT_COOKIES to a logged-in cookies.txt to enable it")
		}
	}

	return downloader.NewRegistry(providers...), maxFileSize
}

func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogJSON {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
