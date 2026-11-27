// Package ytdlp wraps the yt-dlp command line tool (and ffmpeg) for providers
// whose sites are too volatile to scrape directly, like YouTube and Instagram.
package ytdlp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	"requested content is not available", "no video could be found", "ip address is blocked",
	// ffmpeg, when asked for audio from a video that has none.
	"does not contain any stream", "unable to obtain file audio codec",
}

// Runner executes yt-dlp with args and returns its stdout.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// Client runs yt-dlp. All providers share one Client, so the concurrency limit
// applies to the whole process, not to each site.
type Client struct {
	run      Runner
	sem      chan struct{}
	defaults []string // added to every run, before per-request arguments

	// Set by KeepFiles: downloads stay on disk in dir, up to maxSize bytes.
	dir     string
	maxSize int64
}

// Options are settings that apply to every yt-dlp run.
type Options struct {
	// Cookies is a Netscape-format cookies.txt used for every site. Sites that
	// block anonymous or datacenter traffic (YouTube) often need it. It can hold
	// cookies for several sites; yt-dlp only sends the ones that match.
	Cookies string
	// Proxy routes every request through a proxy, e.g. http://host:3128 or
	// socks5://host:1080. Useful when a site blocks the server's IP address.
	Proxy string
	// POTServer is the URL of a bgutil PO token server, e.g. http://pot:4416.
	// YouTube withholds the streams from logged-in servers that can't show a
	// PO token ("Requested format is not available"); yt-dlp's bgutil plugin
	// (installed in the Docker image) gets the tokens from this server.
	POTServer string
	// Server is the command line of the yt-dlp fork server
	// (deploy/ytdlp-server/server.py), e.g. ["/opt/yt-dlp/bin/python",
	// "/opt/ytdlp-server/server.py"]. It loads yt-dlp once, which saves about
	// two seconds of start-up on every run. Runs fall back to starting yt-dlp
	// normally whenever the server isn't up. Only on Unix; nil means unused.
	Server []string
}

func (o Options) args() []string {
	var args []string
	if o.Cookies != "" {
		args = append(args, "--cookies", o.Cookies)
	}
	if o.Proxy != "" {
		args = append(args, "--proxy", o.Proxy)
	}
	if o.POTServer != "" {
		args = append(args, "--extractor-args", "youtubepot-bgutilhttp:base_url="+o.POTServer)
	}
	return args
}

// New returns a Client for the yt-dlp executable. It fails if yt-dlp or ffmpeg
// is not installed.
func New(bin string, concurrency int, opts Options) (*Client, error) {
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
	run := execRunner(path)
	if len(opts.Server) > 0 {
		run = startServer(opts.Server, run)
	}
	return NewWithRunner(run, concurrency, opts.args()...), nil
}

// NewWithRunner returns a Client that uses run instead of the real executable.
// defaults are yt-dlp arguments added to every run.
func NewWithRunner(run Runner, concurrency int, defaults ...string) *Client {
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	return &Client{run: run, sem: make(chan struct{}, concurrency), defaults: defaults}
}

// KeepFiles makes Fetch leave downloads on disk in dir (File.Path) instead of
// reading them into memory, with maxSize as the size limit instead of
// downloader.MaxSize; videos over it are compressed to fit. dir must be
// readable by whoever sends the files on (a local Bot API server). Leftovers
// of earlier runs in dir (from a crash, say) are removed.
func (c *Client) KeepFiles(dir string, maxSize int64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("media dir: %w", err)
	}
	old, _ := filepath.Glob(filepath.Join(dir, "ytdl-*"))
	for _, d := range old {
		_ = os.RemoveAll(d)
	}
	c.dir, c.maxSize = dir, maxSize
	return nil
}

// MaxSize is the largest file Fetch returns.
func (c *Client) MaxSize() int64 {
	if c.dir != "" {
		return c.maxSize
	}
	return downloader.MaxSize
}

// exec runs yt-dlp with the client's default arguments followed by args. Any
// cookies file is replaced by a private temporary copy for the run: yt-dlp
// rewrites its cookies file on exit, so concurrent runs sharing one file could
// corrupt it, and a read-only mount (Docker) would make it fail.
func (c *Client) exec(ctx context.Context, args []string) ([]byte, error) {
	args, cleanup, err := copyCookies(append(slices.Clone(c.defaults), args...))
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return c.run(ctx, args...)
}

func copyCookies(args []string) (out []string, cleanup func(), err error) {
	var tmpFiles []string
	cleanup = func() {
		for _, f := range tmpFiles {
			_ = os.Remove(f)
		}
	}

	out = slices.Clone(args)
	for i := 0; i+1 < len(out); i++ {
		// Stop at "--": everything after it is the link, never an option.
		if out[i] == "--" {
			break
		}
		if out[i] != "--cookies" {
			continue
		}
		data, err := os.ReadFile(out[i+1])
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("read cookies file: %w", err)
		}
		f, err := os.CreateTemp("", "ytdl-cookies-*.txt") // created with mode 0600
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("copy cookies file: %w", err)
		}
		tmpFiles = append(tmpFiles, f.Name())
		_, werr := f.Write(data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			cleanup()
			return nil, nil, fmt.Errorf("copy cookies file: %w", werr)
		}
		out[i+1] = f.Name()
		i++
	}
	return out, cleanup, nil
}

func execRunner(bin string) Runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, bin, args...)
		outPipe, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		errPipe, err := cmd.StderrPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}

		stdout, stderr := drain(ctx, outPipe, errPipe)
		return result(ctx, cmd.Wait(), stdout, stderr)
	}
}

// drain reads yt-dlp's stdout and stderr until both are closed. It reads both
// while yt-dlp runs, so progress is reported live. (Where yt-dlp prints
// progress depends on its quiet mode, so it checks both.)
func drain(ctx context.Context, outPipe, errPipe io.Reader) (stdout, stderr *bytes.Buffer) {
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	var downloading atomic.Bool // shared: progress may come on either stream
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); scanOutput(ctx, outPipe, stdout, &downloading) }()
	go func() { defer wg.Done(); scanOutput(ctx, errPipe, stderr, &downloading) }()
	wg.Wait()
	return stdout, stderr
}

// result turns a finished run into the Runner's return values.
func result(ctx context.Context, runErr error, stdout, stderr *bytes.Buffer) ([]byte, error) {
	if runErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > maxStderr {
			msg = msg[len(msg)-maxStderr:]
		}
		return nil, fmt.Errorf("yt-dlp: %w: %s", runErr, msg)
	}
	return stdout.Bytes(), nil
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
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Uploader  string  `json:"uploader"`
	Thumbnail string  `json:"thumbnail"` // cover image URL, if the site has one
	Duration  float64 `json:"duration"`  // seconds; 0 if unknown
	IsLive    bool    `json:"is_live"`
}

// Probe fetches a link's metadata without downloading it. extra are additional
// yt-dlp arguments, such as --cookies.
func (c *Client) Probe(ctx context.Context, link string, extra ...string) (*Info, error) {
	args := append([]string{"--dump-single-json", "--no-download", "--no-playlist", "--no-warnings"}, extra...)
	out, err := c.exec(ctx, append(args, "--", link))
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
	// MaxDuration, if set, makes yt-dlp itself refuse live streams and media
	// longer than this before anything is downloaded (downloader.ErrNoMedia,
	// downloader.ErrTooLong), so the link needs no separate Probe first.
	MaxDuration time.Duration
	// AllowUnknownDuration lets media through MaxDuration whose duration the
	// site doesn't report. Otherwise such media counts as a live stream.
	AllowUnknownDuration bool
}

// File is a downloaded file and the metadata yt-dlp found for it.
type File struct {
	Name string
	Data []byte // the file, unless it was kept on disk (KeepFiles)
	Path string // the file on disk, if it was kept there; see Remove
	Info Info

	dir string // Path's temporary directory
}

// Remove deletes a file kept on disk. It is safe to call more than once.
func (f *File) Remove() {
	if f.dir != "" {
		_ = os.RemoveAll(f.dir)
	}
}

// Media is the file as a downloader.Media of kind. A file on disk is removed
// when the Media is closed.
func (f *File) Media(kind downloader.Kind) *downloader.Media {
	m := &downloader.Media{Kind: kind, Filename: f.Name, Data: f.Data, Path: f.Path}
	if f.Path != "" {
		m.Cleanup = f.Remove
	}
	return m
}

// Format size placeholders, replaced in Request.Format by sizes derived from
// the client's size limit: the largest video stream (leaving room for the
// audio) and the largest single file (leaving room for container overhead).
// With the in-memory limit of 50 MB they are 40M and 48M.
const (
	VideoSize = "{video-size}"
	FileSize  = "{file-size}"
)

func (c *Client) formatSizes(format string) string {
	mb := func(frac float64) string {
		return strconv.Itoa(int(float64(c.MaxSize())*frac)>>20) + "M"
	}
	return strings.NewReplacer(VideoSize, mb(0.8), FileSize, mb(0.96)).Replace(format)
}

// metaPrefix marks the metadata line Fetch has yt-dlp print.
const metaPrefix = "anydl-meta:"

// compressFactor is how much bigger than the size limit a video may be
// downloaded, to be compressed to fit afterwards.
const compressFactor = 4

// Fetch downloads link in a single yt-dlp run, into memory or, with
// KeepFiles, to disk. It returns downloader.ErrTooLarge if the file doesn't
// fit in MaxSize (a video kept on disk is compressed to fit first).
func (c *Client) Fetch(ctx context.Context, link string, req Request) (_ *File, err error) {
	dir, err := os.MkdirTemp(c.dir, "ytdl-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	keep := c.dir != ""
	defer func() {
		if !keep || err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	limit := c.MaxSize()
	maxDownload := limit
	if keep && !req.Audio {
		maxDownload = limit * compressFactor
	}

	args := []string{
		"--no-playlist", "--no-warnings", "--no-mtime", "--restrict-filenames",
		"--max-filesize", strconv.FormatInt(maxDownload>>20, 10) + "M",
	}
	args = append(args, progressArgs...)
	if req.Audio {
		args = append(args,
			"-f", "bestaudio/best", "-x", "--audio-format", "mp3", "--audio-quality", "192K",
			"--embed-metadata", "--embed-thumbnail", "--convert-thumbnails", "jpg")
	} else {
		args = append(args, "-f", c.formatSizes(req.Format), "-S", req.Sort, "--merge-output-format", "mp4")
	}
	if req.MaxDuration > 0 {
		limit := int(req.MaxDuration.Seconds())
		filter := fmt.Sprintf("!is_live & duration <= %d", limit)
		if req.AllowUnknownDuration {
			filter = fmt.Sprintf("!is_live & (!duration | duration <= %d)", limit)
		}
		args = append(args, "--match-filter", filter)
	}
	args = append(args, req.Extra...)
	args = append(args,
		"-P", dir, "-o", "%(id)s.%(ext)s",
		// The metadata is printed right after the lookup, before the match
		// filter, so a refused video still says why.
		"--print", "pre_process:"+metaPrefix+"%(.{id,title,uploader,thumbnail,duration,is_live})j",
		"--no-simulate", "--print", "after_move:filepath",
		"--", link)

	out, err := c.exec(ctx, args)
	if err != nil {
		return nil, Classify(err)
	}

	var meta Info
	file := ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if m, ok := strings.CutPrefix(line, metaPrefix); ok {
			_ = json.Unmarshal([]byte(m), &meta) // only used for titles and refusals
			continue
		}
		if line != "" {
			file = line
		}
	}

	if file == "" {
		// yt-dlp exits 0 without a file when the match filter or --max-filesize
		// refuses the media.
		d := time.Duration(meta.Duration * float64(time.Second))
		switch {
		case req.MaxDuration > 0 && meta.IsLive:
			return nil, fmt.Errorf("%w: live streams are not supported", downloader.ErrNoMedia)
		case req.MaxDuration > 0 && d > req.MaxDuration:
			return nil, fmt.Errorf("%w: %s (limit %s)", downloader.ErrTooLong, d.Round(time.Second), req.MaxDuration)
		case req.MaxDuration > 0 && meta.ID != "" && d <= 0 && !req.AllowUnknownDuration:
			return nil, fmt.Errorf("%w: no duration, probably a live stream", downloader.ErrNoMedia)
		}
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
	if info.Size() > limit {
		if !keep || req.Audio {
			return nil, downloader.ErrTooLarge
		}
		downloader.Report(ctx, downloader.Progress{Stage: downloader.StageProcessing})
		if file, err = compress(ctx, file, meta.Duration, limit); err != nil {
			return nil, err
		}
	}
	if keep {
		// The local Bot API server runs as another user: let it read the file.
		if err := os.Chmod(dir, 0o755); err != nil {
			return nil, fmt.Errorf("media dir permissions: %w", err)
		}
		if err := os.Chmod(file, 0o644); err != nil {
			return nil, fmt.Errorf("media file permissions: %w", err)
		}
		return &File{Name: filepath.Base(file), Path: file, Info: meta, dir: dir}, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read downloaded file: %w", err)
	}
	return &File{Name: filepath.Base(file), Data: data, Info: meta}, nil
}

// FastYouTube are yt-dlp arguments that make YouTube lookups about a second
// faster: yt-dlp asks YouTube as only one app (tv_samsung, which serves the
// streams with cookies and a PO token) instead of several. YouTube changes
// which apps work now and then, so callers retry without them on failure.
var FastYouTube = []string{"--extractor-args", "youtube:player_client=tv_samsung"}

// DirectArgs make a run connect directly, bypassing the shared --proxy
// (yt-dlp treats an empty proxy as "no proxy"; the last --proxy wins).
var DirectArgs = []string{"--proxy", ""}

// Direct is a single downloadable file, as yt-dlp resolved it.
type Direct struct {
	URL  string
	Ext  string // e.g. "mp4"
	Size int64  // bytes, exact or estimated; 0 if unknown
}

// errNotSingleFile means the chosen format is not one file at one URL (it
// would need a merge, or the site streams it in pieces).
var errNotSingleFile = errors.New("format is not a single file")

// DirectURL resolves link to the URL of one file in req.Format, without
// downloading anything. Formats that need merging are refused, so req.Format
// should pick a file that has both video and audio.
func (c *Client) DirectURL(ctx context.Context, link string, req Request) (*Direct, error) {
	args := []string{"--no-playlist", "--no-warnings", "-f", req.Format}
	if req.Sort != "" {
		args = append(args, "-S", req.Sort)
	}
	args = append(args, req.Extra...)
	args = append(args,
		"--print", "%(protocol)s\t%(ext)s\t%(filesize,filesize_approx)s\t%(url)s",
		"--", link)

	out, err := c.exec(ctx, args)
	if err != nil {
		return nil, Classify(err)
	}
	fields := strings.SplitN(lastLine(out), "\t", 4)
	if len(fields) != 4 {
		return nil, fmt.Errorf("unexpected yt-dlp output %q", lastLine(out))
	}
	proto, ext, size, u := fields[0], fields[1], fields[2], fields[3]
	// Merged formats have no single url; m3u8/dash are playlists, not files.
	if !strings.HasPrefix(u, "https://") || proto != "https" {
		return nil, errNotSingleFile
	}
	return &Direct{URL: u, Ext: ext, Size: parseCount(size)}, nil
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

// Progress lines yt-dlp prints for us; see the --progress-template arguments in Fetch.
const (
	progressPrefix = "anydl-progress:"
	processPrefix  = "anydl-pp:"
)

// progressArgs make yt-dlp print one machine-readable line per progress update.
var progressArgs = []string{
	"--progress", "--newline",
	"--progress-template", "download:" + progressPrefix + "%(progress.downloaded_bytes)s:%(progress.total_bytes)s:%(progress.total_bytes_estimate)s",
	"--progress-template", "postprocess:" + processPrefix + "%(progress.status)s",
}

// scanOutput reads yt-dlp's output line by line: progress lines become
// progress reports, everything else is kept in keep. Lines may end in \n or \r.
//
// Post-processing is only reported once the download has begun: yt-dlp also
// reports the metadata print that runs right after the lookup (Fetch's
// "pre_process" --print) as post-processing, and the lookup must keep showing
// as "preparing".
func scanOutput(ctx context.Context, r io.Reader, keep *bytes.Buffer, downloading *atomic.Bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20) // --dump-single-json prints one long line
	sc.Split(scanLines)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, progressPrefix):
			parts := strings.Split(strings.TrimPrefix(line, progressPrefix), ":")
			if len(parts) != 3 {
				continue
			}
			total := parseCount(parts[1])
			if total == 0 {
				total = parseCount(parts[2]) // an estimate is better than nothing
			}
			downloading.Store(true)
			downloader.Report(ctx, downloader.Progress{Stage: downloader.StageDownloading, Done: parseCount(parts[0]), Total: total})
		case strings.HasPrefix(line, processPrefix):
			if downloading.Load() {
				downloader.Report(ctx, downloader.Progress{Stage: downloader.StageProcessing})
			}
		default:
			keep.WriteString(line)
			keep.WriteByte('\n')
		}
	}
	_, _ = io.Copy(io.Discard, r) // drain anything left after a scanner error
}

// scanLines is bufio.ScanLines that also splits on a bare \r.
func scanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, c := range data {
		if c == '\n' || c == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// parseCount parses a byte count from a progress template; "NA" and anything
// else unparsable is 0. yt-dlp may print estimates with a fraction.
func parseCount(s string) int64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 {
		return 0
	}
	return int64(f)
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
