// Package config loads and validates application configuration from the environment.
package config

import (
	"encoding/base64"
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
	// FetchProxy optionally routes the bot's own web requests (Pinterest, X,
	// previews) through a proxy. Telegram's API uses TelegramProxy. (Not named
	// HTTPS_PROXY: Docker treats that name specially during image builds.)
	FetchProxy string
	// TelegramProxy optionally routes all Telegram Bot API traffic (polling,
	// messages, uploads) through a proxy, for servers that can't reach
	// api.telegram.org reliably (getUpdates failing with 502 Bad Gateway).
	TelegramProxy string
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

	// inlineCookies holds decoded NAME_B64 cookie settings until Load writes
	// them to files; indexed like cookieSettings.
	inlineCookies [numCookieSettings]string
}

const numCookieSettings = 3

// cookieSettings are the cookies.txt settings. Each is a path (NAME) or, where
// mounting a file is awkward (Coolify compose apps, for one), the file's
// base64-encoded contents (NAME_B64). Base64 keeps the file's tabs and line
// breaks intact inside an environment variable.
var cookieSettings = [numCookieSettings]struct {
	env  string
	path func(*Config) *string
}{
	{"YTDLP_COOKIES", func(c *Config) *string { return &c.YtDlpCookies }},
	{"INSTAGRAM_COOKIES", func(c *Config) *string { return &c.InstagramCookies }},
	{"REDDIT_COOKIES", func(c *Config) *string { return &c.RedditCookies }},
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

	// Inline (base64) cookies go into private temporary files, so the rest of
	// the bot only ever deals with paths. (Done here, not in load, to keep load pure.)
	for i, s := range cookieSettings {
		if cfg.inlineCookies[i] == "" {
			continue
		}
		path, err := writePrivateFile(cfg.inlineCookies[i])
		if err != nil {
			return Config{}, fmt.Errorf("%s_B64: %w", s.env, err)
		}
		*s.path(&cfg) = path
		cfg.inlineCookies[i] = ""
	}

	// A cookies path that doesn't exist would otherwise only show up as failing
	// downloads, so fail at startup.
	var errs []error
	for _, s := range cookieSettings {
		path := *s.path(&cfg)
		if path == "" {
			continue
		}
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			errs = append(errs, fmt.Errorf("%s: %q is not a readable file", s.env, path))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// writePrivateFile stores data in a new temporary file readable only by this
// process's user and returns its path.
func writePrivateFile(data string) (string, error) {
	f, err := os.CreateTemp("", "cookies-*.txt") // created with mode 0600
	if err != nil {
		return "", err
	}
	_, werr := f.WriteString(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(f.Name())
		return "", werr
	}
	return f.Name(), nil
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

	for i, s := range cookieSettings {
		path := strings.TrimSpace(getenv(s.env))
		// Base64 may arrive wrapped over several lines; whitespace is never part of it.
		b64 := strings.Join(strings.Fields(getenv(s.env+"_B64")), "")
		switch {
		case path != "" && b64 != "":
			errs = append(errs, fmt.Errorf("set either %s or %s_B64, not both", s.env, s.env))
		case b64 != "":
			data, err := base64.StdEncoding.DecodeString(b64)
			if err != nil || len(data) == 0 {
				// Never echo the value: it is a secret.
				errs = append(errs, fmt.Errorf("%s_B64: not a valid base64-encoded cookies file", s.env))
				continue
			}
			cfg.inlineCookies[i] = string(data)
		default:
			*s.path(&cfg) = path
		}
	}

	// yt-dlp understands socks4; Go's HTTP client (FETCH_PROXY, TELEGRAM_PROXY) doesn't.
	goSchemes := []string{"http", "https", "socks5", "socks5h"}
	for _, p := range []struct {
		env     string
		dst     *string
		schemes []string
	}{
		{"YTDLP_PROXY", &cfg.YtDlpProxy, append([]string{"socks4"}, goSchemes...)},
		{"FETCH_PROXY", &cfg.FetchProxy, goSchemes},
		{"TELEGRAM_PROXY", &cfg.TelegramProxy, goSchemes},
	} {
		v := strings.TrimSpace(getenv(p.env))
		if v == "" {
			continue
		}
		if u, err := url.Parse(v); err != nil || u.Host == "" || !slices.Contains(p.schemes, u.Scheme) {
			errs = append(errs, fmt.Errorf("%s: must look like http://host:port or socks5://host:port", p.env))
			continue
		}
		*p.dst = v
	}

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
