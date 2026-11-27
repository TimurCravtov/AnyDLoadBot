// Package instagram is a downloader.Provider for Instagram posts, reels and
// IGTV. Videos go through the shared yt-dlp client; image posts fall back to
// Instagram's public "media" redirect. Only public posts work without cookies.
package instagram

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"anydloadbot/internal/downloader"
	"anydloadbot/internal/downloader/ytdlp"
)

const (
	siteBase       = "https://www.instagram.com"
	previewTimeout = 3 * time.Second

	// Format "0" is Instagram's progressive h264 file, which plays everywhere in
	// Telegram; the DASH streams are VP9 video-only. The rest are fallbacks.
	formatSelector = "0/bv*+ba/b"
	formatSort     = "res:720,ext:mp4:m4a"
	// directFormat is formatSelector without the merge: a direct URL has to be
	// one file with both video and sound.
	directFormat = "0/b[ext=mp4]"
)

var (
	linkRe = regexp.MustCompile(`https?://[^\s<>"]+`)
	postID = regexp.MustCompile(`^[A-Za-z0-9_-]{5,24}$`)

	// Path segments that introduce a post ID: /p/ID, /reel/ID, /reels/ID, /tv/ID,
	// also written /<username>/p/ID.
	postKinds = map[string]bool{"p": true, "reel": true, "reels": true, "tv": true}

	// yt-dlp's wording when a post has no video, i.e. it is an image post.
	noVideoRe = regexp.MustCompile(`(?i)no video (in this post|formats found)|there is no video`)
)

// Options configures a Provider.
type Options struct {
	// Cookies is an optional Netscape-format cookies.txt exported from a logged-in
	// browser. Instagram requires it for some posts (age-restricted, rate-limited).
	Cookies string
}

// Provider downloads Instagram media.
type Provider struct {
	yt       *ytdlp.Client
	cookies  string
	fetch    *downloader.Fetcher
	siteBase string
}

var (
	_ downloader.Provider        = (*Provider)(nil)
	_ downloader.Previewer       = (*Provider)(nil)
	_ downloader.AudioDownloader = (*Provider)(nil)
	_ downloader.Linker          = (*Provider)(nil)
)

// New creates an Instagram provider on top of a shared yt-dlp client.
func New(yt *ytdlp.Client, opts Options) *Provider {
	return &Provider{
		yt:       yt,
		cookies:  opts.Cookies,
		siteBase: siteBase,
		fetch: downloader.NewFetcher(func(host string) bool {
			return isInstagramHost(host) || isCDNHost(host)
		}),
	}
}

func isCDNHost(host string) bool {
	host = strings.ToLower(host)
	return strings.HasSuffix(host, ".cdninstagram.com") || strings.HasSuffix(host, ".fbcdn.net")
}

// Name implements downloader.Provider.
func (*Provider) Name() string { return "Instagram" }

// Description implements downloader.Describer.
func (*Provider) Description() string {
	return `reels, videos and images from public posts (first item of a carousel); add "mp3" after the link for the sound`
}

func isInstagramHost(host string) bool {
	host = strings.ToLower(host)
	return host == "instagram.com" || host == "www.instagram.com" || host == "instagr.am"
}

// FindURL implements downloader.Provider. The returned link is canonical
// (https://www.instagram.com/p/ID/), which yt-dlp accepts for reels too.
func (*Provider) FindURL(text string) (string, bool) {
	for _, m := range linkRe.FindAllString(text, -1) {
		if id, ok := parsePostID(strings.TrimRight(m, ".,;:!?)]}'")); ok {
			return siteBase + "/p/" + id + "/", true
		}
	}
	return "", false
}

func parsePostID(link string) (string, bool) {
	u, err := url.Parse(link)
	if err != nil || !isInstagramHost(u.Hostname()) {
		return "", false
	}

	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	for i := 0; i < 2 && i+1 < len(parts); i++ {
		if postKinds[parts[i]] && postID.MatchString(parts[i+1]) {
			return parts[i+1], true
		}
	}
	return "", false
}

// Preview implements downloader.Previewer. Instagram's media endpoint redirects
// to the post's image on a CDN; that URL is the placeholder thumbnail. Without
// it there is no placeholder (Telegram requires a photo).
func (p *Provider) Preview(ctx context.Context, link string) (*downloader.Preview, bool) {
	id, ok := parsePostID(link)
	if !ok {
		return nil, false
	}

	ctx, cancel := context.WithTimeout(ctx, previewTimeout)
	defer cancel()

	thumb, err := p.fetch.FinalURL(ctx, p.mediaURL(id))
	if err != nil {
		return nil, false
	}
	return &downloader.Preview{
		ThumbnailURL: thumb,
		Title:        "Instagram post",
		Description:  "Tap to download",
	}, true
}

func (p *Provider) mediaURL(id string) string {
	return p.siteBase + "/p/" + id + "/media/?size=l"
}

// Download implements downloader.Provider.
func (p *Provider) Download(ctx context.Context, link string) (*downloader.Media, error) {
	id, ok := parsePostID(link)
	if !ok {
		return nil, fmt.Errorf("not an Instagram link: %q", link)
	}

	release, err := p.yt.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	req := ytdlp.Request{Format: formatSelector, Sort: formatSort}
	if p.cookies != "" {
		req.Extra = []string{"--cookies", p.cookies}
	}

	file, err := p.yt.Fetch(ctx, link, req)
	switch {
	case err == nil:
		return file.Media(downloader.KindVideo), nil
	case noVideoRe.MatchString(err.Error()):
		return p.image(ctx, id)
	default:
		return nil, err
	}
}

// MediaURL implements downloader.Linker: the post's video file on Instagram's
// CDN, or for image posts the picture, for Telegram to fetch itself.
func (p *Provider) MediaURL(ctx context.Context, link string) (*downloader.Source, error) {
	id, ok := parsePostID(link)
	if !ok {
		return nil, fmt.Errorf("not an Instagram link: %q", link)
	}

	release, err := p.yt.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	req := ytdlp.Request{Format: directFormat, Sort: formatSort}
	if p.cookies != "" {
		req.Extra = []string{"--cookies", p.cookies}
	}
	d, err := p.yt.DirectURL(ctx, link, req)
	release()

	switch {
	case err == nil:
	case noVideoRe.MatchString(err.Error()):
		img, err := p.fetch.FinalURL(ctx, p.mediaURL(id))
		if err != nil {
			return nil, fmt.Errorf("image URL: %w", err)
		}
		if u, err := url.Parse(img); err != nil || u.Scheme != "https" || !isCDNHost(u.Hostname()) {
			return nil, fmt.Errorf("image URL %q is not on Instagram's CDN", img)
		}
		return &downloader.Source{Kind: downloader.KindPhoto, URL: img}, nil
	default:
		return nil, err
	}

	if u, err := url.Parse(d.URL); err != nil || !isCDNHost(u.Hostname()) {
		return nil, fmt.Errorf("video URL is not on Instagram's CDN: %q", d.URL)
	}
	if d.Ext != "mp4" {
		return nil, fmt.Errorf("video is %s, not mp4", d.Ext)
	}
	if d.Size > downloader.MaxURLSize {
		// Not ErrTooLarge: up to downloader.MaxSize it can still be uploaded.
		return nil, fmt.Errorf("%d bytes is more than Telegram fetches from a URL", d.Size)
	}
	return &downloader.Source{Kind: downloader.KindVideo, URL: d.URL, MimeType: "video/mp4"}, nil
}

// DownloadAudio implements downloader.AudioDownloader: the post's sound as MP3,
// with the title and cover embedded. Image posts and silent videos have no
// audio and return downloader.ErrNoMedia.
func (p *Provider) DownloadAudio(ctx context.Context, link string) (*downloader.Media, error) {
	if _, ok := parsePostID(link); !ok {
		return nil, fmt.Errorf("not an Instagram link: %q", link)
	}

	release, err := p.yt.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	var extra []string
	if p.cookies != "" {
		extra = []string{"--cookies", p.cookies}
	}

	// One run: the metadata comes with the download.
	file, err := p.yt.Fetch(ctx, link, ytdlp.Request{Audio: true, Extra: extra})
	if err != nil {
		if noVideoRe.MatchString(err.Error()) {
			return nil, fmt.Errorf("%w: image posts have no audio", downloader.ErrNoMedia)
		}
		return nil, err
	}
	m := file.Media(downloader.KindAudio)
	m.Title = file.Info.Title
	m.Performer = file.Info.Uploader
	m.Duration = int(file.Info.Duration)
	return m, nil
}

// image downloads the post's picture (the first one, for carousels).
func (p *Provider) image(ctx context.Context, id string) (*downloader.Media, error) {
	data, err := p.fetch.GetMedia(ctx, p.mediaURL(id), downloader.MaxSize, "image/jpeg")
	if err != nil {
		return nil, fmt.Errorf("%w: image: %v", downloader.ErrNoMedia, err)
	}
	if !strings.HasPrefix(http.DetectContentType(data), "image/") {
		return nil, fmt.Errorf("%w: media endpoint did not return an image", downloader.ErrNoMedia)
	}
	return &downloader.Media{Kind: downloader.KindPhoto, Filename: id + ".jpg", Data: data}, nil
}
