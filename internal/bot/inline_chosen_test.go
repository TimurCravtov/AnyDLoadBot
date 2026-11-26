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
	mu             sync.Mutex
	downloads      int
	audioDownloads int
	err            error
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

func (p *slowProvider) DownloadAudio(context.Context, string) (*downloader.Media, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.audioDownloads++
	if p.err != nil {
		return nil, p.err
	}
	return &downloader.Media{
		Kind: downloader.KindAudio, Filename: "a.mp3", Data: []byte("audio"),
		Title: "Song", Performer: "Band", Duration: 187,
	}, nil
}

// fakeTelegram records Bot API calls and answers like Telegram would.
type fakeTelegram struct {
	mu     sync.Mutex
	calls  []fakeCall
	msgIDs int
	// rejectURLs makes sendVideo fail for URLs, like Telegram does when it
	// can't fetch them.
	rejectURLs bool
	// rejectEmoji makes calls with custom emoji fail, like Telegram does for
	// bots whose owner has no Premium.
	rejectEmoji bool
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
	reject, rejectEmoji := f.rejectURLs, f.rejectEmoji
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if reject && strings.HasPrefix(call.fields["video"], "https://") {
		fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: failed to get HTTP URL content"}`)
		return
	}
	if rejectEmoji && strings.Contains(call.fields["entities"], "custom_emoji") {
		fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: custom emoji entities can't be used"}`)
		return
	}
	chat := fmt.Sprintf(`"message_id":1,"date":0,"chat":{"id":%d,"type":"private"}`, storageChat)
	switch call.method {
	case "sendVideo":
		fmt.Fprintf(w, `{"ok":true,"result":{%s,"video":{"file_id":%q,"file_unique_id":"u","width":1,"height":1,"duration":1}}}`, chat, fakeFileID)
	case "sendMessage":
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"date":0,"chat":{"id":5,"type":"private"},"text":"x"}}`, f.nextMessageID())
	case "sendAudio":
		fmt.Fprintf(w, `{"ok":true,"result":{%s,"audio":{"file_id":%q,"file_unique_id":"u","duration":1}}}`, chat, fakeFileID)
	case "getCustomEmojiStickers": // knows one custom emoji
		fmt.Fprintf(w, `{"ok":true,"result":[{"file_id":"f","file_unique_id":"u","type":"custom_emoji","width":100,"height":100,"is_animated":false,"is_video":false,"emoji":%q,"custom_emoji_id":%q}]}`, knownEmoji, knownEmojiID)
	case "sendPhoto":
		fmt.Fprintf(w, `{"ok":true,"result":{%s,"photo":[{"file_id":"small","file_unique_id":"s","width":1,"height":1},{"file_id":%q,"file_unique_id":"u","width":2,"height":2}]}}`, chat, fakeFileID)
	default:
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}
}

// nextMessageID hands out message IDs for sendMessage, starting at 100.
func (f *fakeTelegram) nextMessageID() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgIDs++
	return 99 + f.msgIDs
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

func newServerBot(t *testing.T, p downloader.Provider, extra ...Option) (*Bot, *fakeTelegram) {
	t.Helper()

	tg := &fakeTelegram{}
	srv := httptest.NewServer(tg)
	t.Cleanup(srv.Close)

	opts := append([]Option{
		WithStorageChat(storageChat),
		withAPIOptions(tgbot.WithServerURL(srv.URL), tgbot.WithSkipGetMe(), tgbot.WithNotAsyncHandlers()),
	}, extra...)
	b, err := New("TOKEN", downloader.NewRegistry(p), newTestBot().log, opts...)
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
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (video and mp3)", len(got))
	}

	video, ok := got[0].(*models.InlineQueryResultArticle)
	if !ok {
		t.Fatalf("result is %T, want a text row (*InlineQueryResultArticle)", got[0])
	}
	if video.Title != "Slow video" || video.Description != "Some channel" {
		t.Errorf("title/description not passed through: %q / %q", video.Title, video.Description)
	}
	if video.ThumbnailURL != "https://img/abc.jpg" || video.ReplyMarkup == nil {
		t.Errorf("unexpected placeholder: %+v", video)
	}

	audio := got[1].(*models.InlineQueryResultArticle)
	if !strings.HasPrefix(video.ID, videoPrefix) || !strings.HasPrefix(audio.ID, mp3Prefix) || video.ID == audio.ID {
		t.Errorf("result IDs must identify the format: %q, %q", video.ID, audio.ID)
	}
	if !strings.Contains(audio.Title, "MP3") || !strings.Contains(audio.Title, "Slow video") {
		t.Errorf("audio title = %q", audio.Title)
	}
	for _, r := range []*models.InlineQueryResultArticle{video, audio} {
		if len(r.ID) == 0 || len(r.ID) > 64 {
			t.Errorf("result ID %q must be 1-64 bytes", r.ID)
		}
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
	// Inline messages go out without a caption; the title was in the results list.
	if strings.Contains(edit.fields["media"], `"caption"`) {
		t.Errorf("inline video has a caption: %s", edit.fields["media"])
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

	if got := strings.Join(tg.methods(), ","); got != "editMessageText" {
		t.Fatalf("API calls = %s, want only editMessageText", got)
	}
	edit, _ := tg.call("editMessageText")
	if !strings.Contains(edit.fields["text"], tooLongText) {
		t.Errorf("text = %q, want the too-long message", edit.fields["text"])
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

func TestChosenInlineMP3(t *testing.T) {
	t.Parallel()

	p := &slowProvider{}
	b, tg := newServerBot(t, p)

	pick := chosen("slow://abc")
	pick.ChosenInlineResult.ResultID = mp3Prefix + "whatever"
	b.handleChosenInline(context.Background(), b.api, pick)

	if got := strings.Join(tg.methods(), ","); got != "sendAudio,editMessageMedia" {
		t.Fatalf("API calls = %s, want sendAudio,editMessageMedia", got)
	}
	if p.downloads != 0 || p.audioDownloads != 1 {
		t.Errorf("downloads video=%d audio=%d, want 0 and 1", p.downloads, p.audioDownloads)
	}

	send, _ := tg.call("sendAudio")
	if send.fields["title"] != "Song" || send.fields["performer"] != "Band" || send.fields["duration"] != "187" {
		t.Errorf("sendAudio tags: %v", send.fields)
	}
	edit, _ := tg.call("editMessageMedia")
	if !strings.Contains(edit.fields["media"], fakeFileID) || !strings.Contains(edit.fields["media"], `"type":"audio"`) {
		t.Errorf("editMessageMedia media = %q, want an audio item with the file_id", edit.fields["media"])
	}

	// The video and the MP3 of the same link are cached separately.
	b.handleChosenInline(context.Background(), b.api, chosen("slow://abc"))
	if p.downloads != 1 || p.audioDownloads != 1 {
		t.Errorf("downloads video=%d audio=%d, want 1 and 1", p.downloads, p.audioDownloads)
	}
}

// photoProvider returns a single image, like an Instagram or X image post. It
// deliberately has no DownloadAudio.
type photoProvider struct{}

func (*photoProvider) Name() string { return "Photo" }
func (*photoProvider) FindURL(text string) (string, bool) {
	for _, w := range strings.Fields(text) {
		if strings.HasPrefix(w, "photo://") {
			return w, true
		}
	}
	return "", false
}

func (*photoProvider) Download(context.Context, string) (*downloader.Media, error) {
	return &downloader.Media{Kind: downloader.KindPhoto, Filename: "p.jpg", Data: []byte("jpeg"), Title: "Cap"}, nil
}

func TestChosenInlinePhoto(t *testing.T) {
	t.Parallel()

	b, tg := newServerBot(t, &photoProvider{})
	b.handleChosenInline(context.Background(), b.api, chosen("photo://x"))

	if got := strings.Join(tg.methods(), ","); got != "sendPhoto,editMessageMedia" {
		t.Fatalf("API calls = %s, want sendPhoto,editMessageMedia", got)
	}
	edit, _ := tg.call("editMessageMedia")
	// The file_id must be the largest photo size, not the first.
	if !strings.Contains(edit.fields["media"], fakeFileID) || !strings.Contains(edit.fields["media"], `"type":"photo"`) {
		t.Errorf("editMessageMedia media = %q", edit.fields["media"])
	}
}

func TestChatSendsByKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		prov downloader.Provider
		want string
	}{
		{"video", "slow://abc", &slowProvider{}, "sendMessage,sendChatAction,sendVideo,deleteMessage"},
		{"mp3 keyword", "slow://abc mp3", &slowProvider{}, "sendMessage,sendChatAction,sendAudio,deleteMessage"},
		{"MP3 uppercase before link", "MP3 slow://abc", &slowProvider{}, "sendMessage,sendChatAction,sendAudio,deleteMessage"},
		{"photo", "photo://x", &photoProvider{}, "sendMessage,sendChatAction,sendPhoto,deleteMessage"},
		{"mp3 unsupported", "photo://x mp3", &photoProvider{}, "sendMessage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, tg := newServerBot(t, tt.prov)
			b.handleDefault(context.Background(), b.api, &models.Update{Message: &models.Message{
				ID: 9, Text: tt.text, Chat: models.Chat{ID: 77},
			}})
			if got := strings.Join(tg.methods(), ","); got != tt.want {
				t.Errorf("API calls = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestWantsAudio(t *testing.T) {
	t.Parallel()

	for text, want := range map[string]bool{
		"https://youtu.be/x mp3":                  true,
		"mp3 https://youtu.be/x":                  true,
		"https://youtu.be/x MP3":                  true,
		"https://youtu.be/x, mp3!":                true,
		"https://youtu.be/x":                      false,
		"https://example.com/dark-mp3-mix":        false, // inside a URL
		"https://youtu.be/x mp3player":            false,
		"https://youtu.be/x https://a.com/b mp3x": false,
	} {
		if got := wantsAudio(text); got != want {
			t.Errorf("wantsAudio(%q) = %v, want %v", text, got, want)
		}
	}
}
