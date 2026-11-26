package bot

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"anydloadbot/internal/downloader"
)

// blockingProvider's downloads run until release is closed or their context is
// cancelled, like a slow yt-dlp run.
type blockingProvider struct {
	started atomic.Int32
	release chan struct{}
}

func newBlockingProvider() *blockingProvider { return &blockingProvider{release: make(chan struct{})} }

func (*blockingProvider) Name() string { return "Block" }
func (*blockingProvider) FindURL(text string) (string, bool) {
	for _, w := range strings.Fields(text) {
		if strings.HasPrefix(w, "block://") {
			return w, true
		}
	}
	return "", false
}

func (p *blockingProvider) Download(ctx context.Context, _ string) (*downloader.Media, error) {
	p.started.Add(1)
	select {
	case <-p.release:
		return &downloader.Media{Filename: "v.mp4", Data: []byte("video"), Title: "done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func chatMessage(id int, user int64, text string) *models.Update {
	return &models.Update{Message: &models.Message{
		ID: id, Text: text, From: &models.User{ID: user}, Chat: models.Chat{ID: 5},
	}}
}

// runChat starts a chat download in the background and returns a function that waits for it.
func runChat(b *Bot, id int, user int64) (wait func()) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.handleDefault(context.Background(), b.api, chatMessage(id, user, "block://x"))
	}()
	return wg.Wait
}

func (f *fakeTelegram) count(method string) int {
	n := 0
	for _, m := range f.methods() {
		if m == method {
			n++
		}
	}
	return n
}

// texts returns the text field of every call to method, in order.
func (f *fakeTelegram) texts(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c.fields["text"])
		}
	}
	return out
}

func anyContains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestUserLimitAndCancelCommand(t *testing.T) {
	t.Parallel()

	p := newBlockingProvider()
	b, tg := newServerBot(t, p, WithLimits(2, 100))

	wait1, wait2 := runChat(b, 1, 5), runChat(b, 2, 5)
	waitFor(t, "two downloads to start", func() bool { return p.started.Load() == 2 })

	// The third is refused straight away and never reaches the provider.
	b.handleDefault(context.Background(), b.api, chatMessage(3, 5, "block://x"))
	if !anyContains(tg.texts("sendMessage"), "already have 2 downloads") {
		t.Errorf("no limit message; sent: %q", tg.texts("sendMessage"))
	}
	if p.started.Load() != 2 {
		t.Errorf("refused download reached the provider (%d started)", p.started.Load())
	}

	// Another user isn't affected by this user's limit.
	other := runChat(b, 4, 6)
	waitFor(t, "another user's download", func() bool { return p.started.Load() == 3 })

	// /cancel stops this user's downloads only.
	b.handleCancel(context.Background(), b.api, chatMessage(5, 5, "/cancel"))
	wait1()
	wait2()
	if got := tg.texts("sendMessage"); !anyContains(got, "Cancelling your 2 downloads") {
		t.Errorf("no /cancel reply; sent: %q", got)
	}
	if n := tg.count("editMessageText"); n != 2 {
		t.Errorf("%d status messages marked cancelled, want 2", n)
	}
	if !anyContains(tg.texts("editMessageText"), cancelledText) {
		t.Errorf("status not marked cancelled: %q", tg.texts("editMessageText"))
	}
	if tg.count("sendVideo") != 0 {
		t.Error("a cancelled download still sent a video")
	}

	// The other user's download is untouched and still completes.
	close(p.release)
	other()
	if tg.count("sendVideo") != 1 {
		t.Errorf("the other user's download did not complete: %v", tg.methods())
	}

	// Slots are free again.
	if n := b.jobs.byUserCount(5); n != 0 {
		t.Errorf("leaked jobs for the cancelled user: %d", n)
	}
}

func TestCancelButton(t *testing.T) {
	t.Parallel()

	p := newBlockingProvider()
	b, tg := newServerBot(t, p)
	wait := runChat(b, 1, 5)
	waitFor(t, "download to start", func() bool { return p.started.Load() == 1 })
	waitFor(t, "status message", func() bool { return tg.count("sendMessage") == 1 })

	press := func(user int64) {
		b.api.ProcessUpdate(context.Background(), &models.Update{CallbackQuery: &models.CallbackQuery{
			ID: "cb", From: models.User{ID: user}, Data: cancelData,
			// The first status message the fake hands out has ID 100.
			Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: 100, Chat: models.Chat{ID: 5}}},
		}})
	}

	// Someone else pressing the button changes nothing.
	press(99)
	if got := tg.texts("answerCallbackQuery"); len(got) != 1 {
		t.Fatalf("callback answers = %q", got)
	}
	if tg.count("editMessageText") != 0 {
		t.Fatal("a stranger's button press affected the download")
	}

	press(5)
	wait()
	if tg.count("editMessageText") != 1 || !anyContains(tg.texts("editMessageText"), cancelledText) {
		t.Errorf("status not marked cancelled: %v", tg.methods())
	}
	if tg.count("deleteMessage") != 0 || tg.count("sendVideo") != 0 {
		t.Errorf("unexpected calls after cancel: %v", tg.methods())
	}
}

func TestGlobalLimit(t *testing.T) {
	t.Parallel()

	p := newBlockingProvider()
	b, tg := newServerBot(t, p, WithLimits(5, 1))
	wait := runChat(b, 1, 5)
	waitFor(t, "download to start", func() bool { return p.started.Load() == 1 })

	// A different user is turned away because the whole bot is busy, not because of their own limit.
	b.handleDefault(context.Background(), b.api, chatMessage(2, 6, "block://x"))
	if !anyContains(tg.texts("sendMessage"), "handling a lot of downloads") {
		t.Errorf("no busy message; sent: %q", tg.texts("sendMessage"))
	}
	if p.started.Load() != 1 {
		t.Error("refused download reached the provider")
	}

	close(p.release)
	wait()

	// Once the slot is free, the next request goes through. (Another link: the
	// first one is cached now and would be sent without a download.)
	b.handleDefault(context.Background(), b.api, chatMessage(3, 6, "block://y"))
	if p.started.Load() != 2 {
		t.Errorf("download after the slot freed did not start (%d started)", p.started.Load())
	}
}

func TestInlineLimitAndCancel(t *testing.T) {
	t.Parallel()

	p := newBlockingProvider()
	b, tg := newServerBot(t, p, WithLimits(1, 100))

	pick := func(inlineID string) *models.Update {
		u := chosen("block://x")
		u.ChosenInlineResult.InlineMessageID = inlineID
		return u
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.handleChosenInline(context.Background(), b.api, pick("INL-1"))
	}()
	waitFor(t, "inline download to start", func() bool { return p.started.Load() == 1 })

	// A second pick by the same user is refused in the placeholder itself.
	b.handleChosenInline(context.Background(), b.api, pick("INL-2"))
	if !anyContains(tg.texts("editMessageText"), "already have 1 downloads") {
		t.Errorf("no limit message in the placeholder; edits: %q", tg.texts("editMessageText"))
	}

	// The Cancel button on the placeholder stops the first one.
	b.api.ProcessUpdate(context.Background(), &models.Update{CallbackQuery: &models.CallbackQuery{
		ID: "cb", From: models.User{ID: 5}, Data: cancelData, InlineMessageID: "INL-1",
	}})
	wg.Wait()

	if !anyContains(tg.texts("editMessageText"), cancelledText) {
		t.Errorf("placeholder not marked cancelled: %q", tg.texts("editMessageText"))
	}
	if tg.count("sendVideo") != 0 || tg.count("editMessageMedia") != 0 {
		t.Errorf("a cancelled inline download still sent media: %v", tg.methods())
	}
}

func TestPlaceholderHasCancelButton(t *testing.T) {
	t.Parallel()

	b, _ := newServerBot(t, &slowProvider{})
	got := b.inlineResults(context.Background(), "slow://abc")
	row := got[0].(*models.InlineQueryResultArticle).ReplyMarkup.(*models.InlineKeyboardMarkup).InlineKeyboard[0]

	var hasCancel, hasSource bool
	for _, btn := range row {
		hasCancel = hasCancel || btn.CallbackData == cancelData
		hasSource = hasSource || btn.URL != ""
	}
	if !hasCancel || !hasSource {
		t.Errorf("placeholder buttons: %+v, want Source and Cancel", row)
	}
}
