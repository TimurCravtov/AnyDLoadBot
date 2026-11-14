package bot

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"anydloadbot/internal/downloader"
)

const (
	noMediaText    = "I couldn't find any downloadable media at that link."
	tooLargeText   = "That file is too large for Telegram bots (50 MB limit)."
	tooLongText    = "That video is too long for me to download."
	downloadFailed = "Sorry, I couldn't download that. Please try again later."

	// Long videos need yt-dlp to download and merge streams, which takes a while.
	downloadTimeout = 5 * time.Minute
	maxCaptionRunes = 200
)

func (b *Bot) supported() string {
	return strings.Join(b.providers.Names(), ", ")
}

func (b *Bot) handleStart(ctx context.Context, api *tgbot.Bot, u *models.Update) {
	b.reply(ctx, api, u, "Hi! Send me a link and I'll send the video back.\nSupported sites: "+b.supported()+"\n\nUse /help for more info.")
}

func (b *Bot) handleHelp(ctx context.Context, api *tgbot.Bot, u *models.Update) {
	b.reply(ctx, api, u, "Send me a link to a video and I'll download it.\nSupported sites: "+b.supported()+"\n\nInline: type my @username followed by a link in any chat.\n\n/start - greeting\n/help - this message")
}

// handleDefault handles every non-command message: it looks for a link that a
// provider supports and, if found, downloads and sends the video.
func (b *Bot) handleDefault(ctx context.Context, api *tgbot.Bot, u *models.Update) {
	if u.Message == nil {
		return
	}

	provider, link, ok := b.providers.Find(u.Message.Text + " " + u.Message.Caption)
	if !ok {
		b.reply(ctx, api, u, "Send me a link from a supported site ("+b.supported()+"), or use /help.")
		return
	}

	chatID := u.Message.Chat.ID
	log := b.log.With("chat_id", chatID, "provider", provider.Name(), "link", link)

	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	_, _ = api.SendChatAction(ctx, &tgbot.SendChatActionParams{
		ChatID: chatID,
		Action: models.ChatActionUploadVideo,
	})

	media, err := provider.Download(ctx, link)
	if err != nil {
		text, unexpected := failureText(err)
		if unexpected {
			log.Error("download video", "err", err)
		}
		b.reply(ctx, api, u, text)
		return
	}

	_, err = api.SendVideo(ctx, &tgbot.SendVideoParams{
		ChatID:            chatID,
		Video:             &models.InputFileUpload{Filename: media.Filename, Data: bytes.NewReader(media.Data)},
		Caption:           truncate(media.Title, maxCaptionRunes),
		SupportsStreaming: true,
		ReplyParameters:   &models.ReplyParameters{MessageID: u.Message.ID},
	})
	if err != nil {
		log.Error("send video", "err", err)
		b.reply(ctx, api, u, downloadFailed)
	}
}

// failureText maps a download error to a user-facing message. unexpected is
// true for errors worth logging (anything but "the video can't be fetched").
func failureText(err error) (text string, unexpected bool) {
	switch {
	case errors.Is(err, downloader.ErrNoMedia):
		return noMediaText, false
	case errors.Is(err, downloader.ErrTooLarge):
		return tooLargeText, false
	case errors.Is(err, downloader.ErrTooLong):
		return tooLongText, false
	default:
		return downloadFailed, true
	}
}

func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes-1]) + "…"
}

// reply sends text to the chat the update came from. Updates without a message are ignored.
func (b *Bot) reply(ctx context.Context, api *tgbot.Bot, u *models.Update, text string) {
	if u.Message == nil {
		return
	}
	_, err := api.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID: u.Message.Chat.ID,
		Text:   text,
	})
	if err != nil {
		b.log.Error("send message", "chat_id", u.Message.Chat.ID, "err", err)
	}
}
