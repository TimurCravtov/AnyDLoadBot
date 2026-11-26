package ytdlp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"anydloadbot/internal/downloader"
)

// recorder is a Runner that remembers its arguments and the contents of any
// cookies file it was given while it ran (the copy is deleted afterwards).
type recorder struct {
	mu      sync.Mutex
	calls   [][]string
	cookies []string // contents of each --cookies file at call time
	paths   []string // paths of those files
}

func (r *recorder) run(_ context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, slices.Clone(args))
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--cookies" {
			data, err := os.ReadFile(args[i+1])
			if err != nil {
				return nil, err
			}
			r.cookies = append(r.cookies, string(data))
			r.paths = append(r.paths, args[i+1])
		}
	}
	if slices.Contains(args, "--dump-single-json") {
		return []byte(`{"id":"x","duration":10}`), nil
	}
	dir := args[slices.Index(args, "-P")+1]
	path := filepath.Join(dir, "x.mp4")
	if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
		return nil, err
	}
	return []byte(path + "\n"), nil
}

func writeCookies(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOptionsArgs(t *testing.T) {
	t.Parallel()

	if got := (Options{}).args(); len(got) != 0 {
		t.Errorf("empty options produce args: %v", got)
	}
	got := Options{Cookies: "c.txt", Proxy: "socks5://h:1", POTServer: "http://pot:4416"}.args()
	want := []string{
		"--cookies", "c.txt", "--proxy", "socks5://h:1",
		"--extractor-args", "youtubepot-bgutilhttp:base_url=http://pot:4416",
	}
	if !slices.Equal(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
}

func TestDefaultsAreUsedForProbeAndFetch(t *testing.T) {
	t.Parallel()

	const link = "https://example.com/v"
	cookies := writeCookies(t, "# Netscape HTTP Cookie File\n.youtube.com\tTRUE\t/\tTRUE\t0\tSID\tsecret\n")
	r := &recorder{}
	c := NewWithRunner(r.run, 1, Options{Cookies: cookies, Proxy: "http://proxy:3128"}.args()...)

	if _, err := c.Probe(context.Background(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(context.Background(), link, Request{Format: "b", Sort: "res"}); err != nil {
		t.Fatal(err)
	}

	if len(r.calls) != 2 {
		t.Fatalf("%d yt-dlp runs, want 2", len(r.calls))
	}
	for i, args := range r.calls {
		if j := slices.Index(args, "--proxy"); j < 0 || args[j+1] != "http://proxy:3128" {
			t.Errorf("run %d: proxy missing: %v", i, args)
		}
		// Defaults come before "--", so they can never be mistaken for the link.
		if sep := slices.Index(args, "--"); sep < slices.Index(args, "--cookies") || args[len(args)-1] != link {
			t.Errorf("run %d: bad argument order: %v", i, args)
		}
	}
	for i, content := range r.cookies {
		if content == "" || content[0] != '#' {
			t.Errorf("run %d: yt-dlp saw cookies %q, want the original contents", i, content)
		}
	}
}

func TestCookiesAreCopiedPerRun(t *testing.T) {
	t.Parallel()

	original := writeCookies(t, "original")
	r := &recorder{}
	c := NewWithRunner(r.run, 2, "--cookies", original)

	for i := 0; i < 2; i++ {
		if _, err := c.Probe(context.Background(), "https://example.com/v"); err != nil {
			t.Fatal(err)
		}
	}

	if len(r.paths) != 2 || r.paths[0] == original || r.paths[1] == original {
		t.Fatalf("yt-dlp was given the original cookies file: %v", r.paths)
	}
	if r.paths[0] == r.paths[1] {
		t.Error("two runs shared one cookies file")
	}
	for _, p := range r.paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("temporary cookies copy %s was not removed (err=%v)", p, err)
		}
	}
	if got, _ := os.ReadFile(original); string(got) != "original" {
		t.Errorf("original cookies file was modified: %q", got)
	}
}

func TestPerRequestCookiesOverrideDefaults(t *testing.T) {
	t.Parallel()

	global := writeCookies(t, "global")
	site := writeCookies(t, "site")
	r := &recorder{}
	c := NewWithRunner(r.run, 1, "--cookies", global)

	if _, err := c.Fetch(context.Background(), "https://example.com/v", Request{Format: "b", Sort: "res", Extra: []string{"--cookies", site}}); err != nil {
		t.Fatal(err)
	}

	// Both are passed, global first; yt-dlp uses the last --cookies, which is the site's.
	if len(r.cookies) != 2 || r.cookies[0] != "global" || r.cookies[1] != "site" {
		t.Errorf("cookies seen by yt-dlp (in order) = %q, want [global site]", r.cookies)
	}
}

func TestMissingCookiesFileFailsTheRun(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	c := NewWithRunner(r.run, 1, "--cookies", filepath.Join(t.TempDir(), "nope.txt"))
	if _, err := c.Probe(context.Background(), "https://example.com/v"); err == nil {
		t.Error("Probe() succeeded with a missing cookies file")
	}
	if len(r.calls) != 0 {
		t.Error("yt-dlp ran even though the cookies file could not be read")
	}
}

func TestNoDefaultsMeansNoExtraArgs(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	c := NewWithRunner(r.run, 1)
	if _, err := c.Probe(context.Background(), "https://example.com/v"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(r.calls[0], "--cookies") || slices.Contains(r.calls[0], "--proxy") {
		t.Errorf("unexpected arguments: %v", r.calls[0])
	}
}

func TestLinkIsNeverTreatedAsCookiesOption(t *testing.T) {
	t.Parallel()

	// A hostile "link" that looks like an option must stay after "--" untouched.
	r := &recorder{}
	c := NewWithRunner(r.run, 1)
	if _, err := c.Probe(context.Background(), "--cookies"); err != nil {
		t.Fatal(err)
	}
	args := r.calls[0]
	if args[len(args)-2] != "--" || args[len(args)-1] != "--cookies" || len(r.paths) != 0 {
		t.Errorf("link handled as an option: %v", args)
	}
}

func TestClassifySilentVideoIsNoMedia(t *testing.T) {
	t.Parallel()

	for _, msg := range []string{
		"yt-dlp: exit status 1: ERROR: Postprocessing: WARNING: unable to obtain file audio codec with ffprobe",
		"yt-dlp: exit status 1: ERROR: Postprocessing: Output file #0 does not contain any stream",
	} {
		if err := Classify(errors.New(msg)); !errors.Is(err, downloader.ErrNoMedia) {
			t.Errorf("Classify(%q) = %v, want ErrNoMedia", msg, err)
		}
	}
	if err := Classify(errors.New("yt-dlp: exit status 1: ERROR: network unreachable")); errors.Is(err, downloader.ErrNoMedia) {
		t.Error("an unrelated failure was classified as no media")
	}
}

func TestDirectURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, out string
		want      *Direct
	}{
		{name: "single file", out: "https\tmp4\t1234\thttps://cdn.example/v.mp4?sig=a\tb\n",
			want: &Direct{URL: "https://cdn.example/v.mp4?sig=a\tb", Ext: "mp4", Size: 1234}},
		{name: "size unknown", out: "https\tmp4\tNA\thttps://cdn.example/v.mp4\n",
			want: &Direct{URL: "https://cdn.example/v.mp4", Ext: "mp4"}},
		{name: "merged formats have no single url", out: "https+https\tmp4\tNA\tNA\n"},
		{name: "playlists are not files", out: "m3u8_native\tmp4\tNA\thttps://cdn.example/v.m3u8\n"},
		{name: "plain http", out: "http\tmp4\tNA\thttp://cdn.example/v.mp4\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var args []string
			c := NewWithRunner(func(_ context.Context, a ...string) ([]byte, error) {
				args = a
				return []byte(tt.out), nil
			}, 1, "--proxy", "socks5://warp:1080")

			got, err := c.DirectURL(context.Background(), "https://example.com/v", Request{Format: "0/b", Sort: "res:720"})
			if tt.want == nil {
				if err == nil {
					t.Fatalf("DirectURL() = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if *got != *tt.want {
				t.Errorf("DirectURL() = %+v, want %+v", got, tt.want)
			}
			// Nothing is downloaded, and the defaults (the proxy) still apply.
			if slices.Contains(args, "-P") || slices.Contains(args, "--no-simulate") || !slices.Contains(args, "--proxy") {
				t.Errorf("unexpected arguments: %v", args)
			}
			if i := slices.Index(args, "-f"); i < 0 || args[i+1] != "0/b" {
				t.Errorf("format not passed: %v", args)
			}
		})
	}
}

func TestFetchSingleRun(t *testing.T) {
	t.Parallel()

	const link = "https://example.com/v"
	dir := ""
	run := func(meta string, pass bool) Runner {
		return func(_ context.Context, args ...string) ([]byte, error) {
			out := "anydl-meta:" + meta + "\n"
			if !pass {
				return []byte(out), nil // the match filter refused it: no file
			}
			dir = args[slices.Index(args, "-P")+1]
			path := filepath.Join(dir, "x.mp4")
			if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
				return nil, err
			}
			return []byte(out + path + "\n"), nil
		}
	}
	req := Request{Format: "b", MaxDuration: 15 * time.Minute}

	// The metadata comes with the download.
	f, err := NewWithRunner(run(`{"id":"x","title":"Clip","uploader":"Me","duration":61}`, true), 1).Fetch(context.Background(), link, req)
	if err != nil || f.Info.Title != "Clip" || f.Info.Uploader != "Me" || f.Info.Duration != 61 || string(f.Data) != "video" {
		t.Fatalf("Fetch() = %+v, %v", f, err)
	}

	// Refusals by the filter say why.
	for _, tt := range []struct {
		meta string
		want error
	}{
		{`{"id":"x","duration":901}`, downloader.ErrTooLong},
		{`{"id":"x","is_live":true}`, downloader.ErrNoMedia},
		{`{"id":"x"}`, downloader.ErrNoMedia},                // no duration: treated as live
		{`{"id":"x","duration":60}`, downloader.ErrTooLarge}, // passed the filter, refused by --max-filesize
	} {
		if _, err := NewWithRunner(run(tt.meta, false), 1).Fetch(context.Background(), link, req); !errors.Is(err, tt.want) {
			t.Errorf("meta %s: err = %v, want %v", tt.meta, err, tt.want)
		}
	}
}
