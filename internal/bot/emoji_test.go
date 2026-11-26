package bot

import (
	"context"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
)

// The custom emoji the fake Telegram knows, and the plain emoji its sticker names.
const (
	knownEmojiID = "5368324170671202286"
	knownEmoji   = "🫠"
)

func TestStatusEmoji(t *testing.T) {
	t.Parallel()

	b, tg := newServerBot(t, &slowProvider{}, withStatusEmoji([numStatusKinds]string{
		statusPreparing: knownEmojiID, statusDownloading: "999",
	}))
	b.loadEmoji(context.Background())

	if b.sendStatus(context.Background(), b.api, 5, 1) == 0 {
		t.Fatal("status message not sent")
	}
	send, _ := tg.call("sendMessage")
	// The sticker's own emoji replaces the plain one, and the entity covers it
	// (two UTF-16 units for 🫠).
	if !strings.HasPrefix(send.fields["text"], knownEmoji+" Preparing") {
		t.Errorf("text = %s, want it to start with the sticker's emoji", send.fields["text"])
	}
	if e := send.fields["entities"]; !strings.Contains(e, `"custom_emoji_id":"`+knownEmojiID+`"`) || !strings.Contains(e, `"length":2`) {
		t.Errorf("entities = %s", e)
	}

	// An ID Telegram doesn't know is dropped instead of breaking messages.
	if text, entities := b.statusMessage(statusDownloading, "⏳ Downloading… 1 MB"); text != "⏳ Downloading… 1 MB" || entities != nil {
		t.Errorf("unknown emoji used: %q %v", text, entities)
	}
	// Statuses without a custom emoji are unchanged.
	if text, entities := b.statusMessage(statusUploading, "📤 Uploading to Telegram…"); text != "📤 Uploading to Telegram…" || entities != nil {
		t.Errorf("uploading changed: %q %v", text, entities)
	}
}

func TestDefaultStatusEmoji(t *testing.T) {
	t.Parallel()

	// The running download shows the loading emoji; "Cancelled" stays plain.
	b, _ := newServerBot(t, &slowProvider{})
	for _, k := range []statusKind{statusPreparing, statusDownloading, statusProcessing, statusUploading} {
		if _, entities := b.statusMessage(k, defaultEmoji[k]+" x"); len(entities) != 1 || entities[0].CustomEmojiID != loadingEmojiID {
			t.Errorf("%s: entities = %v, want the loading emoji", statusNames[k], entities)
		}
	}
	if _, entities := b.statusMessage(statusCancelled, cancelledText); entities != nil {
		t.Errorf("cancelled: entities = %v, want none", entities)
	}
}

func TestStatusEmojiRefused(t *testing.T) {
	t.Parallel()

	b, tg := newServerBot(t, &slowProvider{}, withStatusEmoji([numStatusKinds]string{statusPreparing: knownEmojiID}))
	tg.rejectEmoji = true
	b.loadEmoji(context.Background())

	// Refused with the emoji, sent without it, and custom emoji are off from then on.
	if b.sendStatus(context.Background(), b.api, 5, 1) == 0 {
		t.Fatal("status message not sent without the emoji")
	}
	if n := tg.count("sendMessage"); n != 2 {
		t.Errorf("sendMessage calls = %d, want 2 (with, then without the emoji)", n)
	}
	if !b.emojiOff.Load() {
		t.Error("custom emoji still on after Telegram refused them")
	}
	b.sendStatus(context.Background(), b.api, 5, 1)
	if n := tg.count("sendMessage"); n != 3 {
		t.Errorf("sendMessage calls = %d, want 3 (no more attempts with the emoji)", n)
	}
}

func TestEmojiCommand(t *testing.T) {
	t.Parallel()

	b, tg := newServerBot(t, &slowProvider{})
	text := "/emoji 🫠🔥" // 🫠 and 🔥 are two UTF-16 units each
	b.handleEmoji(context.Background(), b.api, &models.Update{Message: &models.Message{
		ID: 1, Text: text, Chat: models.Chat{ID: 5},
		Entities: []models.MessageEntity{
			{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: 6},
			{Type: models.MessageEntityTypeCustomEmoji, Offset: 7, Length: 2, CustomEmojiID: "111"},
			{Type: models.MessageEntityTypeCustomEmoji, Offset: 9, Length: 2, CustomEmojiID: "222"},
		},
	}})
	got := strings.Join(tg.texts("sendMessage"), "")
	if !strings.Contains(got, "🫠 111\n🔥 222") {
		t.Errorf("reply = %s, want each emoji with its ID", got)
	}

	// Without premium emoji, it explains what to send.
	b.handleEmoji(context.Background(), b.api, chatMessage(2, 5, "/emoji"))
	if got := tg.texts("sendMessage"); len(got) != 2 || !strings.Contains(got[1], "followed by premium emoji") {
		t.Errorf("reply = %q, want instructions", got)
	}
}
