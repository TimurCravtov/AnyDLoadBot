package spotify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"anydloadbot/internal/downloader"
	"anydloadbot/internal/downloader/ytdlp"
)

func TestFindURL(t *testing.T) {
	t.Parallel()

	const want = "https://open.spotify.com/track/4cOdK2wGLETKBW3PvgPWqT"
	tests := []struct {
		text string
		want string
		ok   bool
	}{
		{"https://open.spotify.com/track/4cOdK2wGLETKBW3PvgPWqT", want, true},
		{"https://open.spotify.com/track/4cOdK2wGLETKBW3PvgPWqT?si=abc123", want, true},
		{"https://open.spotify.com/intl-de/track/4cOdK2wGLETKBW3PvgPWqT", want, true},
		{"https://open.spotify.com/embed/track/4cOdK2wGLETKBW3PvgPWqT", want, true},
		{"https://play.spotify.com/track/4cOdK2wGLETKBW3PvgPWqT", want, true},
		{"listen: https://open.spotify.com/track/4cOdK2wGLETKBW3PvgPWqT.", want, true},
		{"https://open.spotify.com/album/4cOdK2wGLETKBW3PvgPWqT", "", false},
		{"https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M", "", false},
		{"https://open.spotify.com/track/short", "", false},
		{"https://evilspotify.com/track/4cOdK2wGLETKBW3PvgPWqT", "", false},
		{"https://open.spotify.com.evil.io/track/4cOdK2wGLETKBW3PvgPWqT", "", false},
		{"no links", "", false},
	}
	for _, tt := range tests {
		got, ok := (&Provider{}).FindURL(tt.text)
		if got != tt.want || ok != tt.ok {
			t.Errorf("FindURL(%q) = %q, %v; want %q, %v", tt.text, got, ok, tt.want, tt.ok)
		}
	}
}

const (
	link      = "https://open.spotify.com/track/4cOdK2wGLETKBW3PvgPWqT"
	embedHTML = `<!DOCTYPE html><html><body><script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{"state":{"data":{"entity":{` +
		`"title":"Never Gonna Give You Up","duration":213573,` +
		`"artists":[{"name":"Rick Astley"}],` +
		`"visualIdentity":{"image":[` +
		`{"url":"https://image-cdn.spotifycdn.com/small.jpg","maxWidth":64},` +
		`{"url":"https://image-cdn.spotifycdn.com/large.jpg","maxWidth":640},` +
		`{"url":"https://image-cdn.spotifycdn.com/med.jpg","maxWidth":300}]}` +
		`}}}}}}</script></body></html>`
	notFoundHTML = `<!DOCTYPE html><html><body><script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{"status":404,"title":"Page not found"}}}</script></body></html>`
)

type fakeRun struct {
	fileBytes int
	noFile    bool
	err       error
	calls     [][]string
}

func (f *fakeRun) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return nil, f.err
	}
	if f.noFile {
		return []byte("\n"), nil
	}
	dir := args[slices.Index(args, "-P")+1]
	path := filepath.Join(dir, "dQw4w9WgXcQ.mp3")
	if err := os.WriteFile(path, make([]byte, f.fileBytes), 0o600); err != nil {
		return nil, err
	}
	return []byte(path + "\n"), nil
}

func newTestProvider(t *testing.T, f *fakeRun, page string) *Provider {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed/track/4cOdK2wGLETKBW3PvgPWqT" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(srv.Close)

	p := New(ytdlp.NewWithRunner(f.run, 1), Options{MaxDuration: 15 * time.Minute})
	p.fetch = downloader.NewFetcher(func(string) bool { return true })
	p.siteBase = srv.URL
	return p
}

func TestPreview(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("title, artist and largest cover from embed", func(t *testing.T) {
		t.Parallel()

		p := newTestProvider(t, &fakeRun{}, embedHTML)
		got, ok := p.Preview(ctx, link)
		if !ok || got.Title != "Never Gonna Give You Up" ||
			got.Description != "Rick Astley · Tap to download" ||
			got.ThumbnailURL != "https://image-cdn.spotifycdn.com/large.jpg" {
			t.Errorf("Preview() = %+v, %v", got, ok)
		}
	})

	t.Run("fails when track is missing", func(t *testing.T) {
		t.Parallel()

		p := newTestProvider(t, &fakeRun{}, notFoundHTML)
		if _, ok := p.Preview(ctx, link); ok {
			t.Error("Preview() succeeded for a 404 embed page")
		}
	})

	t.Run("rejects non-Spotify links", func(t *testing.T) {
		t.Parallel()

		if _, ok := (&Provider{}).Preview(ctx, "https://example.com/track/4cOdK2wGLETKBW3PvgPWqT"); ok {
			t.Error("Preview() accepted a non-Spotify link")
		}
	})
}

func TestDownload(t *testing.T) {
	t.Parallel()

	f := &fakeRun{fileBytes: 1234}
	m, err := newTestProvider(t, f, embedHTML).Download(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != downloader.KindAudio || m.Filename != "dQw4w9WgXcQ.mp3" || len(m.Data) != 1234 {
		t.Errorf("unexpected media: kind=%v %q %d bytes", m.Kind, m.Filename, len(m.Data))
	}
	if m.Title != "Never Gonna Give You Up" || m.Performer != "Rick Astley" || m.Duration != 213 {
		t.Errorf("tags: title=%q performer=%q duration=%d", m.Title, m.Performer, m.Duration)
	}
	if len(f.calls) != 1 {
		t.Fatalf("yt-dlp ran %d times, want 1", len(f.calls))
	}

	args := f.calls[0]
	for _, want := range []string{"-x", "--embed-metadata", "--embed-thumbnail"} {
		if !slices.Contains(args, want) {
			t.Errorf("yt-dlp args missing %s: %v", want, args)
		}
	}
	const wantSearch = "ytsearch1:Rick Astley - Never Gonna Give You Up"
	if args[len(args)-2] != "--" || args[len(args)-1] != wantSearch {
		t.Errorf("search query not passed after --: %v", args)
	}
}

func TestDownloadLimitsAndErrors(t *testing.T) {
	t.Parallel()

	pageWithDuration := func(ms string) string {
		return `<script id="__NEXT_DATA__">{"props":{"pageProps":{"state":{"data":{"entity":{"title":"Song","duration":` + ms + `}}}}}}</script>`
	}

	tests := []struct {
		name      string
		page      string
		fake      *fakeRun
		wantErr   error
		wantCalls int
	}{
		{
			name:      "exactly at the limit is allowed",
			page:      pageWithDuration("900000"),
			fake:      &fakeRun{fileBytes: 1},
			wantCalls: 1,
		},
		{
			name:      "too long never starts yt-dlp",
			page:      pageWithDuration("900500"),
			fake:      &fakeRun{},
			wantErr:   downloader.ErrTooLong,
			wantCalls: 0,
		},
		{
			name:      "missing track",
			page:      notFoundHTML,
			fake:      &fakeRun{},
			wantErr:   downloader.ErrNoMedia,
			wantCalls: 0,
		},
		{
			name:      "rejected by max-filesize",
			page:      embedHTML,
			fake:      &fakeRun{noFile: true},
			wantErr:   downloader.ErrTooLarge,
			wantCalls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := newTestProvider(t, tt.fake, tt.page).Download(context.Background(), link)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if len(tt.fake.calls) != tt.wantCalls {
				t.Errorf("yt-dlp ran %d times, want %d", len(tt.fake.calls), tt.wantCalls)
			}
		})
	}
}

func TestDownloadRejectsForeignLinks(t *testing.T) {
	t.Parallel()

	f := &fakeRun{}
	if _, err := newTestProvider(t, f, embedHTML).Download(context.Background(), "https://example.com/track/4cOdK2wGLETKBW3PvgPWqT"); err == nil {
		t.Error("Download() accepted a non-Spotify link")
	}
	if len(f.calls) != 0 {
		t.Error("yt-dlp must not run for foreign links")
	}
}

func TestDownloadRespectsContextWhileQueued(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t, &fakeRun{}, embedHTML)
	release, err := p.yt.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Download(ctx, link); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
