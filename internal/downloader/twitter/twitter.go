// Package twitter is a downloader.Provider for videos, GIFs and images in posts
// on X (twitter.com / x.com). It reads the same public "syndication" JSON that
// embedded tweets use, so it needs no login, yt-dlp or ffmpeg.
package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"anydloadbot/internal/downloader"
)

const (
	syndicationURL = "https://cdn.syndication.twimg.com/tweet-result"
	// The endpoint only checks that a token is present. Embeds compute one from
	// the tweet ID, but any non-empty value is accepted.
	syndicationToken = "x"
	maxJSONSize      = 1 << 20

	// Telegram fetches inline videos itself and is stricter about size than the
	// upload API, so inline picks a smaller variant than a chat download does.
	inlineMaxBytes = 20 << 20
	chatMaxBytes   = 48 << 20
)

var (
	linkRe  = regexp.MustCompile(`https?://[^\s<>"]+`)
	tweetID = regexp.MustCompile(`^[0-9]{1,25}$`)
	tcoLink = regexp.MustCompile(`\s*https://t\.co/\w+\s*$`)

	hosts = map[string]bool{
		"x.com": true, "www.x.com": true, "twitter.com": true, "www.twitter.com": true, "mobile.twitter.com": true,
		// Popular link-preview mirrors; people paste these a lot.
		"fixupx.com": true, "fxtwitter.com": true, "vxtwitter.com": true,
	}
)

// Provider downloads media from X posts.
type Provider struct {
	fetch    *downloader.Fetcher
	endpoint string
}

var (
	_ downloader.Provider = (*Provider)(nil)
	_ downloader.Resolver = (*Provider)(nil)
)

// New creates an X provider.
func New() *Provider {
	return &Provider{
		endpoint: syndicationURL,
		fetch: downloader.NewFetcher(func(host string) bool {
			host = strings.ToLower(host)
			return host == "twimg.com" || strings.HasSuffix(host, ".twimg.com")
		}),
	}
}

// Name implements downloader.Provider.
func (*Provider) Name() string { return "X" }

// Description implements downloader.Describer.
func (*Provider) Description() string {
	return "videos, GIFs and images from posts (x.com and twitter.com)"
}

// FindURL implements downloader.Provider. The returned link is canonical
// (https://x.com/i/status/ID).
func (*Provider) FindURL(text string) (string, bool) {
	for _, m := range linkRe.FindAllString(text, -1) {
		if id, ok := parseTweetID(strings.TrimRight(m, ".,;:!?)]}'")); ok {
			return "https://x.com/i/status/" + id, true
		}
	}
	return "", false
}

func parseTweetID(link string) (string, bool) {
	u, err := url.Parse(link)
	if err != nil || !hosts[strings.ToLower(u.Hostname())] {
		return "", false
	}
	// /<user>/status/<id>[/photo/1|/video/1], /i/status/<id>, /<user>/statuses/<id>
	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	if len(parts) >= 3 && (parts[1] == "status" || parts[1] == "statuses") && tweetID.MatchString(parts[2]) {
		return parts[2], true
	}
	return "", false
}

// tweet is the part of the syndication JSON the provider uses.
type tweet struct {
	Typename string `json:"__typename"`
	Text     string `json:"text"`
	User     struct {
		ScreenName string `json:"screen_name"`
	} `json:"user"`
	MediaDetails []struct {
		Type          string `json:"type"` // photo, video, animated_gif
		MediaURLHTTPS string `json:"media_url_https"`
		VideoInfo     struct {
			DurationMillis int `json:"duration_millis"`
			Variants       []struct {
				ContentType string `json:"content_type"`
				Bitrate     int    `json:"bitrate"`
				URL         string `json:"url"`
			} `json:"variants"`
		} `json:"video_info"`
	} `json:"mediaDetails"`
}

// Resolve implements downloader.Resolver, picking a video variant small enough
// for Telegram to fetch by itself.
func (p *Provider) Resolve(ctx context.Context, link string) (*downloader.Source, error) {
	return p.resolve(ctx, link, inlineMaxBytes)
}

func (p *Provider) resolve(ctx context.Context, link string, maxBytes int64) (*downloader.Source, error) {
	id, ok := parseTweetID(link)
	if !ok {
		return nil, fmt.Errorf("not an X link: %q", link)
	}

	body, err := p.fetch.Get(ctx, p.endpoint+"?id="+id+"&token="+syndicationToken+"&lang=en", maxJSONSize, "application/json")
	if err != nil {
		// A deleted or protected post answers 404.
		return nil, fmt.Errorf("%w: fetch post: %v", downloader.ErrNoMedia, err)
	}
	var t tweet
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("parse post: %w", err)
	}
	if t.Typename == "TweetTombstone" || len(t.MediaDetails) == 0 {
		return nil, downloader.ErrNoMedia
	}

	title := caption(t.User.ScreenName, t.Text)
	m := t.MediaDetails[0]

	if m.Type == "photo" {
		return &downloader.Source{
			Kind:         downloader.KindPhoto,
			URL:          withQuery(m.MediaURLHTTPS, "large"),
			ThumbnailURL: withQuery(m.MediaURLHTTPS, "small"),
			Title:        title,
		}, nil
	}

	// Video or GIF: the highest-bitrate MP4 whose estimated size fits; failing
	// that, the smallest one (the download then reports ErrTooLarge if it's over).
	var best, smallest string
	var bestRate, smallestRate int
	for _, v := range m.VideoInfo.Variants {
		if v.ContentType != "video/mp4" {
			continue // skip the HLS playlist
		}
		if smallest == "" || v.Bitrate < smallestRate {
			smallest, smallestRate = v.URL, v.Bitrate
		}
		size := int64(v.Bitrate) / 8 * int64(m.VideoInfo.DurationMillis) / 1000
		if size <= maxBytes && (best == "" || v.Bitrate > bestRate) {
			best, bestRate = v.URL, v.Bitrate
		}
	}
	if best == "" {
		best = smallest
	}
	if best == "" {
		return nil, downloader.ErrNoMedia
	}
	return &downloader.Source{
		Kind:         downloader.KindVideo,
		URL:          best,
		ThumbnailURL: m.MediaURLHTTPS, // the video's poster frame (JPEG)
		Title:        title,
		MimeType:     "video/mp4",
	}, nil
}

// Download implements downloader.Provider.
func (p *Provider) Download(ctx context.Context, link string) (*downloader.Media, error) {
	src, err := p.resolve(ctx, link, chatMaxBytes)
	if err != nil {
		return nil, err
	}

	data, err := p.fetch.Get(ctx, src.URL, downloader.MaxSize, "*/*")
	if err != nil {
		return nil, fmt.Errorf("download media: %w", err)
	}

	u, _ := url.Parse(src.URL)
	return &downloader.Media{Kind: src.Kind, Filename: path.Base(u.Path), Data: data, Title: src.Title}, nil
}

// withQuery sets pbs.twimg.com's size selector on an image URL.
func withQuery(rawURL, name string) string {
	return rawURL + "?name=" + name
}

// caption is "@user: text" with the trailing t.co link (the media itself) removed.
func caption(user, text string) string {
	text = strings.TrimSpace(tcoLink.ReplaceAllString(text, ""))
	switch {
	case user != "" && text != "":
		return "@" + user + ": " + text
	case user != "":
		return "@" + user
	default:
		return text
	}
}
