package ytdlpsite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"anydloadbot/internal/downloader"
	"anydloadbot/internal/downloader/ytdlp"
)

func TestTikTokFindURL(t *testing.T) {
	t.Parallel()

	const video = "https://www.tiktok.com/@scout2015/video/6718335390845095173"
	tests := []struct {
		text string
		want string
		ok   bool
	}{
		{video, video, true},
		{video + "?is_from_webapp=1&sender_device=pc", video, true}, // tracking parameters dropped
		{"http://www.tiktok.com/@scout2015/video/6718335390845095173", video, true},
		{"look " + video + ".", video, true},
		{"https://m.tiktok.com/@scout2015/video/6718335390845095173", "https://m.tiktok.com/@scout2015/video/6718335390845095173", true},
		{"https://vm.tiktok.com/ZMhAbCdEf/", "https://vm.tiktok.com/ZMhAbCdEf/", true},
		{"https://vt.tiktok.com/ZSabc123/", "https://vt.tiktok.com/ZSabc123/", true},
		{"https://www.tiktok.com/t/ZTRabc123/", "https://www.tiktok.com/t/ZTRabc123/", true},
		{"https://www.tiktok.com/@scout2015", "", false},
		{"https://www.tiktok.com/@scout2015/video/abc", "", false},
		{"https://www.tiktok.com/tag/funny", "", false},
		{"https://nottiktok.com/@u/video/6718335390845095173", "", false},
		{"https://tiktok.com.evil.io/@u/video/6718335390845095173", "", false},
		{"https://example.com/?u=https://www.tiktok.com/@u/video/6718335390845095173", "", false},
		{"no links", "", false},
	}
	p := New(nil, TikTok(), Options{})
	for _, tt := range tests {
		got, ok := p.FindURL(tt.text)
		if got != tt.want || ok != tt.ok {
			t.Errorf("FindURL(%q) = %q, %v; want %q, %v", tt.text, got, ok, tt.want, tt.ok)
		}
	}
}

func TestRedditFindURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text string
		want string
		ok   bool
	}{
		{"https://www.reddit.com/r/videos/comments/6rrwyj/that_small_heart_attack/", "https://www.reddit.com/r/videos/comments/6rrwyj/that_small_heart_attack/", true},
		{"https://www.reddit.com/r/videos/comments/6rrwyj/x/?utm_source=share&utm_medium=web2x", "https://www.reddit.com/r/videos/comments/6rrwyj/x/", true},
		{"https://old.reddit.com/r/videos/comments/6rrwyj/x/", "https://old.reddit.com/r/videos/comments/6rrwyj/x/", true},
		{"https://www.reddit.com/user/someone/comments/6rrwyj/x/", "https://www.reddit.com/user/someone/comments/6rrwyj/x/", true},
		{"https://www.reddit.com/r/videos/s/AbCdEf1234", "https://www.reddit.com/r/videos/s/AbCdEf1234", true}, // share link
		{"https://v.redd.it/zv89llsvexdz", "https://v.redd.it/zv89llsvexdz", true},
		{"https://redd.it/6rrwyj", "https://redd.it/6rrwyj", true},
		{"https://www.reddit.com/r/videos/", "", false},
		{"https://www.reddit.com/user/someone/", "", false},
		{"https://v.redd.it/", "", false},
		{"https://notreddit.com/r/videos/comments/6rrwyj/x/", "", false},
		{"https://reddit.com.evil.io/r/videos/comments/6rrwyj/x/", "", false},
		{"no links", "", false},
	}
	p := New(nil, Reddit(), Options{})
	for _, tt := range tests {
		got, ok := p.FindURL(tt.text)
		if got != tt.want || ok != tt.ok {
			t.Errorf("FindURL(%q) = %q, %v; want %q, %v", tt.text, got, ok, tt.want, tt.ok)
		}
	}
}

const link = "https://www.tiktok.com/@scout2015/video/6718335390845095173"

// fakeRun emulates yt-dlp: metadata for --dump-single-json, otherwise a file in -P.
type fakeRun struct {
	meta    string
	err     error
	calls   [][]string
	cookies []string // contents of the --cookies file at the time of each run
}

func (f *fakeRun) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if i := slices.Index(args, "--cookies"); i >= 0 {
		if data, err := os.ReadFile(args[i+1]); err == nil {
			f.cookies = append(f.cookies, string(data))
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	if slices.Contains(args, "--dump-single-json") {
		return []byte(f.meta), nil
	}
	path := filepath.Join(args[slices.Index(args, "-P")+1], "6718335390845095173.mp4")
	if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
		return nil, err
	}
	return []byte(path + "\n"), nil
}

func newTestProvider(f *fakeRun, opts Options) *Provider {
	return New(ytdlp.NewWithRunner(f.run, 1), TikTok(), opts)
}

func TestDownload(t *testing.T) {
	t.Parallel()

	f := &fakeRun{meta: `{"title":"Scramble up ur name","duration":10}`}
	cookies := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookies, []byte("cookie-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := newTestProvider(f, Options{Cookies: cookies}).Download(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != downloader.KindVideo || string(m.Data) != "video" {
		t.Errorf("unexpected media: %+v", m)
	}
	if m.Title != "" {
		t.Errorf("TikTok video has caption %q, want none", m.Title)
	}

	if len(f.calls) != 2 {
		t.Fatalf("yt-dlp ran %d times, want 2 (probe + download)", len(f.calls))
	}
	for i, args := range f.calls {
		// The link must come after "--" so it can never be parsed as an option.
		if args[len(args)-2] != "--" || args[len(args)-1] != link {
			t.Errorf("call %d: link not passed after --: %v", i, args)
		}
		if !slices.Contains(args, "--cookies") {
			t.Errorf("call %d: cookies not passed: %v", i, args)
		}
	}
	if len(f.cookies) != 2 || f.cookies[0] != "cookie-data" || f.cookies[1] != "cookie-data" {
		t.Errorf("yt-dlp did not receive the configured cookies: %q", f.cookies)
	}
}

func TestDownloadLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		meta      string
		wantErr   error
		wantCalls int
	}{
		{"unknown duration is allowed", `{"title":"x"}`, nil, 2},
		{"at the limit", `{"duration":900}`, nil, 2},
		{"too long never starts the download", `{"duration":900.5}`, downloader.ErrTooLong, 1},
		{"live stream", `{"is_live":true}`, downloader.ErrNoMedia, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := &fakeRun{meta: tt.meta}
			_, err := newTestProvider(f, Options{}).Download(context.Background(), link)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if len(f.calls) != tt.wantCalls {
				t.Errorf("yt-dlp ran %d times, want %d", len(f.calls), tt.wantCalls)
			}
		})
	}
}

func TestDownloadRejectsForeignLinks(t *testing.T) {
	t.Parallel()

	f := &fakeRun{}
	if _, err := newTestProvider(f, Options{}).Download(context.Background(), "https://example.com/@u/video/6718335390845095173"); err == nil {
		t.Error("Download() accepted a foreign link")
	}
	if len(f.calls) != 0 {
		t.Error("yt-dlp must not run for foreign links")
	}
}

func TestDescription(t *testing.T) {
	t.Parallel()

	p := newTestProvider(&fakeRun{}, Options{MaxDuration: 5 * time.Minute})
	if got := p.Description(); got != "videos (up to 5 min)" {
		t.Errorf("Description() = %q", got)
	}
}

func TestCaptionKeptForSitesThatWantIt(t *testing.T) {
	t.Parallel()

	f := &fakeRun{meta: `{"title":"That small heart attack","duration":30}`}
	p := New(ytdlp.NewWithRunner(f.run, 1), Reddit(), Options{})
	m, err := p.Download(context.Background(), "https://www.reddit.com/r/videos/comments/6rrwyj/that_small_heart_attack/")
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "That small heart attack" {
		t.Errorf("Reddit title = %q, want the post title as caption", m.Title)
	}
}
