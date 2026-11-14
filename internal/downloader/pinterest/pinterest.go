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

const maxPageSize = 10 << 20

var (
	pinterestHost = regexp.MustCompile(`^(?:[a-z0-9-]+\.)*pinterest\.[a-z]{2,3}(?:\.[a-z]{2})?$`)
	linkRe        = regexp.MustCompile(`https?://[^\s<>"]+`)
	// Matches direct MP4 links on Pinterest's video CDN (after JSON un-escaping).
	videoRe = regexp.MustCompile(`https://[a-z0-9.-]+\.pinimg\.com/videos/[^"'\s<>\\]+?\.mp4`)
	// Quality hints: ".../720p/..." or "..._720w.mp4".
	qualityRe = regexp.MustCompile(`/(\d+)p/|_(\d+)w\.mp4$`)
	unescaper = strings.NewReplacer(`/`, "/", `/`, "/", `\/`, "/", `&amp;`, "&")
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
	fetch *downloader.Fetcher
}

var _ downloader.Provider = (*Provider)(nil)

// New creates a Pinterest provider. Redirects (including pin.it short links)
// are only followed to Pinterest or its CDN.
func New() *Provider {
	return &Provider{fetch: downloader.NewFetcher(func(h string) bool {
		return isPinterestHost(h) || isCDNHost(h)
	})}
}

// Name implements downloader.Provider.
func (*Provider) Name() string { return "Pinterest" }

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

// Download implements downloader.Provider.
func (p *Provider) Download(ctx context.Context, link string) (*downloader.Media, error) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !isPinterestHost(u.Hostname()) {
		return nil, fmt.Errorf("not a Pinterest link: %q", link)
	}

	page, err := p.fetch.Get(ctx, link, maxPageSize, "text/html")
	if err != nil {
		return nil, fmt.Errorf("fetch pin page: %w", err)
	}

	videoURL, err := ParseVideoURL(page)
	if err != nil {
		return nil, err
	}
	vu, err := url.Parse(videoURL)
	if err != nil || !isCDNHost(vu.Hostname()) {
		return nil, fmt.Errorf("unexpected video host in %q", videoURL)
	}

	data, err := p.fetch.Get(ctx, videoURL, downloader.MaxSize, "video/mp4")
	if err != nil {
		return nil, fmt.Errorf("download video: %w", err)
	}
	return &downloader.Media{Filename: path.Base(vu.Path), Data: data}, nil
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
