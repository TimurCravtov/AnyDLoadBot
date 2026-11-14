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
)

// Media is a downloaded file ready to be sent to the user.
type Media struct {
	Filename string
	Data     []byte
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

// Names lists the supported sites, in registration order.
func (r *Registry) Names() []string {
	names := make([]string, len(r.providers))
	for i, p := range r.providers {
		names[i] = p.Name()
	}
	return names
}
