// Package downloader defines the site-independent pieces of media downloading:
// the Provider interface each supported website implements, and a Registry
// that routes a message to the right provider.
package downloader

import (
	"context"
	"errors"
	"os"
	"slices"
)

// MaxSize is the largest file the Telegram Bot API lets a bot upload, and the
// limit for files held in memory.
const MaxSize = 50 << 20

// MaxLocalSize is the size limit for files kept on disk (Media.Path) and handed
// to a local Bot API server, which accepts far bigger files than MaxSize.
const MaxLocalSize = 300 << 20

var (
	// ErrNoMedia means the link has no downloadable media (e.g. an image-only pin).
	ErrNoMedia = errors.New("no downloadable media found")
	// ErrTooLarge means the media exceeds MaxSize.
	ErrTooLarge = errors.New("media is too large")
	// ErrTooLong means the media runs longer than the provider allows.
	ErrTooLong = errors.New("media is too long")
)

// Kind is the type of a media file, which decides how it is sent to Telegram.
type Kind int

// Media kinds. KindVideo is the zero value, so providers that only return
// videos don't need to set it.
const (
	KindVideo Kind = iota
	KindPhoto
	KindAudio
)

// Media is a downloaded file ready to be sent to the user.
type Media struct {
	Kind     Kind
	Filename string
	Data     []byte
	// Path, if set instead of Data, is the file on disk, in a directory a local
	// Bot API server can read. Whoever receives the Media must call Close.
	Path  string
	Title string // optional; the caption (video/photo) or track title (audio)

	// Audio only.
	Performer string
	Duration  int // seconds

	// Cleanup, if set, removes the file at Path. Close calls it.
	Cleanup func()
}

// Close releases the media's file on disk, if it has one. It is safe to call
// on a nil Media and more than once.
func (m *Media) Close() {
	if m == nil || m.Cleanup == nil {
		return
	}
	m.Cleanup()
	m.Cleanup = nil
}

// Size is the file's size in bytes.
func (m *Media) Size() int64 {
	if m.Path != "" {
		if info, err := os.Stat(m.Path); err == nil {
			return info.Size()
		}
		return 0
	}
	return int64(len(m.Data))
}

// Describer is implemented by providers that tell users what they support, for
// the /help command. Keep it to one short line, e.g. "videos up to 15 min, MP3".
type Describer interface {
	Description() string
}

// AudioDownloader is implemented by providers that can extract an MP3 (with
// title and cover art embedded) from a link.
type AudioDownloader interface {
	DownloadAudio(ctx context.Context, link string) (*Media, error)
}

// Provider downloads media from one website. Implementations must be safe for
// concurrent use.
type Provider interface {
	// Name is a short human-readable site name, e.g. "Pinterest".
	Name() string
	// FindURL returns the first link in text that this provider can handle.
	FindURL(text string) (link string, ok bool)
	// Download fetches the media behind a link returned by FindURL.
	// It returns ErrNoMedia or ErrTooLarge for expected failures.
	Download(ctx context.Context, link string) (*Media, error)
}

// Source describes media that Telegram can fetch itself from a direct URL.
type Source struct {
	Kind         Kind // KindVideo or KindPhoto
	URL          string
	ThumbnailURL string // JPEG preview, required by Telegram for inline videos
	Title        string
	MimeType     string // e.g. "video/mp4"; unused for photos
}

// Resolver is implemented by providers whose media has a direct, publicly
// fetchable URL. Inline mode needs this: Telegram downloads the file itself,
// so the bot only has to answer with a URL instead of uploading bytes.
type Resolver interface {
	// Resolve finds the direct media URL behind a link returned by FindURL.
	// It returns ErrNoMedia for expected failures.
	Resolve(ctx context.Context, link string) (*Source, error)
}

// MaxURLSize is the largest file Telegram fetches from a URL on a bot's behalf
// (sendVideo/sendPhoto with an HTTP URL); bigger files have to be uploaded.
const MaxURLSize = 20 << 20

// Linker is implemented by providers that can usually hand Telegram a direct
// URL to fetch the media from. That skips downloading the file and uploading
// it again, which is most of the time a send takes. The bot falls back to
// Download when MediaURL fails or Telegram can't fetch the URL.
type Linker interface {
	// MediaURL returns a direct URL for the media behind a link returned by
	// FindURL, for a file of at most MaxURLSize as far as the provider knows.
	MediaURL(ctx context.Context, link string) (*Source, error)
}

// Preview is a cheap placeholder shown in inline mode while the real media is
// still being downloaded.
type Preview struct {
	ThumbnailURL string // public JPEG
	Title        string
	Description  string // optional second line, e.g. the channel name
}

// Previewer is implemented by providers that have no direct media URL (so they
// can't be a Resolver) but can still offer a placeholder for inline mode.
// Inline queries must be answered within seconds, so Preview has to be fast:
// at most one quick request, and it should fall back to generic text instead
// of failing when that request doesn't work out.
type Previewer interface {
	Preview(ctx context.Context, link string) (*Preview, bool)
}

// Registry holds the enabled providers and routes messages to them.
type Registry struct {
	providers []Provider
}

// NewRegistry creates a Registry. Providers are tried in the given order.
func NewRegistry(providers ...Provider) *Registry {
	return &Registry{providers: providers}
}

// Find returns the first provider that recognises a link in text.
func (r *Registry) Find(text string) (p Provider, link string, ok bool) {
	for _, p := range r.providers {
		if link, ok := p.FindURL(text); ok {
			return p, link, true
		}
	}
	return nil, "", false
}

// All returns the registered providers, in registration order.
func (r *Registry) All() []Provider {
	return slices.Clone(r.providers)
}

// NamesWhere lists the names of the providers that satisfy keep, in registration order.
func (r *Registry) NamesWhere(keep func(Provider) bool) []string {
	var names []string
	for _, p := range r.providers {
		if keep(p) {
			names = append(names, p.Name())
		}
	}
	return names
}

// Names lists the supported sites, in registration order.
func (r *Registry) Names() []string {
	names := make([]string, len(r.providers))
	for i, p := range r.providers {
		names[i] = p.Name()
	}
	return names
}
