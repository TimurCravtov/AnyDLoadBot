// Package ytdlp wraps the yt-dlp command line tool (and ffmpeg) for providers
// whose sites are too volatile to scrape directly, like YouTube and Instagram.
package ytdlp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"anydloadbot/internal/downloader"
)

const (
	// DefaultConcurrency bounds simultaneous yt-dlp processes.
	DefaultConcurrency = 2

	maxStderr = 500
)

// unavailable lists substrings of yt-dlp errors that mean "this media can't be
// downloaded" rather than "something broke".
var unavailable = []string{
	"video unavailable", "private video", "this video is private", "members-only",
	"sign in to confirm your age", "age-restricted", "has been removed", "not available in your country",
	"live event will begin", "premieres in", "this content isn't available", "login required",
	"requested content is not available", "no video could be found",
}

// Runner executes yt-dlp with args and returns its stdout.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// Client runs yt-dlp. All providers share one Client, so the concurrency limit
// applies to the whole process, not to each site.
type Client struct {
	run Runner
	sem chan struct{}
}

// New returns a Client for the yt-dlp executable. It fails if yt-dlp or ffmpeg
// is not installed.
func New(bin string, concurrency int) (*Client, error) {
	if bin == "" {
		bin = "yt-dlp"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("yt-dlp not found: %w", err)
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg not found (needed to merge streams and extract audio): %w", err)
	}
	return NewWithRunner(execRunner(path), concurrency), nil
}

// NewWithRunner returns a Client that uses run instead of the real executable.
func NewWithRunner(run Runner, concurrency int) *Client {
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	return &Client{run: run, sem: make(chan struct{}, concurrency)}
}

func execRunner(bin string) Runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > maxStderr {
				msg = msg[len(msg)-maxStderr:]
			}
			return nil, fmt.Errorf("yt-dlp: %w: %s", err, msg)
		}
		return stdout.Bytes(), nil
	}
}

// Acquire reserves one of the Client's process slots, waiting if all are busy.
// Call the returned function when done. Providers hold a slot across a probe
// and the download that follows it.
func (c *Client) Acquire(ctx context.Context) (release func(), err error) {
	select {
	case c.sem <- struct{}{}:
		return func() { <-c.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Info is the part of yt-dlp's metadata the providers use.
type Info struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Uploader string  `json:"uploader"`
	Duration float64 `json:"duration"` // seconds; 0 if unknown
	IsLive   bool    `json:"is_live"`
}

// Probe fetches a link's metadata without downloading it. extra are additional
// yt-dlp arguments, such as --cookies.
func (c *Client) Probe(ctx context.Context, link string, extra ...string) (*Info, error) {
	args := append([]string{"--dump-single-json", "--no-download", "--no-playlist", "--no-warnings"}, extra...)
	out, err := c.run(ctx, append(args, "--", link)...)
	if err != nil {
		return nil, Classify(err)
	}
	var info Info
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("parse yt-dlp metadata: %w", err)
	}
	return &info, nil
}

// Request describes what to download.
type Request struct {
	// Format and Sort are yt-dlp's -f and -S values for video downloads.
	Format string
	Sort   string
	// Audio extracts an MP3 with the title, artist and cover art embedded.
	Audio bool
	// Extra are additional yt-dlp arguments, such as --cookies.
	Extra []string
}

// File is a downloaded file.
type File struct {
	Name string
	Data []byte
}

// Fetch downloads link into memory. It returns downloader.ErrTooLarge if no
// format fits in downloader.MaxSize.
func (c *Client) Fetch(ctx context.Context, link string, req Request) (*File, error) {
	dir, err := os.MkdirTemp("", "ytdl-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	args := []string{
		"--no-playlist", "--no-warnings", "--no-progress", "--no-mtime", "--restrict-filenames",
		"--max-filesize", "50M",
	}
	if req.Audio {
		args = append(args,
			"-f", "bestaudio/best", "-x", "--audio-format", "mp3", "--audio-quality", "192K",
			"--embed-metadata", "--embed-thumbnail", "--convert-thumbnails", "jpg")
	} else {
		args = append(args, "-f", req.Format, "-S", req.Sort, "--merge-output-format", "mp4")
	}
	args = append(args, req.Extra...)
	args = append(args,
		"-P", dir, "-o", "%(id)s.%(ext)s",
		"--no-simulate", "--print", "after_move:filepath",
		"--", link)

	out, err := c.run(ctx, args...)
	if err != nil {
		return nil, Classify(err)
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
	return &File{Name: filepath.Base(file), Data: data}, nil
}

// Classify turns yt-dlp failures about the media itself into downloader.ErrNoMedia
// so the user gets a clear message; anything else is returned unchanged.
func Classify(err error) error {
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
