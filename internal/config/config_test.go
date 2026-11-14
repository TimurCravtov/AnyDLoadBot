package config

import (
	"log/slog"
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
			want: Config{Token: "123:abc", LogLevel: slog.LevelInfo, YtDlpPath: "yt-dlp"},
		},
		{
			name: "overrides",
			env: map[string]string{
				"BOT_TOKEN": " 123:abc ", "LOG_LEVEL": "debug", "LOG_FORMAT": "json", "YTDLP_PATH": " /opt/yt-dlp ",
				"STORAGE_CHAT_ID": "-1001234567890",
			},
			want: Config{
				Token: "123:abc", LogLevel: slog.LevelDebug, LogJSON: true,
				YtDlpPath: "/opt/yt-dlp", StorageChatID: -1001234567890,
			},
		},
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
