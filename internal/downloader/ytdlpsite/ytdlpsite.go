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
	"sync"
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
	previews    chan struct{} // preview lookup slots, see previewSlots
	cache       *previewCache
	previewWait time.Duration

	yt          *ytdlp.Client
	site        Site
	maxDuration time.Duration
	cookies     string
}

var (
	_ downloader.Provider        = (*Provider)(nil)
	_ downloader.Describer       = (*Provider)(nil)
	_ downloader.AudioDownloader = (*Provider)(nil)
)

const (
	// previewWait is how long an inline query waits for the metadata lookup
	// before answering with the URL-based row; the answer has to come quickly.
	previewWait = 6 * time.Second
	// previewLookupTimeout bounds a lookup, which keeps running in the
	// background after the query was answered, so the next query for the same
	// link gets the cover and title from the cache.
	previewLookupTimeout = 45 * time.Second
	// previewSlots bounds concurrent preview lookups, so a burst of inline
	// queries can't start an unbounded number of yt-dlp processes.
	previewSlots      = 3
	maxPreviewTitle   = 100
	maxCachedPreviews = 500
)

// rich is the part of a preview that needs the metadata lookup.
type rich struct {
	title, thumbnail string
}

// previewCache holds finished lookups and the ones still running, so that
// queries for the same link share one lookup.
type previewCache struct {
	mu       sync.Mutex
	done     map[string]rich
	inflight map[string]chan struct{}
}

// New creates a provider for site on top of a shared yt-dlp client.
func New(yt *ytdlp.Client, site Site, opts Options) *Provider {
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = DefaultMaxDuration
	}
	return &Provider{
		yt: yt, site: site, maxDuration: opts.MaxDuration, cookies: opts.Cookies,
		previews:    make(chan struct{}, previewSlots),
		cache:       &previewCache{done: map[string]rich{}, inflight: map[string]chan struct{}{}},
		previewWait: previewWait,
	}
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

// Preview implements downloader.Previewer. The row is worked out from the URL,
// then filled in with the real title and cover from a quick metadata lookup.
// If that lookup is slow, busy or fails, the URL-based row is used as is.
func (p *inlineProvider) Preview(ctx context.Context, link string) (*downloader.Preview, bool) {
	if _, ok := p.FindURL(link); !ok {
		return nil, false
	}
	u, err := url.Parse(link)
	if err != nil {
		return nil, false
	}
	prev := p.site.Preview(u)

	if p.yt == nil {
		return prev, true
	}
	if r, ok := p.rich(ctx, link); ok {
		if r.title != "" {
			prev.Title = r.title
		}
		prev.ThumbnailURL = r.thumbnail
	}
	return prev, true
}

// rich returns the cached lookup for link, or starts one (unless all preview
// slots are busy) and waits for it at most previewWait.
func (p *inlineProvider) rich(ctx context.Context, link string) (rich, bool) {
	c := p.cache
	c.mu.Lock()
	if r, ok := c.done[link]; ok {
		c.mu.Unlock()
		return r, true
	}
	ch, running := c.inflight[link]
	if !running {
		select {
		case p.previews <- struct{}{}:
		default:
			c.mu.Unlock()
			return rich{}, false // all slots busy: answer now rather than wait
		}
		ch = make(chan struct{})
		c.inflight[link] = ch
		go p.lookup(link, ch)
	}
	c.mu.Unlock()

	wait := time.NewTimer(p.previewWait)
	defer wait.Stop()
	select {
	case <-ch:
		c.mu.Lock()
		defer c.mu.Unlock()
		r, ok := c.done[link]
		return r, ok
	case <-wait.C:
	case <-ctx.Done():
	}
	return rich{}, false
}

// lookup fetches the metadata for link on its own clock, so it can finish
// after the query that started it has been answered. Only successful lookups
// are cached; a failed one is simply retried by a later query.
func (p *inlineProvider) lookup(link string, done chan struct{}) {
	defer func() { <-p.previews }()

	ctx, cancel := context.WithTimeout(context.Background(), previewLookupTimeout)
	defer cancel()
	meta, err := p.yt.Probe(ctx, link, p.cookieArgs()...)

	c := p.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		r := rich{title: truncate(strings.TrimSpace(meta.Title), maxPreviewTitle)}
		if strings.HasPrefix(meta.Thumbnail, "https://") {
			r.thumbnail = meta.Thumbnail
		}
		if len(c.done) >= maxCachedPreviews {
			clear(c.done) // it only saves time, never correctness
		}
		c.done[link] = r
	}
	delete(c.inflight, link)
	close(done)
}

func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes-1]) + "…"
}

func (p *Provider) cookieArgs() []string {
	if p.cookies == "" {
		return nil
	}
	return []string{"--cookies", p.cookies}
}

// Name implements downloader.Provider.
func (p *Provider) Name() string { return p.site.Name }

// Description implements downloader.Describer.
func (p *Provider) Description() string {
	return fmt.Sprintf(`%s (up to %d min); add "mp3" after the link for the sound`, p.site.Description, int(p.maxDuration.Minutes()))
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
	meta, release, err := p.prepare(ctx, link)
	if err != nil {
		return nil, err
	}
	defer release()

	file, err := p.yt.Fetch(ctx, link, ytdlp.Request{Format: formatSelector, Sort: formatSort, Extra: p.cookieArgs(), Info: meta})
	if err != nil {
		return nil, err
	}
	// The title is the caption in chat; inline messages are sent without one.
	return &downloader.Media{Kind: downloader.KindVideo, Filename: file.Name, Data: file.Data, Title: meta.Title}, nil
}

// DownloadAudio implements downloader.AudioDownloader: the video's sound as an
// MP3, with title, uploader and cover art embedded.
func (p *Provider) DownloadAudio(ctx context.Context, link string) (*downloader.Media, error) {
	meta, release, err := p.prepare(ctx, link)
	if err != nil {
		return nil, err
	}
	defer release()

	file, err := p.yt.Fetch(ctx, link, ytdlp.Request{Audio: true, Extra: p.cookieArgs(), Info: meta})
	if err != nil {
		return nil, err
	}
	return &downloader.Media{
		Kind:      downloader.KindAudio,
		Filename:  file.Name,
		Data:      file.Data,
		Title:     truncate(strings.TrimSpace(meta.Title), maxPreviewTitle),
		Performer: meta.Uploader,
		Duration:  int(meta.Duration),
	}, nil
}

// prepare validates the link, takes a yt-dlp slot and checks the metadata
// before anything is downloaded. The caller must call release.
func (p *Provider) prepare(ctx context.Context, link string) (meta *ytdlp.Info, release func(), err error) {
	if _, ok := p.FindURL(link); !ok {
		return nil, nil, fmt.Errorf("not a %s link: %q", p.site.Name, link)
	}

	release, err = p.yt.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}

	meta, err = p.yt.Probe(ctx, link, p.cookieArgs()...)
	if err != nil {
		release()
		return nil, nil, err
	}
	if meta.IsLive {
		release()
		return nil, nil, fmt.Errorf("%w: live streams are not supported", downloader.ErrNoMedia)
	}
	// Some sites don't report a duration; only enforce the limit when we know it.
	if d := time.Duration(meta.Duration * float64(time.Second)); d > p.maxDuration {
		release()
		return nil, nil, fmt.Errorf("%w: %s (limit %s)", downloader.ErrTooLong, d.Round(time.Second), p.maxDuration)
	}
	return meta, release, nil
}
