package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"anydloadbot/internal/downloader"
)

const (
	storageChat = int64(-100777)
	fakeFileID  = "FILE-ID-123"
)

// slowProvider needs a real download (no direct URL) but offers a placeholder.
type slowProvider struct {
	mu        sync.Mutex
	downloads int
	err       error
}

func (*slowProvider) Name() string { return "Slow" }
func (*slowProvider) FindURL(text string) (string, bool) {
	for _, w := range strings.Fields(text) {
		if strings.HasPrefix(w, "slow://") {
			return w, true
		}
	}
	return "", false
}

func (*slowProvider) Preview(_ context.Context, link string) (*downloader.Preview, bool) {
	return &downloader.Preview{
		ThumbnailURL: "https://img/" + path.Base(link) + ".jpg",
		Title:        "Slow video",
		Description:  "Some channel",
	}, true
}

func (p *slowProvider) Download(context.Context, string) (*downloader.Media, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.downloads++
	if p.err != nil {
		return nil, p.err
	}
	return &downloader.Media{Filename: "v.mp4", Data: []byte("video"), Title: "A title"}, nil
}

// fakeTelegram records Bot API calls and answers like Telegram would.
type fakeTelegram struct {
	mu    sync.Mutex
	calls []fakeCall
}

type fakeCall struct {
	method string
	fields map[string]string
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	call := fakeCall{method: path.Base(r.URL.Path), fields: map[string]string{}}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		_ = r.ParseMultipartForm(32 << 20)
		for k, v := range r.MultipartForm.Value {
			call.fields[k] = v[0]
		}
		for k := range r.MultipartForm.File {
			call.fields["file:"+k] = "uploaded"
		}
	} else {
		var body map[string]json.RawMessage
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		for k, v := range body {
			call.fields[k] = string(v)
		}
	}

	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if call.method == "sendVideo" {
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":%d,"type":"private"},"video":{"file_id":%q,"file_unique_id":"u","width":1,"height":1,"duration":1}}}`, storageChat, fakeFileID)
		return
	}
	_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
}

func (f *fakeTelegram) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.method
	}
	return out
}

func (f *fakeTelegram) call(method string) (fakeCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.method == method {
			return c, true
		}
	}
	return fakeCall{}, false
}

func newServerBot(t *testing.T, p downloader.Provider) (*Bot, *fakeTelegram) {
	t.Helper()

	tg := &fakeTelegram{}
	srv := httptest.NewServer(tg)
	t.Cleanup(srv.Close)

	b, err := New("TOKEN", downloader.NewRegistry(p), newTestBot().log,
		WithStorageChat(storageChat),
		withAPIOptions(tgbot.WithServerURL(srv.URL), tgbot.WithSkipGetMe()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return b, tg
}

func chosen(query string) *models.Update {
	return &models.Update{ChosenInlineResult: &models.ChosenInlineResult{
		ResultID: "r", Query: query, InlineMessageID: "INLINE-1", From: models.User{ID: 5},
	}}
}

func TestPlaceholderResult(t *testing.T) {
	t.Parallel()

	b, _ := newServerBot(t, &slowProvider{})
	got := b.inlineResults(context.Background(), "slow://abc")
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	ph, ok := got[0].(*models.InlineQueryResultPhoto)
	if !ok {
		t.Fatalf("result is %T, want *InlineQueryResultPhoto", got[0])
	}
	if ph.Title != "Slow video" || ph.Description != "Some channel" {
		t.Errorf("title/description not passed through: %q / %q", ph.Title, ph.Description)
	}
	if ph.PhotoURL != "https://img/abc.jpg" || ph.ReplyMarkup == nil || len(ph.ID) == 0 || len(ph.ID) > 64 {
		t.Errorf("unexpected placeholder: %+v", ph)
	}

	// Without a storage chat there is nowhere to upload, so no placeholder is offered.
	noStorage := newTestBot(&slowProvider{})
	if got := noStorage.inlineResults(context.Background(), "slow://abc"); len(got) != 0 {
		t.Errorf("got %d results without storage chat, want 0", len(got))
	}
}

func TestChosenInlineResult(t *testing.T) {
	t.Parallel()

	p := &slowProvider{}
	b, tg := newServerBot(t, p)

	b.handleChosenInline(context.Background(), b.api, chosen("look slow://abc"))

	if got := strings.Join(tg.methods(), ","); got != "sendVideo,editMessageMedia" {
		t.Fatalf("API calls = %s, want sendVideo,editMessageMedia", got)
	}
	send, _ := tg.call("sendVideo")
	if send.fields["chat_id"] != fmt.Sprint(storageChat) || send.fields["file:video"] != "uploaded" {
		t.Errorf("sendVideo fields: %v", send.fields)
	}
	edit, _ := tg.call("editMessageMedia")
	if edit.fields["inline_message_id"] != "INLINE-1" && edit.fields["inline_message_id"] != `"INLINE-1"` {
		t.Errorf("editMessageMedia inline_message_id = %q", edit.fields["inline_message_id"])
	}
	if !strings.Contains(edit.fields["media"], fakeFileID) {
		t.Errorf("editMessageMedia media %q does not reference the uploaded file_id", edit.fields["media"])
	}

	// A second pick of the same link reuses the cached file_id: no new download or upload.
	b.handleChosenInline(context.Background(), b.api, chosen("slow://abc"))
	if p.downloads != 1 {
		t.Errorf("downloads = %d, want 1 (cached)", p.downloads)
	}
	if got := strings.Join(tg.methods(), ","); got != "sendVideo,editMessageMedia,editMessageMedia" {
		t.Errorf("API calls = %s", got)
	}
}

func TestChosenInlineResultFailure(t *testing.T) {
	t.Parallel()

	b, tg := newServerBot(t, &slowProvider{err: downloader.ErrTooLong})
	b.handleChosenInline(context.Background(), b.api, chosen("slow://abc"))

	if got := strings.Join(tg.methods(), ","); got != "editMessageCaption" {
		t.Fatalf("API calls = %s, want only editMessageCaption", got)
	}
	edit, _ := tg.call("editMessageCaption")
	if !strings.Contains(edit.fields["caption"], tooLongText) {
		t.Errorf("caption = %q, want the too-long message", edit.fields["caption"])
	}
}

func TestChosenInlineResultIgnored(t *testing.T) {
	t.Parallel()

	b, tg := newServerBot(t, &slowProvider{})

	noMsgID := chosen("slow://abc")
	noMsgID.ChosenInlineResult.InlineMessageID = "" // direct-URL results have no keyboard, so no ID
	b.handleChosenInline(context.Background(), b.api, noMsgID)
	b.handleChosenInline(context.Background(), b.api, chosen("no link here"))

	if got := tg.methods(); len(got) != 0 {
		t.Errorf("unexpected API calls: %v", got)
	}
}
