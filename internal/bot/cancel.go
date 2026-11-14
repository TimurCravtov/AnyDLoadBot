package bot

import (
	"context"
	"fmt"
	"strconv"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	// cancelData is the callback data of every Cancel button. Which download it
	// belongs to follows from the message the button is on.
	cancelData    = "cancel"
	cancelledText = "✖ Cancelled"
	statusText    = "⏳ Downloading…"
)

func cancelKeyboard() *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
		{Text: "✖ Cancel", CallbackData: cancelData},
	}}}
}

// chatKey identifies a status message in a chat, for the job it belongs to.
func chatKey(chatID int64, messageID int) string {
	return strconv.FormatInt(chatID, 10) + ":" + strconv.Itoa(messageID)
}

// senderID is the user a message came from (the chat itself for anonymous senders).
func senderID(m *models.Message) int64 {
	if m.From != nil {
		return m.From.ID
	}
	return m.Chat.ID
}

// limitText explains why a download was refused.
func (b *Bot) limitText(err error) string {
	if err == errUserLimit {
		return fmt.Sprintf("You already have %d downloads running. Wait for one to finish, or send /cancel to stop them.", b.jobs.maxUser)
	}
	return "I'm handling a lot of downloads right now. Please try again in a minute."
}

// handleCancel stops all of the sender's running downloads (/cancel).
func (b *Bot) handleCancel(ctx context.Context, api *tgbot.Bot, u *models.Update) {
	if u.Message == nil {
		return
	}
	switch n := b.jobs.cancelUser(senderID(u.Message)); n {
	case 0:
		b.reply(ctx, api, u, "You have no downloads running.")
	case 1:
		b.reply(ctx, api, u, "Cancelling your download…")
	default:
		b.reply(ctx, api, u, fmt.Sprintf("Cancelling your %d downloads…", n))
	}
}

// handleCancelButton stops the download whose status message carried the button.
func (b *Bot) handleCancelButton(ctx context.Context, api *tgbot.Bot, u *models.Update) {
	q := u.CallbackQuery

	key := q.InlineMessageID // inline messages are identified by this alone
	if key == "" && q.Message.Message != nil {
		key = chatKey(q.Message.Message.Chat.ID, q.Message.Message.ID)
	}

	text := "Nothing to cancel."
	if key != "" && b.jobs.cancelKey(q.From.ID, key) {
		text = "Cancelling…"
	}
	if _, err := api.AnswerCallbackQuery(ctx, &tgbot.AnswerCallbackQueryParams{CallbackQueryID: q.ID, Text: text}); err != nil {
		b.log.Error("answer callback query", "err", err)
	}
}

// sendStatus shows "downloading" with a Cancel button as a reply to the user's
// message and returns the status message's ID (0 if it couldn't be sent).
func (b *Bot) sendStatus(ctx context.Context, api *tgbot.Bot, chatID int64, replyTo int) int {
	msg, err := api.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID:          chatID,
		Text:            statusText,
		ReplyMarkup:     cancelKeyboard(),
		ReplyParameters: &models.ReplyParameters{MessageID: replyTo},
	})
	if err != nil || msg == nil {
		return 0 // the download still works, it just can't be cancelled with a button
	}
	return msg.ID
}

// endStatus removes the status message, or marks it cancelled. It uses its own
// timeout because the download context may already be done.
func (b *Bot) endStatus(ctx context.Context, api *tgbot.Bot, chatID int64, messageID int, cancelled bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), editTimeout)
	defer cancel()

	var err error
	if cancelled {
		_, err = api.EditMessageText(ctx, &tgbot.EditMessageTextParams{ChatID: chatID, MessageID: messageID, Text: cancelledText})
	} else {
		_, err = api.DeleteMessage(ctx, &tgbot.DeleteMessageParams{ChatID: chatID, MessageID: messageID})
	}
	if err != nil {
		b.log.Warn("update status message", "err", err)
	}
}
