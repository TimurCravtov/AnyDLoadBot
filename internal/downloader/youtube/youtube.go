// Package youtube is a downloader.Provider for YouTube videos. It shells out to
// yt-dlp (which tracks YouTube's frequent changes) and ffmpeg (to merge streams).
package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"anydloadbot/internal/downloader"
)

const (
	// DefaultMaxDuration is the longest video the provider will download.
	DefaultMaxDuration = 15 * time.Minute

	defaultConcurrency = 2
	maxStderr          = 500

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

	// Substrings of yt-dlp errors that mean "this video can't be downloaded"
	// rather than "something broke".
	unavailable = []string{
		"video unavailable", "private video", "this video is private", "members-only",
		"sign in to confirm your age", "age-restricted", "has been removed", "not available in your country",
		"live event will begin", "premieres in",
	}
)

// Options configures a Provider.
type Options struct {
	// YtDlp is the yt-dlp executable (default "yt-dlp").
	YtDlp string
	// MaxDuration is the longest allowed video (default DefaultMaxDuration).
	MaxDuration time.Duration
	// Concurrency bounds simultaneous yt-dlp processes (default 2).
	Concurrency int
}

// runner executes yt-dlp with args and returns its stdout.
type runner func(ctx context.Context, args ...string) ([]byte, error)

// Provider downloads YouTube videos.
type Provider struct {
	run         runner
	maxDuration time.Duration
	sem         chan struct{}

	fetch      *downloader.Fetcher // oEmbed lookups for inline previews
	oembedBase string
}

var (
	_ downloader.Provider  = (*Provider)(nil)
	_ downloader.Previewer = (*Provider)(nil)
)

// New creates a YouTube provider. It fails if yt-dlp or ffmpeg is not installed.
func New(opts Options) (*Provider, error) {
	if opts.YtDlp == "" {
		opts.YtDlp = "yt-dlp"
	}
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = DefaultMaxDuration
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultConcurrency
	}

	bin, err := exec.LookPath(opts.YtDlp)
	if err != nil {
		return nil, fmt.Errorf("yt-dlp not found: %w", err)
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg not found (needed to merge video and audio): %w", err)
	}

	return &Provider{
		run:         execRunner(bin),
		maxDuration: opts.MaxDuration,
		sem:         make(chan struct{}, opts.Concurrency),
		fetch: downloader.NewFetcher(func(host string) bool {
			return host == "youtube.com" || strings.HasSuffix(host, ".youtube.com")
		}),
		oembedBase: oembedURL,
	}, nil
}

func execRunner(bin string) runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > maxStderr {
				msg = msg[len(msg)-maxStderr:]
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("yt-dlp: %w: %s", err, msg)
		}
		return stdout.Bytes(), nil
	}
}

// Name implements downloader.Provider.
func (*Provider) Name() string { return "YouTube" }

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

type metadata struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Duration float64 `json:"duration"`
	IsLive   bool    `json:"is_live"`
}

// Download implements downloader.Provider.
func (p *Provider) Download(ctx context.Context, link string) (*downloader.Media, error) {
	if _, ok := p.FindURL(link); !ok {
		return nil, fmt.Errorf("not a YouTube link: %q", link)
	}

	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	meta, err := p.probe(ctx, link)
	if err != nil {
		return nil, err
	}
	if meta.IsLive || meta.Duration <= 0 {
		return nil, fmt.Errorf("%w: live streams are not supported", downloader.ErrNoMedia)
	}
	if time.Duration(meta.Duration*float64(time.Second)) > p.maxDuration {
		return nil, fmt.Errorf("%w: %s (limit %s)", downloader.ErrTooLong,
			time.Duration(meta.Duration*float64(time.Second)).Round(time.Second), p.maxDuration)
	}

	dir, err := os.MkdirTemp("", "ytdl-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	out, err := p.run(ctx,
		"--no-playlist", "--no-warnings", "--no-progress", "--no-mtime", "--restrict-filenames",
		"--max-filesize", "50M",
		"-f", formatSelector, "-S", formatSort, "--merge-output-format", "mp4",
		"-P", dir, "-o", "%(id)s.%(ext)s",
		"--no-simulate", "--print", "after_move:filepath",
		"--", link)
	if err != nil {
		return nil, classify(err)
	}

	file := lastLine(out)
	if file == "" {
		// yt-dlp exits 0 without a file when --max-filesize rejects every format.
		return nil, downloader.ErrTooLarge
	}
	// Only trust paths inside our temp dir.
	if rel, err := filepath.Rel(dir, file); err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("unexpected output path %q", file)
	}

	info, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("stat downloaded file: %w", err)
	}
	if info.Size() > downloader.MaxSize {
		return nil, downloader.ErrTooLarge
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read downloaded file: %w", err)
	}
	return &downloader.Media{Filename: filepath.Base(file), Data: data, Title: meta.Title}, nil
}

func (p *Provider) probe(ctx context.Context, link string) (*metadata, error) {
	out, err := p.run(ctx, "--dump-single-json", "--no-download", "--no-playlist", "--no-warnings", "--", link)
	if err != nil {
		return nil, classify(err)
	}
	var m metadata
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("parse yt-dlp metadata: %w", err)
	}
	return &m, nil
}

// classify turns yt-dlp failures about the video itself into ErrNoMedia so the
// user gets a clear message; anything else is returned as an internal error.
func classify(err error) error {
	msg := strings.ToLower(err.Error())
	for _, s := range unavailable {
		if strings.Contains(msg, s) {
			return fmt.Errorf("%w: %v", downloader.ErrNoMedia, err)
		}
	}
	return err
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
