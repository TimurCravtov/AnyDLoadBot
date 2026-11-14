package twitter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"anydloadbot/internal/downloader"
)

func TestFindURL(t *testing.T) {
	t.Parallel()

	const id = "2121224564122300486"
	const want = "https://x.com/i/status/" + id
	tests := []struct {
		text string
		want string
		ok   bool
	}{
		{"https://x.com/RealitySalesman/status/" + id, want, true},
		{"https://twitter.com/RealitySalesman/status/" + id + "?s=20", want, true},
		{"https://mobile.twitter.com/u/status/" + id, want, true},
		{"https://x.com/i/status/" + id, want, true},
		{"https://x.com/u/status/" + id + "/video/1", want, true},
		{"https://fixupx.com/u/status/" + id, want, true},
		{"look (https://x.com/u/status/" + id + ").", want, true},
		{"https://x.com/RealitySalesman", "", false},
		{"https://x.com/u/status/abc", "", false},
		{"https://notx.com/u/status/" + id, "", false},
		{"https://x.com.evil.io/u/status/" + id, "", false},
		{"no links", "", false},
	}
	for _, tt := range tests {
		got, ok := New().FindURL(tt.text)
		if got != tt.want || ok != tt.ok {
			t.Errorf("FindURL(%q) = %q, %v; want %q, %v", tt.text, got, ok, tt.want, tt.ok)
		}
	}
}

const (
	videoTweet = `{"__typename":"Tweet","text":"He handled this perfectly. 🫡  https://t.co/eUKtHF3271","user":{"screen_name":"RealitySalesman"},
	"mediaDetails":[{"type":"video","media_url_https":"https://pbs.twimg.com/poster.jpg","video_info":{"duration_millis":50000,"variants":[
		{"content_type":"application/x-mpegURL","url":"https://video.twimg.com/pl/x.m3u8"},
		{"bitrate":432000,"content_type":"video/mp4","url":"https://video.twimg.com/vid/320x320/a.mp4"},
		{"bitrate":832000,"content_type":"video/mp4","url":"https://video.twimg.com/vid/540x540/b.mp4"},
		{"bitrate":1280000,"content_type":"video/mp4","url":"https://video.twimg.com/vid/720x720/c.mp4"}]}}]}`
	photoTweet = `{"__typename":"Tweet","text":"a photo","user":{"screen_name":"u"},"mediaDetails":[
		{"type":"photo","media_url_https":"https://pbs.twimg.com/media/abc.jpg"},
		{"type":"photo","media_url_https":"https://pbs.twimg.com/media/def.jpg"}]}`
	textTweet = `{"__typename":"Tweet","text":"just words","user":{"screen_name":"u"},"mediaDetails":[]}`
	tombstone = `{"__typename":"TweetTombstone"}`
)

func newTestProvider(t *testing.T, body string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") == "" || r.URL.Query().Get("token") == "" {
			_, _ = w.Write([]byte("{}")) // what the real endpoint does without id/token
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	p := New()
	p.endpoint = srv.URL
	p.fetch = downloader.NewFetcher(func(string) bool { return true })
	return p
}

const link = "https://x.com/i/status/2121224564122300486"

func TestResolveVideo(t *testing.T) {
	t.Parallel()

	src, err := newTestProvider(t, videoTweet).Resolve(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	// 1280 kbps * 50 s = 8 MB, under the inline cap, so the best variant wins.
	if src.Kind != downloader.KindVideo || src.URL != "https://video.twimg.com/vid/720x720/c.mp4" {
		t.Errorf("unexpected source: %+v", src)
	}
	if src.ThumbnailURL != "https://pbs.twimg.com/poster.jpg" || src.MimeType != "video/mp4" {
		t.Errorf("thumbnail/mime: %+v", src)
	}
	if src.Title != "@RealitySalesman: He handled this perfectly. 🫡" {
		t.Errorf("title = %q", src.Title)
	}
}

func TestResolvePicksVariantThatFits(t *testing.T) {
	t.Parallel()

	// Same tweet but 10 minutes long: 1280k is ~96 MB, 832k ~62 MB, 432k ~32 MB.
	long := `{"__typename":"Tweet","text":"x","user":{"screen_name":"u"},"mediaDetails":[{"type":"video","media_url_https":"p.jpg","video_info":{"duration_millis":600000,"variants":[
		{"bitrate":432000,"content_type":"video/mp4","url":"low.mp4"},
		{"bitrate":832000,"content_type":"video/mp4","url":"mid.mp4"},
		{"bitrate":1280000,"content_type":"video/mp4","url":"high.mp4"}]}}]}`
	p := newTestProvider(t, long)

	// Chat downloads allow up to 48 MB: only the lowest fits...
	if src, err := p.resolve(context.Background(), link, chatMaxBytes); err != nil || src.URL != "low.mp4" {
		t.Errorf("chat: %+v, %v; want low.mp4", src, err)
	}
	// ...and with a 100 MB budget the best one does.
	if src, err := p.resolve(context.Background(), link, 100<<20); err != nil || src.URL != "high.mp4" {
		t.Errorf("big budget: %+v, %v; want high.mp4", src, err)
	}
	// Nothing fits the inline cap, so it falls back to the smallest variant.
	if src, err := p.Resolve(context.Background(), link); err != nil || src.URL != "low.mp4" {
		t.Errorf("inline: %+v, %v; want smallest as fallback", src, err)
	}
}

func TestResolvePhoto(t *testing.T) {
	t.Parallel()

	src, err := newTestProvider(t, photoTweet).Resolve(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if src.Kind != downloader.KindPhoto || src.URL != "https://pbs.twimg.com/media/abc.jpg?name=large" ||
		src.ThumbnailURL != "https://pbs.twimg.com/media/abc.jpg?name=small" {
		t.Errorf("unexpected source: %+v", src)
	}
}

func TestResolveNoMedia(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{"text only": textTweet, "deleted": tombstone, "empty": "{}"} {
		_, err := newTestProvider(t, body).Resolve(context.Background(), link)
		if !errors.Is(err, downloader.ErrNoMedia) {
			t.Errorf("%s: err = %v, want ErrNoMedia", name, err)
		}
	}
}

func TestDownload(t *testing.T) {
	t.Parallel()

	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("mp4-bytes")) }))
	t.Cleanup(media.Close)

	body := `{"__typename":"Tweet","text":"hi","user":{"screen_name":"u"},"mediaDetails":[{"type":"video","media_url_https":"p.jpg","video_info":{"duration_millis":1000,"variants":[
		{"bitrate":800000,"content_type":"video/mp4","url":"` + media.URL + `/vid/clip.mp4"}]}}]}`
	m, err := newTestProvider(t, body).Download(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != downloader.KindVideo || m.Filename != "clip.mp4" || string(m.Data) != "mp4-bytes" || m.Title != "@u: hi" {
		t.Errorf("unexpected media: %+v", m)
	}
}

func TestResolveRejectsForeignLinks(t *testing.T) {
	t.Parallel()

	if _, err := New().Resolve(context.Background(), "https://example.com/u/status/1"); err == nil {
		t.Error("Resolve() accepted a non-X link")
	}
}
