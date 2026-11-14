package downloader

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
