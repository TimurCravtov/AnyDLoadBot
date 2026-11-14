package pinterest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
			name: "unicode-escaped slashes",
			page: strings.ReplaceAll(`"url":"https://v1.pinimg.com/videos/iht/expMp4/a_720w.mp4"`, "/", `\`+"u002F"),
			want: "https://v1.pinimg.com/videos/iht/expMp4/a_720w.mp4",
		},
		{
			name: "backslash-escaped slashes",
			page: strings.ReplaceAll(`"url":"https://v1.pinimg.com/videos/iht/expMp4/a_720w.mp4"`, "/", `\/`),
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

func TestParseThumbnailURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		page string
		want string
	}{
		{
			name: "prefers video thumbnail",
			page: `"https://i.pinimg.com/236x/aa/bb.jpg" "https://i.pinimg.com/videos/thumbnails/originals/dd/52/x.0000000.jpg"`,
			want: "https://i.pinimg.com/videos/thumbnails/originals/dd/52/x.0000000.jpg",
		},
		{name: "falls back to any jpeg", page: `"https://i.pinimg.com/236x/aa/bb.jpg"`, want: "https://i.pinimg.com/236x/aa/bb.jpg"},
		{name: "ignores non-jpeg and other hosts", page: `https://i.pinimg.com/a.webp https://evil.com/a.jpg`, want: ""},
	}
	for _, tt := range tests {
		if got := ParseThumbnailURL([]byte(tt.page)); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
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

// pinServer serves a widget endpoint and a pin page, counting requests to each.
type pinServer struct {
	widgetBody, pageBody string
	widgetHits, pageHits atomic.Int32
}

func (s *pinServer) start(t *testing.T) (*httptest.Server, *Provider) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v3/pidgets/pins/info/") {
			s.widgetHits.Add(1)
			_, _ = w.Write([]byte(s.widgetBody))
			return
		}
		s.pageHits.Add(1)
		_, _ = w.Write([]byte(s.pageBody))
	}))
	t.Cleanup(srv.Close)

	p := New()
	p.fetch = downloader.NewFetcher(func(string) bool { return true })
	p.widget = srv.URL + "/v3/pidgets/pins/info/?pin_ids="
	p.allowLink = func(string) bool { return true }
	return srv, p
}

const (
	widgetWithVideo = `{"status":"success","data":[{"id":"1036179826779090032","story_pin_data":{"pages":[{"blocks":[{"video":{"video_list":{"V_EXP7":{"url":"https://v1.pinimg.com/videos/iht/expMp4/dd/52/x_720w.mp4","thumbnail":"https://i.pinimg.com/videos/thumbnails/originals/dd/52/x.0000000.jpg"}}}}]}]}}]}`
	widgetNoVideo   = `{"status":"success","data":[{"id":"1","videos":null,"images":{"237x":{"url":"https://i.pinimg.com/237x/aa/bb.jpg"}}}]}`
	pageWithVideo   = `<html><script>{"url":"https://v1.pinimg.com/videos/mc/720p/page/clip.mp4"}</script></html>`
)

func TestResolveUsesWidgetForLongLinks(t *testing.T) {
	t.Parallel()

	s := &pinServer{widgetBody: widgetWithVideo, pageBody: pageWithVideo}
	srv, p := s.start(t)

	src, err := p.Resolve(context.Background(), srv.URL+"/pin/1036179826779090032/")
	if err != nil {
		t.Fatal(err)
	}
	if src.URL != "https://v1.pinimg.com/videos/iht/expMp4/dd/52/x_720w.mp4" {
		t.Errorf("URL = %q, want the widget's video", src.URL)
	}
	if src.ThumbnailURL != "https://i.pinimg.com/videos/thumbnails/originals/dd/52/x.0000000.jpg" {
		t.Errorf("thumbnail = %q", src.ThumbnailURL)
	}
	if s.widgetHits.Load() != 1 || s.pageHits.Load() != 0 {
		t.Errorf("hits widget=%d page=%d, want 1 and 0 (the page must not be needed)", s.widgetHits.Load(), s.pageHits.Load())
	}
}

func TestResolveFallsBackToPageWhenWidgetHasNoVideo(t *testing.T) {
	t.Parallel()

	s := &pinServer{widgetBody: widgetNoVideo, pageBody: pageWithVideo}
	srv, p := s.start(t)

	src, err := p.Resolve(context.Background(), srv.URL+"/pin/123456789/")
	if err != nil {
		t.Fatal(err)
	}
	if src.URL != "https://v1.pinimg.com/videos/mc/720p/page/clip.mp4" {
		t.Errorf("URL = %q, want the page's video", src.URL)
	}
	if s.widgetHits.Load() != 1 || s.pageHits.Load() != 1 {
		t.Errorf("hits widget=%d page=%d, want 1 and 1", s.widgetHits.Load(), s.pageHits.Load())
	}
}

func TestResolveShortLinksSkipTheWidget(t *testing.T) {
	t.Parallel()

	s := &pinServer{widgetBody: widgetWithVideo, pageBody: pageWithVideo}
	srv, p := s.start(t)

	// A pin.it-style link doesn't name the pin, so only the page can be used.
	src, err := p.Resolve(context.Background(), srv.URL+"/AbCdEf/")
	if err != nil {
		t.Fatal(err)
	}
	if src.URL != "https://v1.pinimg.com/videos/mc/720p/page/clip.mp4" || s.widgetHits.Load() != 0 {
		t.Errorf("URL = %q, widget hits = %d; want the page's video and no widget call", src.URL, s.widgetHits.Load())
	}
}

func TestResolveNoVideoAnywhere(t *testing.T) {
	t.Parallel()

	s := &pinServer{widgetBody: widgetNoVideo, pageBody: `<html>image pin</html>`}
	srv, p := s.start(t)

	if _, err := p.Resolve(context.Background(), srv.URL+"/pin/123456789/"); !errors.Is(err, downloader.ErrNoMedia) {
		t.Errorf("err = %v, want ErrNoMedia", err)
	}
}

func TestResolveShortLinkFindsPinIDFromRedirects(t *testing.T) {
	t.Parallel()

	var widgetHits, pinPageHits atomic.Int32
	mux := http.NewServeMux()
	// pin.it -> url shortener -> /pin/<id>/sent/?invite_code=..., like the real chain.
	mux.HandleFunc("/AbCdEf", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/url_shortener/AbCdEf/redirect/", http.StatusFound)
	})
	mux.HandleFunc("/url_shortener/AbCdEf/redirect/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/pin/1036179826779090032/sent/?invite_code=x&sfo=1", http.StatusFound)
	})
	mux.HandleFunc("/pin/", func(w http.ResponseWriter, _ *http.Request) {
		pinPageHits.Add(1)
		_, _ = w.Write([]byte(pageWithVideo))
	})
	mux.HandleFunc("/v3/pidgets/pins/info/", func(w http.ResponseWriter, r *http.Request) {
		widgetHits.Add(1)
		if r.URL.Query().Get("pin_ids") != "1036179826779090032" {
			http.Error(w, "wrong pin", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(widgetWithVideo))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	p := New()
	p.fetch = downloader.NewFetcher(func(string) bool { return true })
	p.widget = srv.URL + "/v3/pidgets/pins/info/?pin_ids="
	p.allowLink = func(string) bool { return true }

	src, err := p.Resolve(context.Background(), srv.URL+"/AbCdEf")
	if err != nil {
		t.Fatal(err)
	}
	if src.URL != "https://v1.pinimg.com/videos/iht/expMp4/dd/52/x_720w.mp4" {
		t.Errorf("URL = %q, want the widget's video", src.URL)
	}
	// The pin page itself must never be downloaded on this route.
	if widgetHits.Load() != 1 || pinPageHits.Load() != 0 {
		t.Errorf("hits widget=%d pin page=%d, want 1 and 0", widgetHits.Load(), pinPageHits.Load())
	}
}

func TestShortLinkRedirectsMustStayOnPinterest(t *testing.T) {
	t.Parallel()

	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("followed a redirect off Pinterest")
	}))
	t.Cleanup(evil.Close)
	// The redirect points at another host name ("localhost"), as an attacker's would.
	evilURL := strings.Replace(evil.URL, "127.0.0.1", "localhost", 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evilURL+"/pin/123456789/", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	p := New()
	p.fetch = downloader.NewFetcher(func(string) bool { return true })
	// Only 127.0.0.1 counts as "Pinterest" here.
	p.allowLink = func(host string) bool { return host == "127.0.0.1" }

	if id := p.pinIDFromRedirects(context.Background(), srv.URL+"/AbCdEf"); id != "" {
		t.Errorf("pin ID %q taken from a redirect that left Pinterest", id)
	}
}
