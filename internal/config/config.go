// Package config loads and validates application configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
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
	}

	if cfg.Token == "" {
		errs = append(errs, errors.New("BOT_TOKEN is required"))
	}

	if v := getenv("LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}
