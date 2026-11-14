// Package pinterest is a downloader.Provider for video pins on Pinterest.
package pinterest

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"anydloadbot/internal/downloader"
)

const (
	maxPageSize   = 10 << 20
	maxWidgetSize = 1 << 20
	// The public JSON endpoint behind Pinterest's embedded-pin widgets.
	widgetURL = "https://widgets.pinterest.com/v3/pidgets/pins/info/?pin_ids="
)

var (
	pinterestHost = regexp.MustCompile(`^(?:[a-z0-9-]+\.)*pinterest\.[a-z]{2,3}(?:\.[a-z]{2})?$`)
	linkRe        = regexp.MustCompile(`https?://[^\s<>"]+`)
	// Matches direct MP4 links on Pinterest's video CDN (after JSON un-escaping).
	videoRe = regexp.MustCompile(`https://[a-z0-9.-]+\.pinimg\.com/videos/[^"'\s<>\\]+?\.mp4`)
	// Telegram only accepts JPEG thumbnails for inline videos.
	thumbRe = regexp.MustCompile(`https://i\.pinimg\.com/[^"'\s<>\\]+?\.jpe?g`)
	// Quality hints: ".../720p/..." or "..._720w.mp4".
	qualityRe = regexp.MustCompile(`/(\d+)p/|_(\d+)w\.mp4$`)
	// JSON may escape "/" as \/ or as the unicode escape \u002F.
	unescaper = strings.NewReplacer(`\/`, "/", `\`+"u002F", "/", `\`+"u002f", "/", `&amp;`, "&")
	// Long links name the pin: /pin/<id>/ (also on country domains).
	pinPath = regexp.MustCompile(`^/pin/([0-9]{5,25})(?:/|$)`)
)

func isPinterestHost(host string) bool {
	host = strings.ToLower(host)
	return host == "pin.it" || pinterestHost.MatchString(host)
}

func isCDNHost(host string) bool {
	host = strings.ToLower(host)
	return host == "pinimg.com" || strings.HasSuffix(host, ".pinimg.com")
}

// Provider downloads videos from Pinterest pins (pinterest.* and pin.it links).
type Provider struct {
	fetch     *downloader.Fetcher
	widget    string                 // widgetURL; tests point it elsewhere
	allowLink func(host string) bool // which links Resolve accepts
}

var (
	_ downloader.Provider = (*Provider)(nil)
	_ downloader.Resolver = (*Provider)(nil)
)

// New creates a Pinterest provider. Redirects (including pin.it short links)
// are only followed to Pinterest or its CDN.
func New() *Provider {
	return &Provider{
		fetch: downloader.NewFetcher(func(h string) bool {
			return isPinterestHost(h) || isCDNHost(h)
		}),
		widget:    widgetURL,
		allowLink: isPinterestHost,
	}
}

// Name implements downloader.Provider.
func (*Provider) Name() string { return "Pinterest" }

// Description implements downloader.Describer.
func (*Provider) Description() string { return "video pins (pinterest.com and pin.it links)" }

// FindURL implements downloader.Provider.
func (*Provider) FindURL(text string) (string, bool) {
	for _, m := range linkRe.FindAllString(text, -1) {
		m = strings.TrimRight(m, ".,;:!?)]}'")
		u, err := url.Parse(m)
		if err == nil && isPinterestHost(u.Hostname()) {
			return m, true
		}
	}
	return "", false
}

// Resolve implements downloader.Resolver.
func (p *Provider) Resolve(ctx context.Context, link string) (*downloader.Source, error) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !p.allowLink(u.Hostname()) {
		return nil, fmt.Errorf("not a Pinterest link: %q", link)
	}

	// Ask the embed widget's JSON first. It is small, and it is served to
	// visitors that get a login or consent wall instead of the full pin page
	// (some regions and datacenter IPs). It needs the pin's ID: long links name
	// it, and short pin.it links redirect to an address that does.
	id := ""
	if m := pinPath.FindStringSubmatch(u.Path); m != nil {
		id = m[1]
	} else {
		id = p.pinIDFromRedirects(ctx, link)
	}
	if id != "" {
		if src, err := p.fromJSON(ctx, p.widget+id, maxWidgetSize, "application/json"); err == nil {
			return src, nil
		}
	}
	return p.fromJSON(ctx, link, maxPageSize, "text/html")
}

// pinIDFromRedirects follows a link's redirects (pin.it goes through a couple)
// until one names the pin, without downloading any page. It returns "" if none
// does or a hop leaves Pinterest.
func (p *Provider) pinIDFromRedirects(ctx context.Context, link string) string {
	next := link
	for range 5 {
		loc, err := p.fetch.Location(ctx, next)
		if err != nil || loc == "" {
			return ""
		}
		u, err := url.Parse(loc)
		if err != nil || !p.allowLink(u.Hostname()) {
			return ""
		}
		if m := pinPath.FindStringSubmatch(u.Path); m != nil {
			return m[1]
		}
		next = loc
	}
	return ""
}

// fromJSON fetches a pin page or widget response and extracts the video from it.
func (p *Provider) fromJSON(ctx context.Context, from string, limit int64, accept string) (*downloader.Source, error) {
	body, err := p.fetch.Get(ctx, from, limit, accept)
	if err != nil {
		return nil, fmt.Errorf("fetch pin: %w", err)
	}

	videoURL, err := ParseVideoURL(body)
	if err != nil {
		return nil, err
	}
	if vu, err := url.Parse(videoURL); err != nil || !isCDNHost(vu.Hostname()) {
		return nil, fmt.Errorf("unexpected video host in %q", videoURL)
	}

	return &downloader.Source{
		URL:          videoURL,
		ThumbnailURL: ParseThumbnailURL(body),
		Title:        "Pinterest video",
		MimeType:     "video/mp4",
	}, nil
}

// Download implements downloader.Provider.
func (p *Provider) Download(ctx context.Context, link string) (*downloader.Media, error) {
	src, err := p.Resolve(ctx, link)
	if err != nil {
		return nil, err
	}

	data, err := p.fetch.Get(ctx, src.URL, downloader.MaxSize, src.MimeType)
	if err != nil {
		return nil, fmt.Errorf("download video: %w", err)
	}
	return &downloader.Media{Filename: path.Base(src.URL), Data: data}, nil
}

// ParseThumbnailURL extracts a JPEG preview from a pin page, preferring the
// video's own thumbnail. It returns "" if none is found.
func ParseThumbnailURL(page []byte) string {
	fallback := ""
	for _, m := range thumbRe.FindAllString(unescaper.Replace(string(page)), -1) {
		if strings.Contains(m, "/videos/thumbnails/") {
			return m
		}
		if fallback == "" {
			fallback = m
		}
	}
	return fallback
}

// ParseVideoURL extracts the best-quality MP4 URL from a pin page.
func ParseVideoURL(page []byte) (string, error) {
	matches := videoRe.FindAllString(unescaper.Replace(string(page)), -1)

	best, bestScore := "", -1
	for _, m := range matches {
		if score := quality(m); score > bestScore {
			best, bestScore = m, score
		}
	}
	if best == "" {
		return "", downloader.ErrNoMedia
	}
	return best, nil
}

func quality(videoURL string) int {
	for _, s := range qualityRe.FindStringSubmatch(videoURL) {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return 0
}
