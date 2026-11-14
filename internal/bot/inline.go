package bot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"anydloadbot/internal/downloader"
)

const (
	inlineTimeout = 10 * time.Second
	// Successful answers are cached by Telegram so repeat queries are instant;
	// empty/failed ones are cached only briefly so typing a link isn't penalised.
	inlineCacheOK    = 300
	inlineCacheEmpty = 5

	downloadingCaption = "⏳ Downloading…"
	editTimeout        = 15 * time.Second
)

func isInlineQuery(u *models.Update) bool        { return u.InlineQuery != nil }
func isChosenInlineResult(u *models.Update) bool { return u.ChosenInlineResult != nil }

// Inline mode has two flows:
//
//  1. Direct URL (downloader.Resolver): the answer is a video result that
//     Telegram fetches itself. Nothing else to do.
//  2. Placeholder (downloader.Previewer): the answer is a photo with a
//     "downloading" caption. When the user picks it Telegram sends a
//     chosen_inline_result (enable it with /setinlinefeedback in BotFather);
//     handleChosenInline then downloads the video, uploads it once to the
//     storage chat to get a file_id, and swaps the placeholder for the video.

// handleInline answers "@bot <link>" queries.
func (b *Bot) handleInline(ctx context.Context, api *tgbot.Bot, u *models.Update) {
	q := u.InlineQuery

	results := b.inlineResults(ctx, q.Query)
	cache := inlineCacheOK
	if len(results) == 0 {
		cache = inlineCacheEmpty
	}

	_, err := api.AnswerInlineQuery(ctx, &tgbot.AnswerInlineQueryParams{
		InlineQueryID: q.ID,
		Results:       results,
		CacheTime:     cache,
	})
	if err != nil {
		b.log.Error("answer inline query", "err", err)
	}
}

// inlineResults never returns nil: Telegram requires a JSON array.
func (b *Bot) inlineResults(ctx context.Context, query string) []models.InlineQueryResult {
	results := []models.InlineQueryResult{}

	provider, link, ok := b.providers.Find(query)
	if !ok {
		return results
	}

	if resolver, ok := provider.(downloader.Resolver); ok {
		return b.directResult(ctx, results, provider, resolver, link)
	}
	if previewer, ok := provider.(downloader.Previewer); ok && b.storageChatID != 0 {
		return b.placeholderResult(ctx, results, previewer, link)
	}

	b.log.Debug("provider does not support inline mode", "provider", provider.Name())
	return results
}

func (b *Bot) directResult(ctx context.Context, results []models.InlineQueryResult, provider downloader.Provider, resolver downloader.Resolver, link string) []models.InlineQueryResult {
	ctx, cancel := context.WithTimeout(ctx, inlineTimeout)
	defer cancel()

	src, err := resolver.Resolve(ctx, link)
	if err != nil {
		if !errors.Is(err, downloader.ErrNoMedia) {
			b.log.Error("resolve inline link", "provider", provider.Name(), "link", link, "err", err)
		}
		return results
	}
	if src.ThumbnailURL == "" {
		// Telegram rejects inline videos without a thumbnail.
		b.log.Warn("no thumbnail for inline result", "provider", provider.Name(), "link", link)
		return results
	}

	return append(results, &models.InlineQueryResultVideo{
		ID:           resultID(src.URL), // result IDs are limited to 64 bytes
		VideoURL:     src.URL,
		MimeType:     src.MimeType,
		ThumbnailURL: src.ThumbnailURL,
		Title:        src.Title,
	})
}

func (b *Bot) placeholderResult(ctx context.Context, results []models.InlineQueryResult, previewer downloader.Previewer, link string) []models.InlineQueryResult {
	ctx, cancel := context.WithTimeout(ctx, inlineTimeout)
	defer cancel()

	p, ok := previewer.Preview(ctx, link)
	if !ok {
		return results
	}

	return append(results, &models.InlineQueryResultPhoto{
		ID:           resultID(link),
		PhotoURL:     p.ThumbnailURL,
		ThumbnailURL: p.ThumbnailURL,
		Title:        p.Title,
		Description:  p.Description,
		Caption:      downloadingCaption,
		// Telegram only reports inline_message_id (needed to edit the message
		// later) for messages that carry an inline keyboard.
		ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: "Source", URL: link},
		}}},
	})
}

func resultID(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// handleChosenInline completes the placeholder flow: it replaces the
// "downloading" photo with the video once it has been downloaded.
func (b *Bot) handleChosenInline(ctx context.Context, api *tgbot.Bot, u *models.Update) {
	c := u.ChosenInlineResult
	if c.InlineMessageID == "" || b.storageChatID == 0 {
		return // a direct-URL result (no keyboard) or storage not configured
	}

	provider, link, ok := b.providers.Find(c.Query)
	if !ok {
		return
	}
	log := b.log.With("provider", provider.Name(), "link", link, "user_id", c.From.ID)
	log.Info("inline result chosen, downloading")
	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	file, err := b.storedVideo(ctx, api, provider, link)
	if err != nil {
		text, unexpected := failureText(err)
		if unexpected {
			log.Error("inline download failed", "err", err, "took", time.Since(start).Round(time.Millisecond))
		} else {
			log.Warn("inline download rejected", "err", err)
		}
		b.editCaption(ctx, api, c.InlineMessageID, text)
		return
	}

	_, err = api.EditMessageMedia(ctx, &tgbot.EditMessageMediaParams{
		InlineMessageID: c.InlineMessageID,
		Media: &models.InputMediaVideo{
			Media:             file.fileID,
			Caption:           truncate(file.title, maxCaptionRunes),
			SupportsStreaming: true,
		},
	})
	if err != nil {
		log.Error("edit inline message", "err", err)
		b.editCaption(ctx, api, c.InlineMessageID, downloadFailed)
		return
	}
	log.Info("inline video sent", "took", time.Since(start).Round(time.Millisecond))
}

// storedVideo downloads the link and uploads it to the storage chat, returning
// the file_id Telegram assigned. Results are cached per link.
func (b *Bot) storedVideo(ctx context.Context, api *tgbot.Bot, provider downloader.Provider, link string) (cachedFile, error) {
	if f, ok := b.files.get(link); ok {
		return f, nil
	}

	media, err := provider.Download(ctx, link)
	if err != nil {
		return cachedFile{}, err
	}

	msg, err := api.SendVideo(ctx, &tgbot.SendVideoParams{
		ChatID:              b.storageChatID,
		Video:               &models.InputFileUpload{Filename: media.Filename, Data: bytes.NewReader(media.Data)},
		SupportsStreaming:   true,
		DisableNotification: true,
	})
	if err != nil {
		return cachedFile{}, fmt.Errorf("upload to storage chat: %w", err)
	}
	if msg.Video == nil || msg.Video.FileID == "" {
		return cachedFile{}, errors.New("storage chat reply has no video file_id")
	}

	f := cachedFile{fileID: msg.Video.FileID, title: media.Title}
	b.files.put(link, f)
	return f, nil
}

// editCaption shows a status/error on the placeholder. It uses its own
// timeout because the download context may already have expired.
func (b *Bot) editCaption(ctx context.Context, api *tgbot.Bot, inlineMessageID, text string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), editTimeout)
	defer cancel()

	if _, err := api.EditMessageCaption(ctx, &tgbot.EditMessageCaptionParams{
		InlineMessageID: inlineMessageID,
		Caption:         text,
	}); err != nil {
		b.log.Error("edit inline caption", "err", err)
	}
}
