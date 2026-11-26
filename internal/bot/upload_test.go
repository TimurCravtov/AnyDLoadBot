package bot

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	tgbot "github.com/go-telegram/bot"

	"anydloadbot/internal/downloader"
)

func TestUploadProgressTransport(t *testing.T) {
	t.Parallel()

	var received int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		received = len(data)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	var mu sync.Mutex
	var reports []downloader.Progress
	ctx := downloader.WithProgress(context.Background(), func(p downloader.Progress) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, p)
	})
	client := newAPIClient(nil)
	body := bytes.Repeat([]byte("v"), 300_000)

	send := func(ctx context.Context) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if received != len(body) {
			t.Errorf("server got %d bytes, want %d", received, len(body))
		}
	}

	// An upload request reports its progress, ending with every byte sent.
	send(withUpload(ctx))
	mu.Lock()
	if len(reports) == 0 {
		t.Fatal("no upload progress reported")
	}
	last := reports[len(reports)-1]
	if last.Stage != downloader.StageUploading || last.Done != int64(len(body)) || last.Total != int64(len(body)) {
		t.Errorf("last report = %+v, want all %d bytes sent", last, len(body))
	}
	reports = nil
	mu.Unlock()

	// Other requests (progress edits, ...) carry the callback too, but are not
	// uploads and must not report anything.
	send(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 0 {
		t.Errorf("an unmarked request reported %d progress updates, want none", len(reports))
	}
}

func TestAPIClientHasNoOverallTimeout(t *testing.T) {
	t.Parallel()

	// A whole-request timeout would cut off large uploads on slow links.
	if c := newAPIClient(nil); c.Timeout != 0 {
		t.Errorf("client timeout = %v, want none", c.Timeout)
	}
}

func TestAPIClientProxy(t *testing.T) {
	t.Parallel()

	// Every API request, polling and uploads alike, goes through the proxy.
	proxy := &url.URL{Scheme: "socks5", Host: "warp:1080"}
	req := httptest.NewRequest(http.MethodPost, "https://api.telegram.org/botTOKEN/getUpdates", nil)

	tr := newAPIClient(proxy).Transport.(uploadProgressTransport).base.(*http.Transport)
	got, err := tr.Proxy(req)
	if err != nil || got == nil || got.String() != proxy.String() {
		t.Errorf("proxy for API request = %v (err %v), want %v", got, err, proxy)
	}
}

func TestLogOut(t *testing.T) {
	t.Parallel()

	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.URL.Path == "/botGOOD/logOut" {
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
	}))
	t.Cleanup(srv.Close)

	if err := logOut(context.Background(), srv.URL, "GOOD", newAPIClient(nil)); err != nil || path != "/botGOOD/logOut" {
		t.Errorf("logOut() = %v (path %s)", err, path)
	}
	if err := logOut(context.Background(), srv.URL, "BAD", newAPIClient(nil)); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("logOut() with a bad token = %v, want Telegram's error", err)
	}
	// A network error must not reveal the token, which is part of the URL.
	if err := logOut(context.Background(), "http://127.0.0.1:1", "SECRET", newAPIClient(nil)); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("logOut() error = %v, want one without the token", err)
	}
}

func TestWaitForAPI(t *testing.T) {
	t.Parallel()

	// Unreachable twice (a local Bot API server still starting), then up.
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		switch {
		case strings.HasSuffix(r.URL.Path, "/botBAD/getMe"):
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
		case calls <= 2:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":502,"description":"Bad Gateway"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"B","username":"anydl_bot"}}`))
		}
	}))
	t.Cleanup(srv.Close)

	newBot := func(token string) *Bot {
		b, err := New(token, downloader.NewRegistry(), newTestBot().log,
			withAPIOptions(tgbot.WithServerURL(srv.URL)))
		if err != nil {
			t.Fatalf("New() = %v; it must not call Telegram", err)
		}
		b.apiRetry = time.Millisecond
		return b
	}

	b := newBot("GOOD")
	if err := b.waitForAPI(context.Background()); err != nil || b.username != "anydl_bot" {
		t.Errorf("waitForAPI() = %v, username %q; want success after retries", err, b.username)
	}
	if calls != 3 {
		t.Errorf("getMe calls = %d, want 3", calls)
	}

	// A rejected token is the one fatal error.
	if err := newBot("BAD").waitForAPI(context.Background()); err == nil || !strings.Contains(err.Error(), "BOT_TOKEN") {
		t.Errorf("waitForAPI() with a bad token = %v", err)
	}
}
