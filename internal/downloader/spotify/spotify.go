// Package spotify is a downloader.Provider for Spotify tracks. Spotify's own
// audio streams are DRM-protected, so the provider reads the track's metadata
// (title, artist, duration, cover art) from Spotify's public embed page and
// fetches the matching audio as MP3 through the shared yt-dlp client.
package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"anydloadbot/internal/downloader"
	"anydloadbot/internal/downloader/ytdlp"
)

const (
	// DefaultMaxDuration is the longest track the provider will download.
	DefaultMaxDuration = 15 * time.Minute

	siteBase       = "https://open.spotify.com"
	previewTimeout = 3 * time.Second
	maxEmbedSize   = 512 << 10
)

var (
	linkRe     = regexp.MustCompile(`https?://[^\s<>"]+`)
	trackID    = regexp.MustCompile(`^[A-Za-z0-9]{22}$`)
	nextDataRe = regexp.MustCompile(`<script id="__NEXT_DATA__"[^>]*>([^<]+)</script>`)
)

// Options configures a Provider.
type Options struct {
	// MaxDuration is the longest allowed track (default DefaultMaxDuration).
	MaxDuration time.Duration
	// Extra are yt-dlp arguments added to every YouTube search and download,
	// e.g. ytdlp.DirectArgs to bypass the shared yt-dlp proxy.
	Extra []string
}

// Provider downloads Spotify tracks as MP3 audio.
type Provider struct {
	yt          *ytdlp.Client
	maxDuration time.Duration
	extra       []string
	fetch       *downloader.Fetcher
	siteBase    string
}

var (
	_ downloader.Provider  = (*Provider)(nil)
	_ downloader.Previewer = (*Provider)(nil)
)

// New creates a Spotify provider on top of a shared yt-dlp client.
func New(yt *ytdlp.Client, opts Options) *Provider {
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = DefaultMaxDuration
	}
	return &Provider{
		yt:          yt,
		maxDuration: opts.MaxDuration,
		extra:       opts.Extra,
		siteBase:    siteBase,
		fetch:       downloader.NewFetcher(isSpotifyHost),
	}
}

// Name implements downloader.Provider.
func (*Provider) Name() string { return "Spotify" }

func isSpotifyHost(host string) bool {
	host = strings.ToLower(host)
	return host == "open.spotify.com" || host == "play.spotify.com"
}

// FindURL implements downloader.Provider. The returned link is canonical
// (https://open.spotify.com/track/ID).
func (*Provider) FindURL(text string) (string, bool) {
	for _, m := range linkRe.FindAllString(text, -1) {
		if id, ok := parseTrackID(strings.TrimRight(m, ".,;:!?)]}'")); ok {
			return siteBase + "/track/" + id, true
		}
	}
	return "", false
}

func parseTrackID(link string) (string, bool) {
	u, err := url.Parse(link)
	if err != nil || !isSpotifyHost(u.Hostname()) {
		return "", false
	}
	// /track/<id>, /intl-<locale>/track/<id>, /embed/track/<id>
	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	if len(parts) >= 2 && (strings.HasPrefix(parts[0], "intl-") || parts[0] == "embed") {
		parts = parts[1:]
	}
	if len(parts) == 2 && parts[0] == "track" && trackID.MatchString(parts[1]) {
		return parts[1], true
	}
	return "", false
}

// Preview implements downloader.Previewer.
func (p *Provider) Preview(ctx context.Context, link string) (*downloader.Preview, bool) {
	id, ok := parseTrackID(link)
	if !ok {
		return nil, false
	}

	ctx, cancel := context.WithTimeout(ctx, previewTimeout)
	defer cancel()

	t, err := p.track(ctx, id)
	if err != nil || t.ThumbnailURL == "" {
		return nil, false
	}
	desc := "Tap to download"
	if t.Artist != "" {
		desc = t.Artist + " · Tap to download"
	}
	return &downloader.Preview{
		ThumbnailURL: t.ThumbnailURL,
		Title:        t.Title,
		Description:  desc,
	}, true
}

// Download implements downloader.Provider.
func (p *Provider) Download(ctx context.Context, link string) (*downloader.Media, error) {
	id, ok := parseTrackID(link)
	if !ok {
		return nil, fmt.Errorf("not a Spotify link: %q", link)
	}

	t, err := p.track(ctx, id)
	if err != nil {
		return nil, err
	}
	if d := time.Duration(t.Duration) * time.Millisecond; d > p.maxDuration {
		return nil, fmt.Errorf("%w: %s (limit %s)", downloader.ErrTooLong, d.Round(time.Second), p.maxDuration)
	}

	release, err := p.yt.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	query := t.Title
	if t.Artist != "" {
		query = t.Artist + " - " + t.Title
	}
	file, err := p.yt.Fetch(ctx, "ytsearch1:"+query, ytdlp.Request{Audio: true, Extra: append(slices.Clone(p.extra), ytdlp.FastYouTube...)})
	if err != nil && ctx.Err() == nil && !errors.Is(err, downloader.ErrTooLarge) && !errors.Is(err, downloader.ErrNoMedia) {
		// The fast YouTube lookup didn't work out: the default one.
		file, err = p.yt.Fetch(ctx, "ytsearch1:"+query, ytdlp.Request{Audio: true, Extra: p.extra})
	}
	if err != nil {
		return nil, err
	}
	return &downloader.Media{
		Kind:      downloader.KindAudio,
		Filename:  file.Name,
		Data:      file.Data,
		Title:     t.Title,
		Performer: t.Artist,
		Duration:  t.Duration / 1000,
	}, nil
}

type track struct {
	Title        string
	Artist       string
	Duration     int // milliseconds
	ThumbnailURL string
}

type embedPage struct {
	Props struct {
		PageProps struct {
			State struct {
				Data struct {
					Entity struct {
						Title    string `json:"title"`
						Name     string `json:"name"`
						Duration int    `json:"duration"`
						Artists  []struct {
							Name string `json:"name"`
						} `json:"artists"`
						VisualIdentity struct {
							Image []struct {
								URL      string `json:"url"`
								MaxWidth int    `json:"maxWidth"`
							} `json:"image"`
						} `json:"visualIdentity"`
					} `json:"entity"`
				} `json:"data"`
			} `json:"state"`
		} `json:"pageProps"`
	} `json:"props"`
}

func (p *Provider) track(ctx context.Context, id string) (*track, error) {
	if p.fetch == nil {
		return nil, errors.New("no HTTP fetcher")
	}
	page, err := p.fetch.Get(ctx, p.siteBase+"/embed/track/"+id, maxEmbedSize, "text/html")
	if err != nil {
		return nil, fmt.Errorf("fetch track page: %w", err)
	}

	m := nextDataRe.FindSubmatch(page)
	if m == nil {
		return nil, downloader.ErrNoMedia
	}
	var data embedPage
	if err := json.Unmarshal(m[1], &data); err != nil {
		return nil, fmt.Errorf("parse track metadata: %w", err)
	}

	e := data.Props.PageProps.State.Data.Entity
	title := e.Title
	if title == "" {
		title = e.Name
	}
	if title == "" {
		return nil, downloader.ErrNoMedia
	}

	var artists []string
	for _, a := range e.Artists {
		if a.Name != "" {
			artists = append(artists, a.Name)
		}
	}

	var thumb string
	var bestWidth int
	for _, img := range e.VisualIdentity.Image {
		if img.URL != "" && (thumb == "" || img.MaxWidth > bestWidth) {
			thumb, bestWidth = img.URL, img.MaxWidth
		}
	}

	return &track{
		Title:        title,
		Artist:       strings.Join(artists, ", "),
		Duration:     e.Duration,
		ThumbnailURL: thumb,
	}, nil
}
