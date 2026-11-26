package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"anydloadbot/internal/downloader"
)

// Premium (custom) emoji in status messages. Telegram only shows them if the
// bot's owner has Telegram Premium (or the bot has a username bought on
// Fragment), and only in messages the bot sends itself: inline placeholders
// are sent through the user, so they keep the plain emoji.

// statusKind is what a status message says is happening.
type statusKind int

const (
	statusPreparing statusKind = iota
	statusDownloading
	statusProcessing
	statusUploading
	statusCancelled
	numStatusKinds
)

// defaultEmoji is the plain emoji each status message starts with.
var defaultEmoji = [numStatusKinds]string{"⏳", "⏳", "🔧", "📤", "✖"}

// statusNames name the statuses in logs.
var statusNames = [numStatusKinds]string{"preparing", "downloading", "processing", "uploading", "cancelled"}

// loadingEmojiID is the custom emoji shown while a download is running.
const loadingEmojiID = "5433910799477255476"

// statusEmojiIDs are the custom emoji for each status; "" keeps the plain
// emoji. Send the bot /emoji with premium emoji to find their IDs.
var statusEmojiIDs = [numStatusKinds]string{
	statusPreparing:   loadingEmojiID,
	statusDownloading: loadingEmojiID,
	statusProcessing:  loadingEmojiID,
	statusUploading:   loadingEmojiID,
}

// withStatusEmoji replaces statusEmojiIDs (used by tests).
func withStatusEmoji(ids [numStatusKinds]string) Option {
	return func(o *options) { o.statusEmoji = &ids }
}

// customEmoji is a custom emoji and the plain emoji Telegram shows where it
// can't (the one the emoji's sticker names, as Telegram requires).
type customEmoji struct{ id, fallback string }

// loadEmoji looks up the configured custom emoji, for their plain fallback
// emoji. IDs Telegram doesn't know are dropped, so a typo can't break status
// messages. Called once in Run, before polling starts.
func (b *Bot) loadEmoji(ctx context.Context) {
	var ids []string
	for _, e := range b.emoji {
		if e.id != "" {
			ids = append(ids, e.id)
		}
	}
	if len(ids) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, editTimeout)
	defer cancel()
	stickers, err := b.api.GetCustomEmojiStickers(ctx, &tgbot.GetCustomEmojiStickersParams{CustomEmojiIDs: ids})
	if err != nil {
		b.log.Warn("could not look up the status emoji; using plain emoji", "err", err)
		b.emoji = [numStatusKinds]customEmoji{}
		return
	}
	found := map[string]string{}
	for _, s := range stickers {
		if s != nil && s.Emoji != "" {
			found[s.CustomEmojiID] = s.Emoji
		}
	}
	for k := range b.emoji {
		id := b.emoji[k].id
		if id == "" {
			continue
		}
		fallback, ok := found[id]
		if !ok {
			b.log.Warn("unknown custom emoji ID, using the plain emoji", "status", statusNames[k], "id", id)
			b.emoji[k] = customEmoji{}
			continue
		}
		b.emoji[k].fallback = fallback
	}
	b.log.Info("custom status emoji loaded", "count", len(found))
}

// statusMessage puts kind's custom emoji in place of the plain one text
// starts with. Without a custom emoji, text is returned unchanged.
func (b *Bot) statusMessage(kind statusKind, text string) (string, []models.MessageEntity) {
	e := b.emoji[kind]
	if e.id == "" || b.emojiOff.Load() {
		return text, nil
	}
	rest, ok := strings.CutPrefix(text, defaultEmoji[kind])
	if !ok {
		return text, nil
	}
	return e.fallback + rest, []models.MessageEntity{{
		Type: models.MessageEntityTypeCustomEmoji, Offset: 0, Length: utf16Len(e.fallback), CustomEmojiID: e.id,
	}}
}

// withEmoji makes an API call with custom emoji entities. If Telegram refuses
// it and the same call without them works, this bot may not use custom emoji
// (the owner has no Premium): they are switched off for good, with a warning.
func (b *Bot) withEmoji(entities []models.MessageEntity, call func([]models.MessageEntity) error) error {
	err := call(entities)
	if err == nil || len(entities) == 0 || !errors.Is(err, tgbot.ErrorBadRequest) || strings.Contains(err.Error(), "not modified") {
		return err
	}
	if call(nil) != nil {
		return err
	}
	if b.emojiOff.CompareAndSwap(false, true) {
		b.log.Warn("Telegram refused custom emoji; status messages use plain emoji from now on",
			"err", err, "hint", "the bot's owner needs Telegram Premium")
	}
	return nil
}

// kindOf is the status a progress snapshot shows.
func kindOf(p downloader.Progress) statusKind {
	switch p.Stage {
	case downloader.StageDownloading:
		return statusDownloading
	case downloader.StageProcessing:
		return statusProcessing
	case downloader.StageUploading:
		return statusUploading
	default:
		return statusPreparing
	}
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// handleEmoji (/emoji) replies with the IDs of the custom emoji in the
// message, one per line, to put into statusEmojiIDs.
func (b *Bot) handleEmoji(ctx context.Context, api *tgbot.Bot, u *models.Update) {
	m := u.Message
	if m == nil {
		return
	}
	text := utf16.Encode([]rune(m.Text))

	var lines []string
	for _, e := range m.Entities {
		if e.Type != models.MessageEntityTypeCustomEmoji || e.CustomEmojiID == "" {
			continue
		}
		emoji := "?"
		if e.Offset >= 0 && e.Offset+e.Length <= len(text) {
			emoji = string(utf16.Decode(text[e.Offset : e.Offset+e.Length]))
		}
		lines = append(lines, fmt.Sprintf("%s %s", emoji, e.CustomEmojiID))
	}

	if len(lines) == 0 {
		b.reply(ctx, api, u, "Send /emoji followed by premium emoji, and I'll reply with their IDs.")
		return
	}
	b.send(ctx, api, u, "<pre>"+strings.Join(lines, "\n")+"</pre>", models.ParseModeHTML)
}
