package pinterest

import (
	"context"
	"errors"
	"testing"

	"anydloadbot/internal/downloader"
)

func TestFindURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text string
		want string
		ok   bool
	}{
		{"https://www.pinterest.com/pin/1036179826779090032/", "https://www.pinterest.com/pin/1036179826779090032/", true},
		{"look: https://pin.it/abc123.", "https://pin.it/abc123", true},
		{"(https://ru.pinterest.com/pin/1/)", "https://ru.pinterest.com/pin/1/", true},
		{"https://www.pinterest.co.uk/pin/1/", "https://www.pinterest.co.uk/pin/1/", true},
		{"https://example.com/pinterest.com/pin/1", "", false},
		{"https://evilpinterest.com/pin/1", "", false},
		{"https://pinterest.com.evil.io/pin/1", "", false},
		{"no links here", "", false},
	}
	for _, tt := range tests {
		got, ok := New().FindURL(tt.text)
		if got != tt.want || ok != tt.ok {
			t.Errorf("FindURL(%q) = %q, %v; want %q, %v", tt.text, got, ok, tt.want, tt.ok)
		}
	}
}

func TestParseVideoURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		page    string
		want    string
		wantErr error
	}{
		{
			name: "single mp4 plus hls",
			page: `{"url":"https://v1.pinimg.com/videos/iht/expMp4/dd/52/x_360w.mp4"},{"url":"https://v1.pinimg.com/videos/iht/hls/dd/52/x.m3u8"}`,
			want: "https://v1.pinimg.com/videos/iht/expMp4/dd/52/x_360w.mp4",
		},
		{
			name: "prefers higher quality",
			page: `"https://v1.pinimg.com/videos/mc/720p/a/b.mp4" "https://v1.pinimg.com/videos/mc/360p/a/b.mp4"`,
			want: "https://v1.pinimg.com/videos/mc/720p/a/b.mp4",
		},
		{
			name: "escaped slashes",
			page: `"url":"https://v1.pinimg.com/videos/iht/expMp4/a_720w.mp4"`,
			want: "https://v1.pinimg.com/videos/iht/expMp4/a_720w.mp4",
		},
		{name: "ignores other hosts", page: `https://evil.com/videos/a.mp4`, wantErr: downloader.ErrNoMedia},
		{name: "image pin", page: `{"videos":null}`, wantErr: downloader.ErrNoMedia},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseVideoURL([]byte(tt.page))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDownloadRejectsForeignLinks(t *testing.T) {
	t.Parallel()

	for _, link := range []string{"https://example.com/pin/1/", "ftp://pinterest.com/pin/1/", "not a url"} {
		if _, err := New().Download(context.Background(), link); err == nil {
			t.Errorf("Download(%q) succeeded, want error", link)
		}
	}
}
