// Package config loads and validates application configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds all runtime settings for the bot.
type Config struct {
	// Token is the Telegram Bot API token from @BotFather.
	Token string
	// LogLevel is the minimum log level (debug, info, warn, error).
	LogLevel slog.Level
	// LogJSON switches logging from human-readable text to JSON.
	LogJSON bool
	// YtDlpPath is the yt-dlp executable used for YouTube downloads.
	YtDlpPath string
	// YtDlpCookies is an optional cookies.txt applied to every yt-dlp run.
	// YouTube, in particular, blocks datacenter IPs unless you are logged in.
	YtDlpCookies string
	// YtDlpProxy optionally routes yt-dlp through a proxy (http, https, socks5).
	YtDlpProxy string
	// InstagramCookies is an optional path to a Netscape cookies.txt from a
	// logged-in browser, needed for Instagram posts that require login.
	InstagramCookies string
	// RedditCookies is a path to a logged-in cookies.txt; Reddit support is
	// only enabled when it is set, because Reddit refuses anonymous requests.
	RedditCookies string
	// MaxUserDownloads and MaxTotalDownloads cap concurrent downloads per user
	// and across the whole bot.
	MaxUserDownloads  int
	MaxTotalDownloads int
	// StorageChatID is a private chat the bot uploads videos to for inline mode
	// (0 = disabled). See bot.WithStorageChat.
	StorageChatID int64
}

// Load reads configuration from environment variables, first loading a .env
// file from the working directory if one exists. Variables already set in the
// real environment take precedence over the file. All validation problems are
// collected and returned together.
func Load() (Config, error) {
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}
	cfg, err := load(os.Getenv)
	if err != nil {
		return Config{}, err
	}
	// A cookies path that doesn't exist would otherwise only show up as failing
	// downloads, so fail at startup. (Checked here, not in load, to keep load pure.)
	var errs []error
	for name, path := range map[string]string{
		"YTDLP_COOKIES": cfg.YtDlpCookies, "INSTAGRAM_COOKIES": cfg.InstagramCookies, "REDDIT_COOKIES": cfg.RedditCookies,
	} {
		if path == "" {
			continue
		}
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			errs = append(errs, fmt.Errorf("%s: %q is not a readable file", name, path))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

func load(getenv func(string) string) (Config, error) {
	var errs []error

	cfg := Config{
		Token:    strings.TrimSpace(getenv("BOT_TOKEN")),
		LogLevel: slog.LevelInfo,
		LogJSON:  getenv("LOG_FORMAT") == "json",

		MaxUserDownloads:  5,
		MaxTotalDownloads: 100,

		YtDlpPath: "yt-dlp",
	}
	if v := strings.TrimSpace(getenv("YTDLP_PATH")); v != "" {
		cfg.YtDlpPath = v
	}

	if cfg.Token == "" {
		errs = append(errs, errors.New("BOT_TOKEN is required"))
	}

	if v := getenv("LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
		}
	}

	for _, l := range []struct {
		env string
		dst *int
	}{{"MAX_USER_DOWNLOADS", &cfg.MaxUserDownloads}, {"MAX_TOTAL_DOWNLOADS", &cfg.MaxTotalDownloads}} {
		if v := strings.TrimSpace(getenv(l.env)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				errs = append(errs, fmt.Errorf("%s: %q is not a positive number", l.env, v))
				continue
			}
			*l.dst = n
		}
	}

	cfg.YtDlpCookies = strings.TrimSpace(getenv("YTDLP_COOKIES"))
	if v := strings.TrimSpace(getenv("YTDLP_PROXY")); v != "" {
		if u, err := url.Parse(v); err != nil || u.Host == "" || !slices.Contains([]string{"http", "https", "socks4", "socks5", "socks5h"}, u.Scheme) {
			errs = append(errs, errors.New("YTDLP_PROXY: must look like http://host:port or socks5://host:port"))
		} else {
			cfg.YtDlpProxy = v
		}
	}
	cfg.InstagramCookies = strings.TrimSpace(getenv("INSTAGRAM_COOKIES"))
	cfg.RedditCookies = strings.TrimSpace(getenv("REDDIT_COOKIES"))

	if v := strings.TrimSpace(getenv("STORAGE_CHAT_ID")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id == 0 {
			errs = append(errs, fmt.Errorf("STORAGE_CHAT_ID: %q is not a valid chat id", v))
		} else {
			cfg.StorageChatID = id
		}
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}
