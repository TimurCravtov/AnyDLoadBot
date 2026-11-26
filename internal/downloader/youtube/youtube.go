// Package youtube is a downloader.Provider for YouTube videos and MP3 audio. It
// uses the shared yt-dlp client (yt-dlp tracks YouTube's frequent changes).
package youtube

import (
	"context"
	"encoding/json"
	"errors"
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

	oembedURL     = "https://www.youtube.com/oembed"
	oembedTimeout = 3 * time.Second
	maxOEmbedSize = 64 << 10

	// Size limits sit below downloader.MaxSize to leave room for container overhead.
	// Prefer the best streams that fit; fall back to the smallest ones. yt-dlp's
	// --max-filesize is the hard stop when sizes are unknown up front.
	formatSelector = "bv*[filesize<40M]+ba/b[filesize<48M]/bv*[filesize_approx<40M]+ba/b[filesize_approx<48M]/wv*+wa/w"
	formatSort     = "res:720,ext:mp4:m4a"
)

var (
	linkRe  = regexp.MustCompile(`https?://[^\s<>"]+`)
	videoID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
)

// Options configures a Provider.
type Options struct {
	// MaxDuration is the longest allowed video (default DefaultMaxDuration).
	MaxDuration time.Duration
}

// Provider downloads YouTube videos and audio.
type Provider struct {
	yt          *ytdlp.Client
	maxDuration time.Duration

	fetch      *downloader.Fetcher // oEmbed lookups for inline previews
	oembedBase string
}

var (
	_ downloader.Provider        = (*Provider)(nil)
	_ downloader.Previewer       = (*Provider)(nil)
	_ downloader.AudioDownloader = (*Provider)(nil)
)

// New creates a YouTube provider on top of a shared yt-dlp client.
func New(yt *ytdlp.Client, opts Options) *Provider {
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = DefaultMaxDuration
	}
	return &Provider{
		yt:          yt,
		maxDuration: opts.MaxDuration,
		fetch: downloader.NewFetcher(func(host string) bool {
			return host == "youtube.com" || strings.HasSuffix(host, ".youtube.com")
		}),
		oembedBase: oembedURL,
	}
}

// Name implements downloader.Provider.
func (*Provider) Name() string { return "YouTube" }

// Description implements downloader.Describer.
func (p *Provider) Description() string {
	return fmt.Sprintf("videos and Shorts up to %d min; add \"mp3\" after the link for audio with title and cover", int(p.maxDuration.Minutes()))
}

// FindURL implements downloader.Provider. The returned link is canonical
// (https://www.youtube.com/watch?v=ID), so playlist and tracking parameters are dropped.
func (*Provider) FindURL(text string) (string, bool) {
	for _, m := range linkRe.FindAllString(text, -1) {
		if id, ok := parseVideoID(strings.TrimRight(m, ".,;:!?)]}'")); ok {
			return "https://www.youtube.com/watch?v=" + id, true
		}
	}
	return "", false
}

// Preview implements downloader.Previewer. The thumbnail is derived from the
// video ID; the title and channel come from YouTube's public oEmbed endpoint,
// falling back to generic text if that request fails.
func (p *Provider) Preview(ctx context.Context, link string) (*downloader.Preview, bool) {
	id, ok := parseVideoID(link)
	if !ok {
		return nil, false
	}

	prev := &downloader.Preview{
		ThumbnailURL: "https://i.ytimg.com/vi/" + id + "/hqdefault.jpg",
		Title:        "YouTube video",
		Description:  "Tap to download",
	}
	if o, err := p.oembed(ctx, link); err == nil && o.Title != "" {
		prev.Title = o.Title
		if o.Author != "" {
			prev.Description = o.Author + " · Tap to download"
		}
	}
	return prev, true
}

type oembedResponse struct {
	Title  string `json:"title"`
	Author string `json:"author_name"`
}

func (p *Provider) oembed(ctx context.Context, link string) (*oembedResponse, error) {
	if p.fetch == nil {
		return nil, errors.New("no HTTP fetcher")
	}
	ctx, cancel := context.WithTimeout(ctx, oembedTimeout)
	defer cancel()

	body, err := p.fetch.Get(ctx, p.oembedBase+"?format=json&url="+url.QueryEscape(link), maxOEmbedSize, "application/json")
	if err != nil {
		return nil, err
	}
	var o oembedResponse
	if err := json.Unmarshal(body, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

func parseVideoID(link string) (string, bool) {
	u, err := url.Parse(link)
	if err != nil {
		return "", false
	}

	var id string
	switch strings.ToLower(u.Hostname()) {
	case "youtu.be":
		id = strings.TrimPrefix(u.Path, "/")
	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com":
		switch {
		case u.Path == "/watch":
			id = u.Query().Get("v")
		default:
			for _, prefix := range []string{"/shorts/", "/live/", "/embed/"} {
				if rest, ok := strings.CutPrefix(u.Path, prefix); ok {
					id = rest
					break
				}
			}
		}
	}
	id = strings.TrimSuffix(id, "/")
	return id, videoID.MatchString(id)
}

// Download implements downloader.Provider.
func (p *Provider) Download(ctx context.Context, link string) (*downloader.Media, error) {
	meta, release, err := p.prepare(ctx, link)
	if err != nil {
		return nil, err
	}
	defer release()

	file, err := p.yt.Fetch(ctx, link, ytdlp.Request{Format: formatSelector, Sort: formatSort, Info: meta})
	if err != nil {
		return nil, err
	}
	return &downloader.Media{Kind: downloader.KindVideo, Filename: file.Name, Data: file.Data, Title: meta.Title}, nil
}

// DownloadAudio implements downloader.AudioDownloader. The MP3 has the title,
// channel name and thumbnail embedded as ID3 tags and cover art.
func (p *Provider) DownloadAudio(ctx context.Context, link string) (*downloader.Media, error) {
	meta, release, err := p.prepare(ctx, link)
	if err != nil {
		return nil, err
	}
	defer release()

	file, err := p.yt.Fetch(ctx, link, ytdlp.Request{Audio: true, Info: meta})
	if err != nil {
		return nil, err
	}
	return &downloader.Media{
		Kind:      downloader.KindAudio,
		Filename:  file.Name,
		Data:      file.Data,
		Title:     meta.Title,
		Performer: meta.Uploader,
		Duration:  int(meta.Duration),
	}, nil
}

// prepare validates the link, takes a yt-dlp slot and checks the video's
// metadata before anything is downloaded. The caller must call release.
func (p *Provider) prepare(ctx context.Context, link string) (meta *ytdlp.Info, release func(), err error) {
	if _, ok := p.FindURL(link); !ok {
		return nil, nil, fmt.Errorf("not a YouTube link: %q", link)
	}

	release, err = p.yt.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}

	meta, err = p.yt.Probe(ctx, link)
	if err != nil {
		release()
		return nil, nil, err
	}
	if meta.IsLive || meta.Duration <= 0 {
		release()
		return nil, nil, fmt.Errorf("%w: live streams are not supported", downloader.ErrNoMedia)
	}
	if d := time.Duration(meta.Duration * float64(time.Second)); d > p.maxDuration {
		release()
		return nil, nil, fmt.Errorf("%w: %s (limit %s)", downloader.ErrTooLong, d.Round(time.Second), p.maxDuration)
	}
	return meta, release, nil
}
