package bot

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"

	"anydloadbot/internal/downloader"
)

// plainProvider supports downloads but not inline mode.
type plainProvider struct{}

func (plainProvider) Name() string { return "Plain" }
func (plainProvider) FindURL(text string) (string, bool) {
	if strings.Contains(text, "plain://") {
		return "plain://x", true
	}
	return "", false
}
func (plainProvider) Download(context.Context, string) (*downloader.Media, error) { return nil, nil }

// inlineProvider also implements downloader.Resolver.
type inlineProvider struct {
	plainProvider
	src *downloader.Source
}

func (inlineProvider) Name() string { return "Inline" }
func (inlineProvider) FindURL(text string) (string, bool) {
	if strings.Contains(text, "inline://") {
		return "inline://x", true
	}
	return "", false
}

func (p inlineProvider) Resolve(context.Context, string) (*downloader.Source, error) {
	if p.src == nil {
		return nil, downloader.ErrNoMedia
	}
	return p.src, nil
}

func newTestBot(ps ...downloader.Provider) *Bot {
	return &Bot{
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		providers: downloader.NewRegistry(ps...),
		jobs:      newJobs(DefaultUserLimit, DefaultTotalLimit),
		files:     newFileCache(maxCachedFiles),
	}
}

func TestInlineResults(t *testing.T) {
	t.Parallel()

	good := &downloader.Source{URL: "https://cdn/v.mp4", ThumbnailURL: "https://cdn/t.jpg", Title: "T", MimeType: "video/mp4"}

	tests := []struct {
		name  string
		query string
		prov  downloader.Provider
		want  int
	}{
		{"video link", "inline://pin", inlineProvider{src: good}, 1},
		{"no link", "hello", inlineProvider{src: good}, 0},
		{"provider without inline support", "plain://pin", plainProvider{}, 0},
		{"no media", "inline://pin", inlineProvider{}, 0},
		{"missing thumbnail", "inline://pin", inlineProvider{src: &downloader.Source{URL: "https://cdn/v.mp4"}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := newTestBot(tt.prov).inlineResults(context.Background(), tt.query)
			if got == nil {
				t.Fatal("results is nil; Telegram needs a JSON array")
			}
			if len(got) != tt.want {
				t.Fatalf("got %d results, want %d", len(got), tt.want)
			}
			if tt.want == 0 {
				return
			}
			v, ok := got[0].(*models.InlineQueryResultVideo)
			if !ok {
				t.Fatalf("result is %T, want *InlineQueryResultVideo", got[0])
			}
			if v.VideoURL != good.URL || v.ThumbnailURL != good.ThumbnailURL || v.MimeType != "video/mp4" {
				t.Errorf("unexpected result: %+v", v)
			}
			if len(v.ID) == 0 || len(v.ID) > 64 {
				t.Errorf("result ID %q must be 1-64 bytes", v.ID)
			}
		})
	}
}
