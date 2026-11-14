package youtube

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

	const want = "https://www.youtube.com/watch?v=RkW7kOKUcko"
	tests := []struct {
		text string
		want string
		ok   bool
	}{
		{"https://www.youtube.com/watch?v=RkW7kOKUcko", want, true},
		{"watch this https://www.youtube.com/watch?v=RkW7kOKUcko&list=PL123&t=30s.", want, true},
		{"https://youtu.be/RkW7kOKUcko?si=abc", want, true},
		{"https://m.youtube.com/watch?v=RkW7kOKUcko", want, true},
		{"https://www.youtube.com/shorts/RkW7kOKUcko", want, true},
		{"https://music.youtube.com/watch?v=RkW7kOKUcko", want, true},
		{"https://www.youtube.com/playlist?list=PL123", "", false},
		{"https://www.youtube.com/watch?v=short", "", false},
		{"https://evilyoutube.com/watch?v=RkW7kOKUcko", "", false},
		{"https://youtube.com.evil.io/watch?v=RkW7kOKUcko", "", false},
		{"no links", "", false},
	}
	for _, tt := range tests {
		got, ok := (&Provider{}).FindURL(tt.text)
		if got != tt.want || ok != tt.ok {
			t.Errorf("FindURL(%q) = %q, %v; want %q, %v", tt.text, got, ok, tt.want, tt.ok)
		}
	}
}

func TestPreview(t *testing.T) {
	t.Parallel()

	const thumb = "https://i.ytimg.com/vi/RkW7kOKUcko/hqdefault.jpg"
	ctx := context.Background()

	t.Run("title and channel from oEmbed", func(t *testing.T) {
		t.Parallel()

		var gotURL string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotURL = r.URL.Query().Get("url")
			_, _ = w.Write([]byte(`{"title":"Don't Look Back","author_name":"eRtRko"}`))
		}))
		t.Cleanup(srv.Close)

		p := &Provider{fetch: downloader.NewFetcher(func(string) bool { return true }), oembedBase: srv.URL}
		got, ok := p.Preview(ctx, link)
		if !ok || got.Title != "Don't Look Back" || got.Description != "eRtRko · Tap to download" || got.ThumbnailURL != thumb {
			t.Errorf("Preview() = %+v, %v", got, ok)
		}
		if gotURL != link {
			t.Errorf("oEmbed asked about %q, want %q", gotURL, link)
		}
	})

	t.Run("falls back when oEmbed fails", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.NotFoundHandler())
		t.Cleanup(srv.Close)

		p := &Provider{fetch: downloader.NewFetcher(func(string) bool { return true }), oembedBase: srv.URL}
		got, ok := p.Preview(ctx, link)
		if !ok || got.Title != "YouTube video" || got.ThumbnailURL != thumb {
			t.Errorf("Preview() = %+v, %v; want generic fallback", got, ok)
		}
	})

	t.Run("works without a fetcher", func(t *testing.T) {
		t.Parallel()

		if got, ok := (&Provider{}).Preview(ctx, link); !ok || got.Title != "YouTube video" {
			t.Errorf("Preview() = %+v, %v", got, ok)
		}
	})

	t.Run("rejects non-YouTube links", func(t *testing.T) {
		t.Parallel()

		if _, ok := (&Provider{}).Preview(ctx, "https://example.com/x"); ok {
			t.Error("Preview() accepted a non-YouTube link")
		}
	})
}

// fakeRun emulates yt-dlp: metadata for --dump-single-json, otherwise writes
// a file into the -P directory and prints its path. It records every call.
type fakeRun struct {
	meta      string
	fileBytes int  // size of the file the "download" produces
	noFile    bool // simulate --max-filesize rejecting everything
	err       error
	calls     [][]string
}

func (f *fakeRun) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return nil, f.err
	}
	if slices.Contains(args, "--dump-single-json") {
		return []byte(f.meta), nil
	}
	if f.noFile {
		return []byte("\n"), nil
	}
	dir := args[slices.Index(args, "-P")+1]
	ext := ".mp4"
	if slices.Contains(args, "-x") {
		ext = ".mp3"
	}
	path := filepath.Join(dir, "RkW7kOKUcko"+ext)
	if err := os.WriteFile(path, make([]byte, f.fileBytes), 0o600); err != nil {
		return nil, err
	}
	return []byte(path + "\n"), nil
}

func newTestProvider(f *fakeRun) *Provider {
	return New(ytdlp.NewWithRunner(f.run, 1), Options{MaxDuration: 15 * time.Minute})
}

const link = "https://www.youtube.com/watch?v=RkW7kOKUcko"

func TestDownload(t *testing.T) {
	t.Parallel()

	f := &fakeRun{meta: `{"id":"RkW7kOKUcko","title":"Hello","duration":212.4}`, fileBytes: 1234}
	m, err := newTestProvider(f).Download(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if m.Filename != "RkW7kOKUcko.mp4" || len(m.Data) != 1234 || m.Title != "Hello" {
		t.Errorf("unexpected media: %q, %d bytes, %q", m.Filename, len(m.Data), m.Title)
	}
	if len(f.calls) != 2 {
		t.Fatalf("yt-dlp ran %d times, want 2 (probe + download)", len(f.calls))
	}
	// The link must come after "--" so it can never be parsed as an option.
	last := f.calls[1]
	if last[len(last)-2] != "--" || last[len(last)-1] != link {
		t.Errorf("link not passed after --: %v", last)
	}
}

func TestDownloadLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		fake      *fakeRun
		wantErr   error
		wantCalls int
	}{
		{
			name:      "exactly at the limit is allowed",
			fake:      &fakeRun{meta: `{"duration":900}`, fileBytes: 1},
			wantCalls: 2,
		},
		{
			name:      "too long never starts the download",
			fake:      &fakeRun{meta: `{"duration":900.5}`},
			wantErr:   downloader.ErrTooLong,
			wantCalls: 1,
		},
		{
			name:      "live stream",
			fake:      &fakeRun{meta: `{"duration":0,"is_live":true}`},
			wantErr:   downloader.ErrNoMedia,
			wantCalls: 1,
		},
		{
			name:      "rejected by max-filesize",
			fake:      &fakeRun{meta: `{"duration":60}`, noFile: true},
			wantErr:   downloader.ErrTooLarge,
			wantCalls: 2,
		},
		{
			name:      "unavailable video",
			fake:      &fakeRun{err: errors.New("yt-dlp: exit status 1: ERROR: [youtube] x: Video unavailable")},
			wantErr:   downloader.ErrNoMedia,
			wantCalls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := newTestProvider(tt.fake).Download(context.Background(), link)
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
	if _, err := newTestProvider(f).Download(context.Background(), "https://example.com/watch?v=RkW7kOKUcko"); err == nil {
		t.Error("Download() accepted a non-YouTube link")
	}
	if len(f.calls) != 0 {
		t.Error("yt-dlp must not run for foreign links")
	}
}

func TestDownloadRespectsContextWhileQueued(t *testing.T) {
	t.Parallel()

	p := newTestProvider(&fakeRun{})
	release, err := p.yt.Acquire(context.Background()) // all slots busy
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

func TestDownloadAudio(t *testing.T) {
	t.Parallel()

	f := &fakeRun{meta: `{"id":"RkW7kOKUcko","title":"Don't Look Back","uploader":"eRtRko","duration":433.2}`, fileBytes: 999}
	m, err := newTestProvider(f).DownloadAudio(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != downloader.KindAudio || m.Filename != "RkW7kOKUcko.mp3" || len(m.Data) != 999 {
		t.Errorf("unexpected media: kind=%v %q %d bytes", m.Kind, m.Filename, len(m.Data))
	}
	if m.Title != "Don't Look Back" || m.Performer != "eRtRko" || m.Duration != 433 {
		t.Errorf("tags: title=%q performer=%q duration=%d", m.Title, m.Performer, m.Duration)
	}

	// yt-dlp must be asked to extract MP3 and embed the metadata and cover art.
	args := f.calls[1]
	for _, want := range []string{"-x", "--embed-metadata", "--embed-thumbnail"} {
		if !slices.Contains(args, want) {
			t.Errorf("yt-dlp args missing %s: %v", want, args)
		}
	}
	if i := slices.Index(args, "--audio-format"); i < 0 || args[i+1] != "mp3" {
		t.Errorf("yt-dlp not asked for mp3: %v", args)
	}
}

func TestDownloadAudioHonoursDurationLimit(t *testing.T) {
	t.Parallel()

	f := &fakeRun{meta: `{"duration":901}`}
	if _, err := newTestProvider(f).DownloadAudio(context.Background(), link); !errors.Is(err, downloader.ErrTooLong) {
		t.Errorf("err = %v, want ErrTooLong", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("yt-dlp ran %d times, want 1 (probe only)", len(f.calls))
	}
}
