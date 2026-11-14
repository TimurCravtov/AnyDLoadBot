// Package downloader defines the site-independent pieces of media downloading:
// the Provider interface each supported website implements, and a Registry
// that routes a message to the right provider.
package downloader

import (
	"context"
	"errors"
)

// MaxSize is the largest file the Telegram Bot API lets a bot upload.
const MaxSize = 50 << 20

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
	Title    string // optional; the caption (video/photo) or track title (audio)

	// Audio only.
	Performer string
	Duration  int // seconds
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
