package bot

import (
	"context"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"

	"anydloadbot/internal/downloader"
)

// describedProvider is a direct-URL provider (inline works) with a description.
type describedProvider struct {
	name, desc string
}

func (p describedProvider) Name() string                { return p.name }
func (p describedProvider) Description() string         { return p.desc }
func (describedProvider) FindURL(string) (string, bool) { return "", false }
func (describedProvider) Download(context.Context, string) (*downloader.Media, error) {
	return nil, nil
}
func (describedProvider) Resolve(context.Context, string) (*downloader.Source, error) {
	return nil, nil
}

func balanced(t *testing.T, s string) {
	t.Helper()
	for _, tag := range []string{"b", "code"} {
		if o, c := strings.Count(s, "<"+tag+">"), strings.Count(s, "</"+tag+">"); o != c {
			t.Errorf("unbalanced <%s>: %d open, %d close in:\n%s", tag, o, c, s)
		}
	}
}

func TestHelpText(t *testing.T) {
	t.Parallel()

	b := newTestBot(
		describedProvider{name: "Pics", desc: "images <& more>"},
		&slowProvider{}, // no description; has MP3 and a placeholder
	)
	b.storageChatID = storageChat
	b.username = "mybot"

	got := b.helpText()
	balanced(t, got)

	for _, want := range []string{
		"<b>Pics</b> — images &lt;&amp; more&gt;", // descriptions are HTML-escaped
		"<b>Slow</b>",
		"MP3",
		"<code>@mybot</code>",
		"⏳",
		"50 MB",
		"/help",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("help is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "images <&") {
		t.Errorf("help contains an unescaped description:\n%s", got)
	}
	// Both sites support inline here: Pics via a direct URL, Slow via the placeholder.
	if n := strings.Count(got, "⚡ inline"); n != 2 {
		t.Errorf("%d sites tagged inline, want 2:\n%s", n, got)
	}
}

func TestHelpTextAdaptsToConfiguration(t *testing.T) {
	t.Parallel()

	// Only a direct-URL site, and the username is unknown: no MP3 or placeholder text.
	b := newTestBot(describedProvider{name: "Pics", desc: "images"})
	got := b.helpText()
	balanced(t, got)

	if strings.Contains(got, "MP3") || strings.Contains(got, "⏳") {
		t.Errorf("help mentions MP3 or the placeholder without a provider for them:\n%s", got)
	}
	if !strings.Contains(got, "<code>@botname</code>") {
		t.Errorf("help should fall back to a placeholder username:\n%s", got)
	}

	// Without a storage chat the placeholder sites can't do inline, so they aren't tagged.
	noStorage := newTestBot(&slowProvider{})
	if strings.Contains(noStorage.helpText(), "⚡ inline") {
		t.Errorf("site tagged inline without a storage chat:\n%s", noStorage.helpText())
	}
}

func TestHandleHelpSendsHTML(t *testing.T) {
	t.Parallel()

	b, tg := newServerBot(t, describedProvider{name: "Pics", desc: "images"})
	b.handleHelp(context.Background(), b.api, &models.Update{Message: &models.Message{ID: 1, Chat: models.Chat{ID: 5}}})

	call, ok := tg.call("sendMessage")
	if !ok {
		t.Fatal("no sendMessage call")
	}
	if got := call.fields["parse_mode"]; got != "HTML" && got != `"HTML"` {
		t.Errorf("parse_mode = %q, want HTML", got)
	}
}

func TestStartText(t *testing.T) {
	t.Parallel()

	got := newTestBot(&slowProvider{}).startText()
	for _, want := range []string{"Slow", "mp3", "/help"} {
		if !strings.Contains(got, want) {
			t.Errorf("start text is missing %q: %s", want, got)
		}
	}
}

func commandUpdate(text string) *models.Update {
	entities := []models.MessageEntity{}
	if strings.HasPrefix(text, "/") {
		end := strings.IndexByte(text, ' ')
		if end < 0 {
			end = len(text)
		}
		entities = append(entities, models.MessageEntity{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: end})
	}
	return &models.Update{ID: 1, Message: &models.Message{ID: 1, Text: text, Entities: entities, Chat: models.Chat{ID: 5}}}
}

// TestCommandsAreDispatched sends real updates through the library's dispatcher.
// It exists because /start and /help were once registered with a pattern the
// library never matched, so both fell through to the default handler.
func TestCommandsAreDispatched(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text     string
		wantText string // substring of the reply
	}{
		{"/start", "Send me a link and I'll send the video or image straight back"},
		{"/help", "Supported sites"},
		{"/HELP", "Supported sites"},
		{"/help@mybot", "Supported sites"},
		{"/help please", "Supported sites"},
		{"/start@mybot", "Send me a link and I'll send the video or image straight back"},
		{"/helpme", "Send me a link from a supported site"}, // a different command: falls back
		{"hello", "Send me a link from a supported site"},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			t.Parallel()

			b, tg := newServerBot(t, describedProvider{name: "Pics", desc: "images"})
			b.api.ProcessUpdate(context.Background(), commandUpdate(tt.text))

			call, ok := tg.call("sendMessage")
			if !ok {
				t.Fatalf("no reply to %q; API calls: %v", tt.text, tg.methods())
			}
			if !strings.Contains(call.fields["text"], tt.wantText) {
				t.Errorf("reply to %q = %q, want it to contain %q", tt.text, call.fields["text"], tt.wantText)
			}
		})
	}
}
