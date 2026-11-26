package ytdlpsite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"anydloadbot/internal/downloader"
	"anydloadbot/internal/downloader/ytdlp"
	"anydloadbot/internal/downloader/ytdlp/ytdlptest"
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
	delay   time.Duration // how long each run takes
	mu      sync.Mutex
	calls   [][]string
	cookies []string // contents of the --cookies file at the time of each run
}

func (f *fakeRun) runs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRun) run(ctx context.Context, args ...string) ([]byte, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
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
	out, pass := ytdlptest.Download(args, f.meta)
	if !pass {
		return out, nil
	}
	ext := ".mp4"
	if slices.Contains(args, "-x") {
		ext = ".mp3"
	}
	path := filepath.Join(args[slices.Index(args, "-P")+1], "6718335390845095173"+ext)
	if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
		return nil, err
	}
	return append(out, path+"\n"...), nil
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
	if m.Title != "Scramble up ur name" {
		t.Errorf("title = %q, want the video's title (the chat caption)", m.Title)
	}

	if len(f.calls) != 1 {
		t.Fatalf("yt-dlp ran %d times, want 1 (checks happen inside the download)", len(f.calls))
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
	if len(f.cookies) != 1 || f.cookies[0] != "cookie-data" {
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
		{"unknown duration is allowed", `{"title":"x"}`, nil, 1},
		{"at the limit", `{"duration":900}`, nil, 1},
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
	if got := p.Description(); got != `videos (up to 5 min); add "mp3" after the link for the sound` {
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

func TestInlinePreview(t *testing.T) {
	t.Parallel()

	tiktok, ok := WithInline(New(nil, TikTok(), Options{})).(downloader.Previewer)
	if !ok {
		t.Fatal("TikTok is not inline-capable")
	}
	reddit, ok := WithInline(New(nil, Reddit(), Options{})).(downloader.Previewer)
	if !ok {
		t.Fatal("Reddit is not inline-capable")
	}

	tests := []struct {
		p           downloader.Previewer
		link        string
		title, desc string
	}{
		{tiktok, "https://www.tiktok.com/@kotlyarenko.narezki/video/7701025806121717024", "TikTok video", "@kotlyarenko.narezki · Tap to download"},
		{tiktok, "https://vm.tiktok.com/ZMhAbCdEf/", "TikTok video", "Tap to download"},
		{reddit, "https://www.reddit.com/r/videos/comments/6rrwyj/x/", "Reddit video", "r/videos · Tap to download"},
		{reddit, "https://v.redd.it/zv89llsvexdz", "Reddit video", "Tap to download"},
	}
	for _, tt := range tests {
		got, ok := tt.p.Preview(context.Background(), tt.link)
		if !ok || got.Title != tt.title || got.Description != tt.desc {
			t.Errorf("Preview(%q) = %+v, %v; want %q / %q", tt.link, got, ok, tt.title, tt.desc)
		}
	}

	if _, ok := tiktok.Preview(context.Background(), "https://example.com/@u/video/1"); ok {
		t.Error("Preview accepted a foreign link")
	}
}

func TestWithInlineWithoutPreviewIsUnchanged(t *testing.T) {
	t.Parallel()

	site := TikTok()
	site.Preview = nil
	p := New(nil, site, Options{})
	if got := WithInline(p); got != downloader.Provider(p) {
		t.Errorf("WithInline changed a site without a preview: %T", got)
	}
	if _, ok := WithInline(p).(downloader.Previewer); ok {
		t.Error("a site without a preview claims inline support")
	}
}

func TestDownloadAudio(t *testing.T) {
	t.Parallel()

	f := &fakeRun{meta: `{"title":"Scramble up ur name","uploader":"scout2015","duration":10}`}
	m, err := newTestProvider(f, Options{}).DownloadAudio(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != downloader.KindAudio || m.Filename != "6718335390845095173.mp3" {
		t.Errorf("unexpected media: kind=%v %q", m.Kind, m.Filename)
	}
	if m.Title != "Scramble up ur name" || m.Performer != "scout2015" || m.Duration != 10 {
		t.Errorf("tags: %q / %q / %d", m.Title, m.Performer, m.Duration)
	}
	if args := f.calls[0]; !slices.Contains(args, "-x") || !slices.Contains(args, "--embed-thumbnail") {
		t.Errorf("yt-dlp not asked for an MP3 with cover: %v", args)
	}

	// The same duration limit as for video applies.
	long := &fakeRun{meta: `{"duration":901}`}
	if _, err := newTestProvider(long, Options{}).DownloadAudio(context.Background(), link); !errors.Is(err, downloader.ErrTooLong) {
		t.Errorf("err = %v, want ErrTooLong", err)
	}
}

func TestRichInlinePreview(t *testing.T) {
	t.Parallel()

	const tiktokLink = "https://www.tiktok.com/@scout2015/video/6718335390845095173"
	long := strings.Repeat("words ", 40)

	f := &fakeRun{meta: `{"title":"` + long + `","thumbnail":"https://p16.tiktokcdn.com/cover.jpeg","uploader":"scout2015"}`}
	p := WithInline(New(ytdlp.NewWithRunner(f.run, 1), TikTok(), Options{})).(downloader.Previewer)

	got, ok := p.Preview(context.Background(), tiktokLink)
	if !ok {
		t.Fatal("no preview")
	}
	if got.ThumbnailURL != "https://p16.tiktokcdn.com/cover.jpeg" {
		t.Errorf("thumbnail = %q, want the cover", got.ThumbnailURL)
	}
	if !strings.HasPrefix(got.Title, "words words") || len([]rune(got.Title)) > maxPreviewTitle {
		t.Errorf("title = %q, want the real title, at most %d characters", got.Title, maxPreviewTitle)
	}
	if got.Description != "@scout2015 · Tap to download" {
		t.Errorf("description = %q", got.Description)
	}

	// When the lookup fails, the URL-based row is still offered.
	failing := &fakeRun{err: errors.New("yt-dlp: exit status 1: ERROR: blocked")}
	fp := WithInline(New(ytdlp.NewWithRunner(failing.run, 1), TikTok(), Options{})).(downloader.Previewer)
	got, ok = fp.Preview(context.Background(), tiktokLink)
	if !ok || got.Title != "TikTok video" || got.ThumbnailURL != "" {
		t.Errorf("fallback preview = %+v, %v", got, ok)
	}
}

func TestPreviewDoesNotWaitForBusySlots(t *testing.T) {
	t.Parallel()

	f := &fakeRun{meta: `{"title":"should not be looked up"}`}
	prov := New(ytdlp.NewWithRunner(f.run, 1), TikTok(), Options{})
	for range previewSlots {
		prov.previews <- struct{}{} // every slot taken
	}
	got, ok := WithInline(prov).(downloader.Previewer).Preview(context.Background(), "https://www.tiktok.com/@a/video/6718335390845095173")
	if !ok || got.Title != "TikTok video" || len(f.calls) != 0 {
		t.Errorf("preview with busy slots = %+v, %v, %d yt-dlp runs; want the instant URL-based row", got, ok, len(f.calls))
	}
}

func TestSlowPreviewIsCachedForTheNextQuery(t *testing.T) {
	t.Parallel()

	const tiktokLink = "https://www.tiktok.com/@scout2015/video/6718335390845095173"
	f := &fakeRun{meta: `{"title":"Real title","thumbnail":"https://p16.tiktokcdn.com/cover.jpeg"}`, delay: 300 * time.Millisecond}
	prov := New(ytdlp.NewWithRunner(f.run, 1), TikTok(), Options{})
	prov.previewWait = 30 * time.Millisecond // much shorter than the lookup
	p := WithInline(prov).(downloader.Previewer)

	// The first query can't wait for the lookup: it gets the URL-based row now.
	first, _ := p.Preview(context.Background(), tiktokLink)
	if first.Title != "TikTok video" || first.ThumbnailURL != "" {
		t.Fatalf("first preview = %+v, want the instant URL-based row", first)
	}

	// The lookup finishes in the background...
	deadline := time.Now().Add(5 * time.Second)
	for f.runs() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let it store the result

	// ...so the next query gets the title and cover straight from the cache.
	second, _ := p.Preview(context.Background(), tiktokLink)
	if second.Title != "Real title" || second.ThumbnailURL != "https://p16.tiktokcdn.com/cover.jpeg" {
		t.Errorf("second preview = %+v, want the cached title and cover", second)
	}
	if n := f.runs(); n != 1 {
		t.Errorf("yt-dlp ran %d times, want 1 (the cache must be used)", n)
	}
}

func TestConcurrentPreviewsShareOneLookup(t *testing.T) {
	t.Parallel()

	const tiktokLink = "https://www.tiktok.com/@scout2015/video/6718335390845095173"
	f := &fakeRun{meta: `{"title":"Shared"}`, delay: 100 * time.Millisecond}
	p := WithInline(New(ytdlp.NewWithRunner(f.run, 1), TikTok(), Options{})).(downloader.Previewer)

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, _ := p.Preview(context.Background(), tiktokLink); got.Title != "Shared" {
				t.Errorf("preview title = %q, want Shared", got.Title)
			}
		}()
	}
	wg.Wait()
	if n := f.runs(); n != 1 {
		t.Errorf("yt-dlp ran %d times for 5 identical queries, want 1", n)
	}
}
