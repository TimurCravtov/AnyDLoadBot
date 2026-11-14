package bot

import (
	"context"
	"errors"
	"regexp"
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
	b.reply(ctx, api, u, "Hi! Send me a link and I'll send the video or image back.\nSupported sites: "+b.supported()+"\n\nUse /help for more info.")
}

func (b *Bot) handleHelp(ctx context.Context, api *tgbot.Bot, u *models.Update) {
	text := "Send me a link to a video or image and I'll download it.\nSupported sites: " + b.supported() +
		"\n\nAdd \"mp3\" after a link to get just the audio (" + b.audioSites() + ")." +
		"\n\nInline: type my @username followed by a link in any chat." +
		"\n\n/start - greeting\n/help - this message"
	b.reply(ctx, api, u, text)
}

// handleDefault handles every non-command message: it looks for a link that a
// provider supports and, if found, downloads and sends the video.
func (b *Bot) handleDefault(ctx context.Context, api *tgbot.Bot, u *models.Update) {
	if u.Message == nil {
		return
	}

	text := u.Message.Text + " " + u.Message.Caption
	provider, link, ok := b.providers.Find(text)
	if !ok {
		b.reply(ctx, api, u, "Send me a link from a supported site ("+b.supported()+"), or use /help.")
		return
	}

	// "<link> mp3" asks for the audio track instead of the video.
	audio := wantsAudio(text)
	var download func(context.Context, string) (*downloader.Media, error) = provider.Download
	if audio {
		ad, ok := provider.(downloader.AudioDownloader)
		if !ok {
			b.reply(ctx, api, u, "MP3 isn't available for "+provider.Name()+". It works for: "+b.audioSites()+".")
			return
		}
		download = ad.DownloadAudio
	}

	chatID := u.Message.Chat.ID
	log := b.log.With("chat_id", chatID, "provider", provider.Name(), "link", link, "audio", audio)
	log.Info("download requested")
	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	action := models.ChatActionUploadVideo
	if audio {
		action = models.ChatActionUploadVoice
	}
	_, _ = api.SendChatAction(ctx, &tgbot.SendChatActionParams{ChatID: chatID, Action: action})

	media, err := download(ctx, link)
	if err != nil {
		text, unexpected := failureText(err)
		if unexpected {
			log.Error("download failed", "err", err, "took", time.Since(start).Round(time.Millisecond))
		} else {
			log.Warn("download rejected", "err", err)
		}
		b.reply(ctx, api, u, text)
		return
	}

	if err := b.sendMedia(ctx, api, chatID, u.Message.ID, media); err != nil {
		log.Error("send media", "err", err)
		b.reply(ctx, api, u, downloadFailed)
		return
	}
	log.Info("media sent", "kind", media.Kind, "bytes", len(media.Data), "took", time.Since(start).Round(time.Millisecond))
}

var (
	mp3Word = regexp.MustCompile(`(?i)(^|[^\pL\pN])mp3($|[^\pL\pN])`)
	urlRe   = regexp.MustCompile(`https?://\S+`)
)

// wantsAudio reports whether the message asks for MP3, e.g. "<link> mp3".
// Links are ignored so a URL that happens to contain "mp3" doesn't count.
func wantsAudio(text string) bool { return mp3Word.MatchString(urlRe.ReplaceAllString(text, " ")) }

// audioSites lists the providers that can extract MP3.
func (b *Bot) audioSites() string {
	return strings.Join(b.providers.NamesWhere(func(p downloader.Provider) bool {
		_, ok := p.(downloader.AudioDownloader)
		return ok
	}), ", ")
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
