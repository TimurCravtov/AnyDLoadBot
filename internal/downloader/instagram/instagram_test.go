package instagram

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"anydloadbot/internal/downloader"
	"anydloadbot/internal/downloader/ytdlp"
)

func TestFindURL(t *testing.T) {
	t.Parallel()

	const want = "https://www.instagram.com/p/Dfm3F4vB-03/"
	tests := []struct {
		text string
		want string
		ok   bool
	}{
		{"https://www.instagram.com/reels/Dfm3F4vB-03/", want, true}, // the /reels/ spelling
		{"https://www.instagram.com/reel/Dfm3F4vB-03/?igsh=abc", want, true},
		{"https://www.instagram.com/p/Dfm3F4vB-03/", want, true},
		{"https://instagram.com/tv/Dfm3F4vB-03", want, true},
		{"https://www.instagram.com/thomyorke/reel/Dfm3F4vB-03/", want, true},
		{"look: https://www.instagram.com/p/Dfm3F4vB-03/.", want, true},
		{"https://www.instagram.com/thomyorke/", "", false},
		{"https://www.instagram.com/stories/thomyorke/123456789/", "", false},
		{"https://evilinstagram.com/p/Dfm3F4vB-03/", "", false},
		{"https://instagram.com.evil.io/p/Dfm3F4vB-03/", "", false},
		{"no links", "", false},
	}
	for _, tt := range tests {
		got, ok := (&Provider{}).FindURL(tt.text)
		if got != tt.want || ok != tt.ok {
			t.Errorf("FindURL(%q) = %q, %v; want %q, %v", tt.text, got, ok, tt.want, tt.ok)
		}
	}
}

const link = "https://www.instagram.com/p/Dfm3F4vB-03/"

// fakeYtDlp writes a file into the -P directory like a successful download, or
// fails with err.
type fakeYtDlp struct {
	err   error
	calls [][]string
}

func (f *fakeYtDlp) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return nil, f.err
	}
	path := filepath.Join(args[slices.Index(args, "-P")+1], "Dfm3F4vB-03.mp4")
	if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
		return nil, err
	}
	return []byte(path + "\n"), nil
}

func newTestProvider(f *fakeYtDlp, mediaBase string) *Provider {
	p := New(ytdlp.NewWithRunner(f.run, 1), Options{Cookies: "cookies.txt"})
	p.fetch = downloader.NewFetcher(func(string) bool { return true })
	p.siteBase = mediaBase
	return p
}

func TestDownloadVideo(t *testing.T) {
	t.Parallel()

	f := &fakeYtDlp{}
	m, err := newTestProvider(f, "").Download(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != downloader.KindVideo || string(m.Data) != "video" {
		t.Errorf("unexpected media: %+v", m)
	}

	args := f.calls[0]
	if i := slices.Index(args, "--cookies"); i < 0 || args[i+1] != "cookies.txt" {
		t.Errorf("cookies not passed to yt-dlp: %v", args)
	}
	if i := slices.Index(args, "-f"); i < 0 || args[i+1] != formatSelector {
		t.Errorf("unexpected format selector: %v", args)
	}
}

func TestDownloadImageFallback(t *testing.T) {
	t.Parallel()

	// A JPEG header is enough for http.DetectContentType.
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/p/Dfm3F4vB-03/media/" || r.URL.Query().Get("size") != "l" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(jpeg)
	}))
	t.Cleanup(srv.Close)

	f := &fakeYtDlp{err: errors.New("yt-dlp: exit status 1: ERROR: [Instagram] X: There is no video in this post")}
	m, err := newTestProvider(f, srv.URL).Download(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != downloader.KindPhoto || m.Filename != "Dfm3F4vB-03.jpg" || len(m.Data) != len(jpeg) {
		t.Errorf("unexpected media: kind=%v %q %d bytes", m.Kind, m.Filename, len(m.Data))
	}
}

func TestDownloadImageFallbackRejectsNonImages(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>Login • Instagram</html>")) // what a login wall looks like
	}))
	t.Cleanup(srv.Close)

	f := &fakeYtDlp{err: errors.New("There is no video in this post")}
	if _, err := newTestProvider(f, srv.URL).Download(context.Background(), link); !errors.Is(err, downloader.ErrNoMedia) {
		t.Errorf("err = %v, want ErrNoMedia", err)
	}
}

func TestDownloadOtherErrorsDoNotFallBack(t *testing.T) {
	t.Parallel()

	// No server is running: reaching the image fallback would fail differently.
	f := &fakeYtDlp{err: errors.New("yt-dlp: exit status 1: ERROR: unable to download webpage: timed out")}
	_, err := newTestProvider(f, "http://127.0.0.1:1").Download(context.Background(), link)
	if err == nil || errors.Is(err, downloader.ErrNoMedia) {
		t.Errorf("err = %v, want the raw yt-dlp error", err)
	}
}

func TestPreview(t *testing.T) {
	t.Parallel()

	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("img")) }))
	t.Cleanup(cdn.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/photo.jpg", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	got, ok := newTestProvider(&fakeYtDlp{}, srv.URL).Preview(context.Background(), link)
	if !ok || got.ThumbnailURL != cdn.URL+"/photo.jpg" || got.Title != "Instagram post" {
		t.Errorf("Preview() = %+v, %v", got, ok)
	}

	// No thumbnail, no placeholder.
	dead := newTestProvider(&fakeYtDlp{}, "http://127.0.0.1:1")
	if _, ok := dead.Preview(context.Background(), link); ok {
		t.Error("Preview() succeeded without a reachable thumbnail")
	}
}
