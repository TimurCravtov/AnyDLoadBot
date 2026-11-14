package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr bool
	}{
		{
			name: "defaults",
			env:  map[string]string{"BOT_TOKEN": "123:abc"},
			want: Config{Token: "123:abc", LogLevel: slog.LevelInfo, YtDlpPath: "yt-dlp", MaxUserDownloads: 5, MaxTotalDownloads: 100},
		},
		{
			name: "overrides",
			env: map[string]string{
				"BOT_TOKEN": " 123:abc ", "LOG_LEVEL": "debug", "LOG_FORMAT": "json", "YTDLP_PATH": " /opt/yt-dlp ",
				"STORAGE_CHAT_ID": "-1001234567890", "MAX_USER_DOWNLOADS": "2", "MAX_TOTAL_DOWNLOADS": "9",
				"YTDLP_COOKIES": " /run/cookies.txt ", "YTDLP_PROXY": "socks5://10.0.0.1:1080",
			},
			want: Config{
				Token: "123:abc", LogLevel: slog.LevelDebug, LogJSON: true,
				YtDlpPath: "/opt/yt-dlp", StorageChatID: -1001234567890,
				MaxUserDownloads: 2, MaxTotalDownloads: 9,
				YtDlpCookies: "/run/cookies.txt", YtDlpProxy: "socks5://10.0.0.1:1080",
			},
		},
		{name: "proxy without scheme", env: map[string]string{"BOT_TOKEN": "t", "YTDLP_PROXY": "10.0.0.1:1080"}, wantErr: true},
		{name: "proxy with a strange scheme", env: map[string]string{"BOT_TOKEN": "t", "YTDLP_PROXY": "ftp://10.0.0.1"}, wantErr: true},
		{name: "bad user limit", env: map[string]string{"BOT_TOKEN": "t", "MAX_USER_DOWNLOADS": "0"}, wantErr: true},
		{name: "bad total limit", env: map[string]string{"BOT_TOKEN": "t", "MAX_TOTAL_DOWNLOADS": "many"}, wantErr: true},
		{name: "bad storage chat", env: map[string]string{"BOT_TOKEN": "t", "STORAGE_CHAT_ID": "me"}, wantErr: true},
		{name: "missing token", env: map[string]string{}, wantErr: true},
		{name: "bad level", env: map[string]string{"BOT_TOKEN": "t", "LOG_LEVEL": "loud"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := load(env(tt.env))
			if (err != nil) != tt.wantErr {
				t.Fatalf("load() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("load() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadRejectsMissingCookiesFile(t *testing.T) {
	// Not parallel: it changes the process environment and working directory.
	dir := t.TempDir()
	t.Chdir(dir) // no .env here
	t.Setenv("BOT_TOKEN", "123:abc")
	t.Setenv("YTDLP_COOKIES", filepath.Join(dir, "missing.txt"))

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "YTDLP_COOKIES") {
		t.Fatalf("Load() = %v, want an error naming YTDLP_COOKIES", err)
	}

	cookies := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(cookies, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YTDLP_COOKIES", cookies)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with an existing file: %v", err)
	}
	if cfg.YtDlpCookies != cookies {
		t.Errorf("YtDlpCookies = %q", cfg.YtDlpCookies)
	}
}
