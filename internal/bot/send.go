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

func upload(m *downloader.Media) *models.InputFileUpload {
	return &models.InputFileUpload{Filename: m.Filename, Data: bytes.NewReader(m.Data)}
}

// sendMedia sends m to a chat as a reply, using the method that matches its kind.
func (b *Bot) sendMedia(ctx context.Context, api *tgbot.Bot, chatID int64, replyTo int, m *downloader.Media) error {
	reply := &models.ReplyParameters{MessageID: replyTo}

	var err error
	switch m.Kind {
	case downloader.KindPhoto:
		_, err = api.SendPhoto(ctx, &tgbot.SendPhotoParams{
			ChatID: chatID, Photo: upload(m), Caption: truncate(m.Title, maxCaptionRunes), ReplyParameters: reply,
		})
	case downloader.KindAudio:
		_, err = api.SendAudio(ctx, &tgbot.SendAudioParams{
			ChatID: chatID, Audio: upload(m), Title: m.Title, Performer: m.Performer, Duration: m.Duration,
			ReplyParameters: reply,
		})
	default:
		_, err = api.SendVideo(ctx, &tgbot.SendVideoParams{
			ChatID: chatID, Video: upload(m), Caption: truncate(m.Title, maxCaptionRunes), SupportsStreaming: true,
			ReplyParameters: reply,
		})
	}
	return err
}

// uploadToStorage sends m to the storage chat and returns the file_id Telegram
// assigned, which can then be used in inline messages (they can't carry uploads).
func (b *Bot) uploadToStorage(ctx context.Context, api *tgbot.Bot, m *downloader.Media) (string, error) {
	var (
		msg *models.Message
		err error
	)
	switch m.Kind {
	case downloader.KindPhoto:
		msg, err = api.SendPhoto(ctx, &tgbot.SendPhotoParams{
			ChatID: b.storageChatID, Photo: upload(m), DisableNotification: true,
		})
	case downloader.KindAudio:
		msg, err = api.SendAudio(ctx, &tgbot.SendAudioParams{
			ChatID: b.storageChatID, Audio: upload(m), Title: m.Title, Performer: m.Performer, Duration: m.Duration,
			DisableNotification: true,
		})
	default:
		msg, err = api.SendVideo(ctx, &tgbot.SendVideoParams{
			ChatID: b.storageChatID, Video: upload(m), SupportsStreaming: true, DisableNotification: true,
		})
	}
	if err != nil {
		return "", fmt.Errorf("upload to storage chat: %w", err)
	}

	switch {
	case m.Kind == downloader.KindPhoto && len(msg.Photo) > 0:
		return msg.Photo[len(msg.Photo)-1].FileID, nil // the largest size
	case m.Kind == downloader.KindAudio && msg.Audio != nil:
		return msg.Audio.FileID, nil
	case m.Kind == downloader.KindVideo && msg.Video != nil:
		return msg.Video.FileID, nil
	}
	return "", errors.New("storage chat reply has no file_id")
}

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
