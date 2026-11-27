package bot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"anydloadbot/internal/downloader"
)

const (
	noMediaText    = "I couldn't find any downloadable media at that link."
	tooLargeText   = "That file is too large to send (over %d MB)."
	tooLongText    = "That video is too long for me to download."
	downloadFailed = "Sorry, I couldn't download that. Please try again later."

	// Long videos need yt-dlp to download and merge streams, which takes a
	// while, and files too big for the size limit are compressed first.
	downloadTimeout = 20 * time.Minute
	maxCaptionRunes = 200
)

func (b *Bot) supported() string {
	return strings.Join(b.providers.Names(), ", ")
}

func (b *Bot) handleStart(ctx context.Context, api *tgbot.Bot, u *models.Update) {
	b.reply(ctx, api, u, b.startText())
}

func (b *Bot) handleHelp(ctx context.Context, api *tgbot.Bot, u *models.Update) {
	b.send(ctx, api, u, b.helpText(), models.ParseModeHTML)
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
	userID := senderID(u.Message)
	log := b.log.With("chat_id", chatID, "user_id", userID, "provider", provider.Name(), "link", link, "audio", audio)

	job, err := b.jobs.start(ctx, userID)
	if err != nil {
		log.Warn("download refused", "reason", err)
		b.reply(ctx, api, u, b.limitText(err))
		return
	}
	defer job.finish()
	log.Info("download requested")
	start := time.Now()

	// The status message has a Cancel button; it goes away (or turns into
	// "Cancelled") once the download is over.
	parent := ctx
	status := b.sendStatus(ctx, api, chatID, u.Message.ID)
	if status != 0 {
		job.setKey(chatKey(chatID, status))
		defer func() { b.endStatus(parent, api, chatID, status, job.wasCancelled()) }()
	}

	ctx, cancel := context.WithTimeout(job.ctx, downloadTimeout)
	defer cancel()

	// Live progress in the status message. Deferred last, so it stops before
	// the status message is removed.
	if status != 0 {
		var stop func()
		ctx, stop = b.startProgress(ctx, func(pctx context.Context, kind statusKind, text string) {
			b.editChatStatus(pctx, api, chatID, status, kind, text)
		})
		defer stop()
	}
	// Time per step (lookup, download, ...) for the log lines below.
	ctx, timer := downloader.WithStageTimer(ctx)

	action := models.ChatActionUploadVideo
	if audio {
		action = models.ChatActionUploadVoice
	}
	_, _ = api.SendChatAction(ctx, &tgbot.SendChatActionParams{ChatID: chatID, Action: action})

	took := func() time.Duration { return time.Since(start).Round(time.Millisecond) }
	key := cacheKey(link, audio)

	// Sent before (here or inline): Telegram still has the file.
	if f, ok := b.files.get(key); ok {
		if _, err := b.deliver(ctx, api, chatID, u.Message.ID, fromCache(f)); err == nil {
			log.Info("media sent", "kind", f.kind, "via", "cache", "took", took())
			return
		} else if job.wasCancelled() {
			return
		} else {
			log.Warn("cached file not accepted, downloading again", "err", err)
		}
	}

	// Telegram fetches the file from the site itself: nothing passes through here.
	if l, ok := provider.(downloader.Linker); ok && !audio {
		f, err := b.sendDirect(ctx, api, chatID, u.Message.ID, l, link)
		if err == nil {
			if f.fileID != "" {
				b.files.put(key, f)
			}
			log.Info("media sent", append([]any{"kind", f.kind, "via", "url", "took", took()}, timer.LogAttrs()...)...)
			return
		}
		if job.wasCancelled() {
			log.Info("download cancelled by the user")
			return
		}
		log.Info("direct URL not used, downloading instead", "reason", err)
	}

	media, err := download(ctx, link)
	if err != nil {
		if job.wasCancelled() {
			log.Info("download cancelled by the user")
			return
		}
		text, unexpected := b.failureText(err)
		if unexpected {
			log.Error("download failed", append([]any{"err", err, "took", took()}, timer.LogAttrs()...)...)
		} else {
			log.Warn("download rejected", "err", err)
		}
		b.reply(ctx, api, u, text)
		return
	}

	defer media.Close()
	downloader.Report(ctx, downloader.Progress{Stage: downloader.StageUploading})
	f, err := b.deliver(withUpload(ctx), api, chatID, u.Message.ID, fromMedia(media))
	if err != nil {
		if job.wasCancelled() {
			log.Info("upload cancelled by the user")
			return
		}
		log.Error("send media", "err", err)
		b.reply(ctx, api, u, downloadFailed)
		return
	}
	if f.fileID != "" {
		b.files.put(key, f)
	}
	log.Info("media sent", append([]any{"kind", media.Kind, "via", "upload", "bytes", media.Size(), "took", took()},
		timer.LogAttrs()...)...)
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

// fileLimit is the size limit users are told about.
func (b *Bot) fileLimit() int64 {
	if b.maxFileSize > 0 {
		return b.maxFileSize
	}
	return downloader.MaxSize
}

// failureText maps a download error to a user-facing message. unexpected is
// true for errors worth logging (anything but "the video can't be fetched").
func (b *Bot) failureText(err error) (text string, unexpected bool) {
	switch {
	case errors.Is(err, downloader.ErrNoMedia):
		return noMediaText, false
	case errors.Is(err, downloader.ErrTooLarge):
		return fmt.Sprintf(tooLargeText, b.fileLimit()>>20), false
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
	b.send(ctx, api, u, text, "")
}

// send is reply with an optional parse mode (e.g. HTML).
func (b *Bot) send(ctx context.Context, api *tgbot.Bot, u *models.Update, text string, mode models.ParseMode) {
	if u.Message == nil {
		return
	}
	_, err := api.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID:    u.Message.Chat.ID,
		Text:      text,
		ParseMode: mode,
	})
	if err != nil {
		b.log.Error("send message", "chat_id", u.Message.Chat.ID, "err", err)
	}
}
