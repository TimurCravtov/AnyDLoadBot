// Package config loads and validates application configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
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
	// InstagramCookies is an optional path to a Netscape cookies.txt from a
	// logged-in browser, needed for Instagram posts that require login.
	InstagramCookies string
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
	return load(os.Getenv)
}

func load(getenv func(string) string) (Config, error) {
	var errs []error

	cfg := Config{
		Token:    strings.TrimSpace(getenv("BOT_TOKEN")),
		LogLevel: slog.LevelInfo,
		LogJSON:  getenv("LOG_FORMAT") == "json",

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

	cfg.InstagramCookies = strings.TrimSpace(getenv("INSTAGRAM_COOKIES"))

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
