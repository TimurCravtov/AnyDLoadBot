package downloader

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

type fakeProvider struct {
	name   string
	prefix string
}

func (f fakeProvider) Name() string { return f.name }

func (f fakeProvider) FindURL(text string) (string, bool) {
	for _, w := range strings.Fields(text) {
		if strings.HasPrefix(w, f.prefix) {
			return w, true
		}
	}
	return "", false
}

func (f fakeProvider) Download(context.Context, string) (*Media, error) { return &Media{}, nil }

func TestRegistry(t *testing.T) {
	t.Parallel()

	a := fakeProvider{name: "A", prefix: "a://"}
	b := fakeProvider{name: "B", prefix: "b://"}
	r := NewRegistry(a, b)

	p, link, ok := r.Find("see b://x and a://y")
	if !ok {
		t.Fatal("Find() found nothing")
	}
	// Providers are tried in registration order, so A wins even though B's link comes first.
	if p.Name() != "A" || link != "a://y" {
		t.Errorf("Find() = %s, %q; want A, a://y", p.Name(), link)
	}

	if _, _, ok := r.Find("nothing"); ok {
		t.Error("Find() matched text without links")
	}
	if got := r.Names(); !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Errorf("Names() = %v", got)
	}
}

func TestFetcherGet(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hello")) })
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://blocked.invalid/x", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	f := NewFetcher(func(host string) bool { return host == "127.0.0.1" })
	ctx := context.Background()

	if data, err := f.Get(ctx, srv.URL+"/ok", 100, "text/plain"); err != nil || string(data) != "hello" {
		t.Errorf("Get(/ok) = %q, %v", data, err)
	}
	if _, err := f.Get(ctx, srv.URL+"/ok", 3, "text/plain"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Get() over limit: err = %v, want ErrTooLarge", err)
	}
	if _, err := f.Get(ctx, srv.URL+"/missing", 100, "text/plain"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("Get(/missing): err = %v", err)
	}
	if _, err := f.Get(ctx, srv.URL+"/redirect", 100, "text/plain"); err == nil || !strings.Contains(err.Error(), "disallowed host") {
		t.Errorf("Get(/redirect): err = %v, want disallowed host", err)
	}
}

func TestFetcherLocation(t *testing.T) {
	t.Parallel()

	var followed atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/short", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/long?x=1", http.StatusFound)
	})
	mux.HandleFunc("/long", func(w http.ResponseWriter, _ *http.Request) {
		followed.Store(true)
		_, _ = w.Write([]byte("page"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	f := NewFetcher(func(string) bool { return true })
	loc, err := f.Location(context.Background(), srv.URL+"/short")
	if err != nil || loc != srv.URL+"/long?x=1" {
		t.Errorf("Location(/short) = %q, %v; want the absolute redirect target", loc, err)
	}
	if followed.Load() {
		t.Error("Location followed the redirect")
	}
	if loc, err := f.Location(context.Background(), srv.URL+"/long"); err != nil || loc != "" {
		t.Errorf("Location(/long) = %q, %v; want empty for a non-redirect", loc, err)
	}
}

// TestSetProxy is not parallel: it changes the package-wide proxy, and Go runs
// it while all parallel tests are paused.
func TestSetProxy(t *testing.T) {
	var gotURL atomic.Value
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL.Store(r.URL.String()) // a proxy receives the absolute URL
		_, _ = w.Write([]byte("via proxy"))
	}))
	t.Cleanup(proxySrv.Close)

	if err := SetProxy(proxySrv.URL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetProxy("") })

	f := NewFetcher(func(string) bool { return true })
	data, err := f.Get(context.Background(), "http://example.invalid/some/path", 100, "text/plain")
	if err != nil || string(data) != "via proxy" {
		t.Fatalf("Get through proxy = %q, %v", data, err)
	}
	if got, _ := gotURL.Load().(string); got != "http://example.invalid/some/path" {
		t.Errorf("proxy saw %q", got)
	}

	if err := SetProxy("not a url"); err == nil || strings.Contains(err.Error(), "not a url") {
		t.Errorf("SetProxy(invalid) = %v; want an error that does not echo the value", err)
	}

	// Back to direct: a Fetcher created now must not use the proxy.
	if err := SetProxy(""); err != nil {
		t.Fatal(err)
	}
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("direct")) }))
	t.Cleanup(direct.Close)
	if data, err := NewFetcher(func(string) bool { return true }).Get(context.Background(), direct.URL, 100, "text/plain"); err != nil || string(data) != "direct" {
		t.Errorf("after reset: %q, %v", data, err)
	}
}
