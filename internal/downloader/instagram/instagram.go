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
	_ downloader.Provider  = (*Provider)(nil)
	_ downloader.Previewer = (*Provider)(nil)
)

// New creates an Instagram provider on top of a shared yt-dlp client.
func New(yt *ytdlp.Client, opts Options) *Provider {
	return &Provider{
		yt:       yt,
		cookies:  opts.Cookies,
		siteBase: siteBase,
		fetch: downloader.NewFetcher(func(host string) bool {
			host = strings.ToLower(host)
			return isInstagramHost(host) ||
				strings.HasSuffix(host, ".cdninstagram.com") ||
				strings.HasSuffix(host, ".fbcdn.net")
		}),
	}
}

// Name implements downloader.Provider.
func (*Provider) Name() string { return "Instagram" }

// Description implements downloader.Describer.
func (*Provider) Description() string {
	return "reels, videos and images from public posts (first item of a carousel)"
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
		return &downloader.Media{Kind: downloader.KindVideo, Filename: file.Name, Data: file.Data}, nil
	case noVideoRe.MatchString(err.Error()):
		return p.image(ctx, id)
	default:
		return nil, err
	}
}

// image downloads the post's picture (the first one, for carousels).
func (p *Provider) image(ctx context.Context, id string) (*downloader.Media, error) {
	data, err := p.fetch.Get(ctx, p.mediaURL(id), downloader.MaxSize, "image/jpeg")
	if err != nil {
		return nil, fmt.Errorf("%w: image: %v", downloader.ErrNoMedia, err)
	}
	if !strings.HasPrefix(http.DetectContentType(data), "image/") {
		return nil, fmt.Errorf("%w: media endpoint did not return an image", downloader.ErrNoMedia)
	}
	return &downloader.Media{Kind: downloader.KindPhoto, Filename: id + ".jpg", Data: data}, nil
}
