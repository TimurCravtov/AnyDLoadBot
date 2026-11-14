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
	downloadFailed = "Sorry, I couldn't download that. Please try again later."

	downloadTimeout = 2 * time.Minute
)

func (b *Bot) supported() string {
	return strings.Join(b.providers.Names(), ", ")
}

func (b *Bot) handleStart(ctx context.Context, api *tgbot.Bot, u *models.Update) {
	b.reply(ctx, api, u, "Hi! Send me a link and I'll send the video back.\nSupported sites: "+b.supported()+"\n\nUse /help for more info.")
}

func (b *Bot) handleHelp(ctx context.Context, api *tgbot.Bot, u *models.Update) {
	b.reply(ctx, api, u, "Send me a link to a video and I'll download it.\nSupported sites: "+b.supported()+"\n\n/start - greeting\n/help - this message")
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
		switch {
		case errors.Is(err, downloader.ErrNoMedia):
			b.reply(ctx, api, u, noMediaText)
		case errors.Is(err, downloader.ErrTooLarge):
			b.reply(ctx, api, u, tooLargeText)
		default:
			log.Error("download video", "err", err)
			b.reply(ctx, api, u, downloadFailed)
		}
		return
	}

	_, err = api.SendVideo(ctx, &tgbot.SendVideoParams{
		ChatID:            chatID,
		Video:             &models.InputFileUpload{Filename: media.Filename, Data: bytes.NewReader(media.Data)},
		SupportsStreaming: true,
		ReplyParameters:   &models.ReplyParameters{MessageID: u.Message.ID},
	})
	if err != nil {
		log.Error("send video", "err", err)
		b.reply(ctx, api, u, downloadFailed)
	}
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
