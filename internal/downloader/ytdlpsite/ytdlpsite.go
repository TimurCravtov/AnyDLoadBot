// Package ytdlpsite is a table-driven downloader.Provider for sites that the
// shared yt-dlp client can already handle (TikTok, Reddit, ...). Adding a site
// means adding a Site value, not writing a new provider.
package ytdlpsite

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"anydloadbot/internal/downloader"
	"anydloadbot/internal/downloader/ytdlp"
)

const (
	// DefaultMaxDuration is the longest video the provider will download.
	DefaultMaxDuration = 15 * time.Minute

	// Prefer the best streams that fit; fall back to whatever yt-dlp thinks is
	// best (its --max-filesize is the hard stop when sizes aren't known up front).
	formatSelector = "bv*[filesize<40M]+ba/b[filesize<48M]/bv*+ba/b"
	// h264 plays everywhere in Telegram; newer codecs may not.
	formatSort = "res:720,vcodec:h264,ext:mp4:m4a"
)

var linkRe = regexp.MustCompile(`https?://[^\s<>"]+`)

// Site describes one website.
type Site struct {
	// Name is shown to users, e.g. "TikTok".
	Name string
	// Description is one short line for /help.
	Description string
	// Match reports whether u is a link to a downloadable post on this site.
	// It must be strict: the host check is what keeps users from pointing the
	// bot at arbitrary servers.
	Match func(u *url.URL) bool
	// NoCaption sends the video without a caption. Use it for sites whose
	// "title" is just a long description (TikTok).
	NoCaption bool
	// Preview, if set, describes a link for its inline-mode result row. It must
	// not do network I/O (inline queries have to be answered quickly); work it
	// out from the URL. Sites with a Preview work in inline mode.
	Preview func(u *url.URL) *downloader.Preview
}

// Options configures a Provider.
type Options struct {
	// MaxDuration is the longest allowed video (default DefaultMaxDuration).
	MaxDuration time.Duration
	// Cookies is an optional Netscape-format cookies.txt for sites that require login.
	Cookies string
}

// Provider downloads videos from one Site through yt-dlp.
type Provider struct {
	yt          *ytdlp.Client
	site        Site
	maxDuration time.Duration
	cookies     string
}

var (
	_ downloader.Provider  = (*Provider)(nil)
	_ downloader.Describer = (*Provider)(nil)
)

// New creates a provider for site on top of a shared yt-dlp client.
func New(yt *ytdlp.Client, site Site, opts Options) *Provider {
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = DefaultMaxDuration
	}
	return &Provider{yt: yt, site: site, maxDuration: opts.MaxDuration, cookies: opts.Cookies}
}

// WithInline returns p as a downloader.Previewer when its site has a Preview,
// so it works in inline mode; otherwise it returns p unchanged. Register the
// result rather than p itself.
func WithInline(p *Provider) downloader.Provider {
	if p.site.Preview == nil {
		return p
	}
	return &inlineProvider{Provider: p}
}

type inlineProvider struct{ *Provider }

var _ downloader.Previewer = (*inlineProvider)(nil)

// Preview implements downloader.Previewer.
func (p *inlineProvider) Preview(_ context.Context, link string) (*downloader.Preview, bool) {
	if _, ok := p.FindURL(link); !ok {
		return nil, false
	}
	u, err := url.Parse(link)
	if err != nil {
		return nil, false
	}
	return p.site.Preview(u), true
}

// Name implements downloader.Provider.
func (p *Provider) Name() string { return p.site.Name }

// Description implements downloader.Describer.
func (p *Provider) Description() string {
	return fmt.Sprintf("%s (up to %d min)", p.site.Description, int(p.maxDuration.Minutes()))
}

// FindURL implements downloader.Provider. The returned link is https with the
// query string and fragment removed, which drops tracking parameters.
func (p *Provider) FindURL(text string) (string, bool) {
	for _, m := range linkRe.FindAllString(text, -1) {
		u, err := url.Parse(strings.TrimRight(m, ".,;:!?)]}'"))
		if err != nil || !p.site.Match(u) {
			continue
		}
		u.Scheme, u.User, u.RawQuery, u.Fragment = "https", nil, "", ""
		return u.String(), true
	}
	return "", false
}

// Download implements downloader.Provider.
func (p *Provider) Download(ctx context.Context, link string) (*downloader.Media, error) {
	if _, ok := p.FindURL(link); !ok {
		return nil, fmt.Errorf("not a %s link: %q", p.site.Name, link)
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

	meta, err := p.yt.Probe(ctx, link, extra...)
	if err != nil {
		return nil, err
	}
	if meta.IsLive {
		return nil, fmt.Errorf("%w: live streams are not supported", downloader.ErrNoMedia)
	}
	// Some sites don't report a duration; only enforce the limit when we know it.
	if d := time.Duration(meta.Duration * float64(time.Second)); d > p.maxDuration {
		return nil, fmt.Errorf("%w: %s (limit %s)", downloader.ErrTooLong, d.Round(time.Second), p.maxDuration)
	}

	file, err := p.yt.Fetch(ctx, link, ytdlp.Request{Format: formatSelector, Sort: formatSort, Extra: extra})
	if err != nil {
		return nil, err
	}
	media := &downloader.Media{Kind: downloader.KindVideo, Filename: file.Name, Data: file.Data}
	if !p.site.NoCaption {
		media.Title = meta.Title
	}
	return media, nil
}
