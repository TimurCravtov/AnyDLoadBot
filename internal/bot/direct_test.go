package bot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"anydloadbot/internal/downloader"
)

const directURL = "https://cdn.example/v.mp4"

// linkProvider has direct media URLs (a downloader.Linker), like Instagram.
type linkProvider struct {
	mu                 sync.Mutex
	lookups, downloads int
	lookupErr          error
}

func (*linkProvider) Name() string { return "Link" }
func (*linkProvider) FindURL(text string) (string, bool) {
	for _, w := range strings.Fields(text) {
		if strings.HasPrefix(w, "link://") {
			return w, true
		}
	}
	return "", false
}

func (p *linkProvider) MediaURL(context.Context, string) (*downloader.Source, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lookups++
	if p.lookupErr != nil {
		return nil, p.lookupErr
	}
	return &downloader.Source{Kind: downloader.KindVideo, URL: directURL, MimeType: "video/mp4"}, nil
}

func (p *linkProvider) Download(context.Context, string) (*downloader.Media, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.downloads++
	return &downloader.Media{Kind: downloader.KindVideo, Filename: "v.mp4", Data: []byte("video"), Title: "Clip"}, nil
}

// sendVideos returns the video field of every sendVideo call: a URL, a
// file_id, or "" for an upload.
func (f *fakeTelegram) sendVideos() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.method == "sendVideo" {
			out = append(out, c.fields["video"])
		}
	}
	return out
}

func TestChatSendsDirectURL(t *testing.T) {
	t.Parallel()

	p := &linkProvider{}
	b, tg := newServerBot(t, p)
	b.handleDefault(context.Background(), b.api, chatMessage(1, 5, "link://a"))

	// Telegram is handed the URL; nothing is downloaded or uploaded.
	if got := tg.sendVideos(); len(got) != 1 || got[0] != directURL {
		t.Fatalf("sendVideo calls = %q, want one with the direct URL", got)
	}
	if p.downloads != 0 {
		t.Errorf("downloads = %d, want 0", p.downloads)
	}

	// The same link again is sent by file_id, without even a lookup.
	b.handleDefault(context.Background(), b.api, chatMessage(2, 5, "link://a"))
	if got := tg.sendVideos(); len(got) != 2 || got[1] != fakeFileID {
		t.Errorf("second sendVideo = %q, want the cached file_id", got)
	}
	if p.lookups != 1 || p.downloads != 0 {
		t.Errorf("lookups = %d, downloads = %d; want 1 and 0", p.lookups, p.downloads)
	}
}

func TestChatFallsBackToUpload(t *testing.T) {
	t.Parallel()

	t.Run("telegram can't fetch the URL", func(t *testing.T) {
		t.Parallel()
		p := &linkProvider{}
		b, tg := newServerBot(t, p)
		tg.rejectURLs = true

		b.handleDefault(context.Background(), b.api, chatMessage(1, 5, "link://a"))
		if got := tg.sendVideos(); len(got) != 2 || got[0] != directURL || got[1] != "" {
			t.Errorf("sendVideo calls = %q, want the URL, then an upload", got)
		}
		if p.downloads != 1 {
			t.Errorf("downloads = %d, want 1", p.downloads)
		}
	})

	t.Run("no direct URL", func(t *testing.T) {
		t.Parallel()
		p := &linkProvider{lookupErr: errors.New("needs a merge")}
		b, tg := newServerBot(t, p)

		b.handleDefault(context.Background(), b.api, chatMessage(1, 5, "link://a"))
		if got := tg.sendVideos(); len(got) != 1 || got[0] != "" {
			t.Errorf("sendVideo calls = %q, want one upload", got)
		}
		if p.downloads != 1 {
			t.Errorf("downloads = %d, want 1", p.downloads)
		}
	})
}

func TestInlineUsesDirectURL(t *testing.T) {
	t.Parallel()

	p := &linkProvider{}
	b, tg := newServerBot(t, p)
	b.handleChosenInline(context.Background(), b.api, chosen("link://a"))

	// Telegram fetches the URL into the storage chat; the placeholder then gets its file_id.
	if got := tg.sendVideos(); len(got) != 1 || got[0] != directURL {
		t.Fatalf("sendVideo calls = %q, want one with the direct URL", got)
	}
	if edit, ok := tg.call("editMessageMedia"); !ok || !strings.Contains(edit.fields["media"], fakeFileID) {
		t.Errorf("placeholder not replaced with the fetched file: %v", tg.methods())
	}
	if p.downloads != 0 {
		t.Errorf("downloads = %d, want 0", p.downloads)
	}
}
