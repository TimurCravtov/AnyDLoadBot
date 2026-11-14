package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
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

	downloadingText = "⏳ Downloading…"
	editTimeout     = 15 * time.Second
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
		return b.placeholderResults(ctx, results, provider, previewer, link)
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
		// Telegram rejects inline videos and photos without a thumbnail.
		b.log.Warn("no thumbnail for inline result", "provider", provider.Name(), "link", link)
		return results
	}

	id := resultID(src.URL) // result IDs are limited to 64 bytes
	if src.Kind == downloader.KindPhoto {
		return append(results, &models.InlineQueryResultPhoto{
			ID:           id,
			PhotoURL:     src.URL,
			ThumbnailURL: src.ThumbnailURL,
			Title:        src.Title,
			Caption:      truncate(src.Title, maxCaptionRunes),
		})
	}
	return append(results, &models.InlineQueryResultVideo{
		ID:           id,
		VideoURL:     src.URL,
		MimeType:     src.MimeType,
		ThumbnailURL: src.ThumbnailURL,
		Title:        src.Title,
		Caption:      truncate(src.Title, maxCaptionRunes),
	})
}

// Placeholder result IDs start with the format, so handleChosenInline knows what
// the user picked. The rest is a hash of the link (IDs are limited to 64 bytes).
const (
	videoPrefix = "vid:"
	mp3Prefix   = "mp3:"
)

// placeholderResults offers the video and, if the provider can extract it, an MP3.
func (b *Bot) placeholderResults(ctx context.Context, results []models.InlineQueryResult, provider downloader.Provider, previewer downloader.Previewer, link string) []models.InlineQueryResult {
	ctx, cancel := context.WithTimeout(ctx, inlineTimeout)
	defer cancel()

	p, ok := previewer.Preview(ctx, link)
	if !ok {
		return results
	}

	results = append(results, placeholder(videoPrefix, p.Title, p.Description, downloadingText, p.ThumbnailURL, link))
	if _, ok := provider.(downloader.AudioDownloader); ok {
		results = append(results, placeholder(mp3Prefix, "🎵 MP3 · "+p.Title, "Audio with title and cover art", "⏳ Preparing MP3…", p.ThumbnailURL, link))
	}
	return results
}

// placeholder is a text row in the results list (thumbnail, title, description).
// Photo results would render as a bare image grid with no text. When picked it
// sends a "downloading" text message, which handleChosenInline later replaces
// with the media (Telegram allows editing a text message into media).
func placeholder(prefix, title, description, status, thumbnailURL, link string) *models.InlineQueryResultArticle {
	return &models.InlineQueryResultArticle{
		ID:                  prefix + resultID(link),
		Title:               title,
		Description:         description,
		ThumbnailURL:        thumbnailURL,
		InputMessageContent: &models.InputTextMessageContent{MessageText: status},
		// Telegram only reports inline_message_id (needed to edit the message
		// later) for messages that carry an inline keyboard.
		ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: "Source", URL: link},
			{Text: "✖ Cancel", CallbackData: cancelData},
		}}},
	}
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
	audio := strings.HasPrefix(c.ResultID, mp3Prefix)
	download := provider.Download
	if audio {
		ad, ok := provider.(downloader.AudioDownloader)
		if !ok {
			return
		}
		download = ad.DownloadAudio
	}

	log := b.log.With("provider", provider.Name(), "link", link, "user_id", c.From.ID, "audio", audio)

	job, err := b.jobs.start(ctx, c.From.ID)
	if err != nil {
		log.Warn("inline download refused", "reason", err)
		b.editStatus(ctx, api, c.InlineMessageID, b.limitText(err))
		return
	}
	defer job.finish()
	job.setKey(c.InlineMessageID) // the placeholder's Cancel button identifies the job by this
	log.Info("inline result chosen, downloading")
	start := time.Now()

	parent := ctx
	ctx, cancel := context.WithTimeout(job.ctx, downloadTimeout)
	defer cancel()

	file, err := b.storedMedia(ctx, api, download, link, audio)
	if err != nil {
		if job.wasCancelled() {
			log.Info("inline download cancelled by the user")
			b.editStatus(parent, api, c.InlineMessageID, cancelledText)
			return
		}
		text, unexpected := failureText(err)
		if unexpected {
			log.Error("inline download failed", "err", err, "took", time.Since(start).Round(time.Millisecond))
		} else {
			log.Warn("inline download rejected", "err", err)
		}
		b.editStatus(ctx, api, c.InlineMessageID, text)
		return
	}

	_, err = api.EditMessageMedia(ctx, &tgbot.EditMessageMediaParams{
		InlineMessageID: c.InlineMessageID,
		Media:           inputMedia(file),
	})
	if err != nil {
		log.Error("edit inline message", "err", err)
		b.editStatus(ctx, api, c.InlineMessageID, downloadFailed)
		return
	}
	log.Info("inline media sent", "took", time.Since(start).Round(time.Millisecond))
}

// storedMedia downloads the link and uploads it to the storage chat, returning
// the file_id Telegram assigned. Results are cached per link and format.
func (b *Bot) storedMedia(ctx context.Context, api *tgbot.Bot, download func(context.Context, string) (*downloader.Media, error), link string, audio bool) (cachedFile, error) {
	key := link
	if audio {
		key += "|mp3"
	}
	if f, ok := b.files.get(key); ok {
		return f, nil
	}

	media, err := download(ctx, link)
	if err != nil {
		return cachedFile{}, err
	}
	fileID, err := b.uploadToStorage(ctx, api, media)
	if err != nil {
		return cachedFile{}, err
	}

	f := cachedFile{kind: media.Kind, fileID: fileID, title: media.Title, performer: media.Performer, duration: media.Duration}
	b.files.put(key, f)
	return f, nil
}

// editStatus shows a status or error on the placeholder text message. It uses
// its own timeout because the download context may already have expired.
func (b *Bot) editStatus(ctx context.Context, api *tgbot.Bot, inlineMessageID, text string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), editTimeout)
	defer cancel()

	if _, err := api.EditMessageText(ctx, &tgbot.EditMessageTextParams{
		InlineMessageID: inlineMessageID,
		Text:            text,
	}); err != nil {
		b.log.Error("edit inline status", "err", err)
	}
}
