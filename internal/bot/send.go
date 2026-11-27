package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"anydloadbot/internal/downloader"
)

// outgoing is a file to send: bytes to upload, a URL for Telegram to fetch, or
// the file_id of a file Telegram already has.
type outgoing struct {
	kind      downloader.Kind
	file      models.InputFile
	title     string // caption in chat (video/photo); the track title (audio)
	performer string // audio only
	duration  int    // audio only, seconds
}

func fromMedia(m *downloader.Media) outgoing {
	var file models.InputFile = &models.InputFileUpload{Filename: m.Filename, Data: bytes.NewReader(m.Data)}
	if m.Path != "" {
		// A local Bot API server reads the file itself (both containers mount
		// the media volume at the same path): nothing is uploaded from here.
		file = &models.InputFileString{Data: "file://" + m.Path}
	}
	return outgoing{
		kind: m.Kind, file: file,
		title: m.Title, performer: m.Performer, duration: m.Duration,
	}
}

func fromSource(s *downloader.Source) outgoing {
	return outgoing{kind: s.Kind, file: &models.InputFileString{Data: s.URL}, title: s.Title}
}

func fromCache(f cachedFile) outgoing {
	return outgoing{
		kind: f.kind, file: &models.InputFileString{Data: f.fileID},
		title: f.title, performer: f.performer, duration: f.duration,
	}
}

// deliver sends o to a chat, as a reply to replyTo. replyTo 0 means the
// storage chat: no reply, no caption, no notification. It returns the sent
// file for the cache; its fileID is empty if Telegram's answer had none (it
// may turn a silent video into an animation, say), which only means it can't
// be cached.
func (b *Bot) deliver(ctx context.Context, api *tgbot.Bot, chatID int64, replyTo int, o outgoing) (cachedFile, error) {
	var (
		reply   *models.ReplyParameters
		caption string
	)
	silent := replyTo == 0
	if !silent {
		reply = &models.ReplyParameters{MessageID: replyTo}
		caption = truncate(o.title, maxCaptionRunes)
	}

	var (
		msg *models.Message
		err error
	)
	switch o.kind {
	case downloader.KindPhoto:
		msg, err = api.SendPhoto(ctx, &tgbot.SendPhotoParams{
			ChatID: chatID, Photo: o.file, Caption: caption, ReplyParameters: reply, DisableNotification: silent,
		})
	case downloader.KindAudio:
		msg, err = api.SendAudio(ctx, &tgbot.SendAudioParams{
			ChatID: chatID, Audio: o.file, Title: o.title, Performer: o.performer, Duration: o.duration,
			ReplyParameters: reply, DisableNotification: silent,
		})
	default:
		msg, err = api.SendVideo(ctx, &tgbot.SendVideoParams{
			ChatID: chatID, Video: o.file, Caption: caption, SupportsStreaming: true,
			ReplyParameters: reply, DisableNotification: silent,
		})
	}
	if err != nil {
		return cachedFile{}, err
	}

	f := cachedFile{kind: o.kind, title: o.title, performer: o.performer, duration: o.duration}
	switch {
	case o.kind == downloader.KindPhoto && len(msg.Photo) > 0:
		f.fileID = msg.Photo[len(msg.Photo)-1].FileID // the largest size
	case o.kind == downloader.KindAudio && msg.Audio != nil:
		f.fileID = msg.Audio.FileID
	case o.kind == downloader.KindVideo && msg.Video != nil:
		f.fileID = msg.Video.FileID
	}
	return f, nil
}

// sendDirect asks the provider for a direct media URL and has Telegram fetch
// it, so the file never passes through this server. Any error means the
// caller should download and upload the file instead.
func (b *Bot) sendDirect(ctx context.Context, api *tgbot.Bot, chatID int64, replyTo int, l downloader.Linker, link string) (cachedFile, error) {
	src, err := l.MediaURL(ctx, link)
	if err != nil {
		return cachedFile{}, fmt.Errorf("no direct URL: %w", err)
	}
	downloader.Report(ctx, downloader.Progress{Stage: downloader.StageUploading})
	f, err := b.deliver(ctx, api, chatID, replyTo, fromSource(src))
	if err != nil {
		return cachedFile{}, fmt.Errorf("telegram could not fetch the direct URL: %w", err)
	}
	return f, nil
}

// cacheKey identifies a link's file in the cache, per format.
func cacheKey(link string, audio bool) string {
	if audio {
		return link + "|mp3"
	}
	return link
}

// errNoFileID means Telegram accepted a file but its answer had no file_id.
var errNoFileID = errors.New("storage chat reply has no file_id")

// inputMedia builds the editMessageMedia payload that swaps an inline
// placeholder for the stored file. Inline messages are sent without a caption
// (the title is shown in the results list instead); an MP3 keeps its track
// title and performer, which are part of the audio itself.
func inputMedia(f cachedFile) models.InputMedia {
	switch f.kind {
	case downloader.KindPhoto:
		return &models.InputMediaPhoto{Media: f.fileID}
	case downloader.KindAudio:
		return &models.InputMediaAudio{Media: f.fileID, Title: f.title, Performer: f.performer, Duration: f.duration}
	default:
		return &models.InputMediaVideo{Media: f.fileID, SupportsStreaming: true}
	}
}
