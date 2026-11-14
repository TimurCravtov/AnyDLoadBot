package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// Fetcher is a size-limited HTTP GET helper shared by providers. It only
// follows redirects to hosts the provider allows, which keeps user-supplied
// links from being used to reach arbitrary servers.
type Fetcher struct {
	client *http.Client
}

// NewFetcher creates a Fetcher that follows redirects only to hosts for which
// allowHost returns true.
func NewFetcher(allowHost func(host string) bool) *Fetcher {
	return &Fetcher{client: &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !allowHost(req.URL.Hostname()) {
				return fmt.Errorf("redirect to disallowed host %q", req.URL.Hostname())
			}
			return nil
		},
	}}
}

// Location makes one request to rawURL without following redirects and returns
// where it points ("" if the response is not a redirect). Callers follow the
// chain themselves, so they can stop as soon as they have what they need and
// check every hop against their own host rules.
func (f *Fetcher) Location(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)

	client := *f.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", nil
	}
	loc, err := resp.Location() // resolves relative redirects
	if err != nil {
		return "", err
	}
	return loc.String(), nil
}

// FinalURL follows redirects from rawURL and returns where they end, without
// reading the body. It is used to turn a redirecting "media" endpoint into a
// stable CDN URL that Telegram can fetch.
func (f *Fetcher) FinalURL(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %s", resp.Status)
	}
	return resp.Request.URL.String(), nil
}

// Get fetches rawURL and reads at most limit bytes; larger bodies yield ErrTooLarge.
// accept is the preferred media type, e.g. "text/html" or "video/mp4".
func (f *Fetcher) Get(ctx context.Context, rawURL string, limit int64, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", accept+",*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	if resp.ContentLength > limit {
		return nil, ErrTooLarge
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}
