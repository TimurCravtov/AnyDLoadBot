// Package bot wires the Telegram client to the application's handlers.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"anydloadbot/internal/downloader"
)

// Bot is the application's Telegram bot.
type Bot struct {
	api           *tgbot.Bot
	log           *slog.Logger
	providers     *downloader.Registry
	storageChatID int64
	files         *fileCache
	jobs          *jobs
	username      string        // set once in Run, before polling starts
	progressEvery time.Duration // how often progress is shown; tests shorten it

	emoji    [numStatusKinds]customEmoji // custom status emoji; completed in Run (loadEmoji)
	emojiOff atomic.Bool                 // set if Telegram refuses custom emoji for this bot
}

// Default concurrent-download limits.
const (
	DefaultUserLimit  = 5
	DefaultTotalLimit = 100
)

// Option configures a Bot.
type Option func(*options)

type options struct {
	storageChatID     int64
	maxUser, maxTotal int
	apiProxy          *url.URL
	statusEmoji       *[numStatusKinds]string // nil: statusEmojiIDs
	api               []tgbot.Option
}

// WithLimits caps how many downloads may run at once: per user, and across the
// whole bot. Values <= 0 keep the defaults.
func WithLimits(perUser, total int) Option {
	return func(o *options) { o.maxUser, o.maxTotal = perUser, total }
}

// WithStorageChat sets the private chat the bot uploads downloaded videos to.
// Telegram can't upload files into inline messages, so inline mode for sites
// without a direct video URL uploads here first and then reuses the file_id.
// The chat must be one the bot can post to (e.g. the owner's chat with the bot).
func WithStorageChat(chatID int64) Option {
	return func(o *options) { o.storageChatID = chatID }
}

// WithAPIProxy sends all Telegram Bot API traffic (polling for updates,
// messages and file uploads) through a proxy (http, https or socks5 URL). Use
// it when the server can't reach api.telegram.org reliably on its own, for
// example when getUpdates keeps failing with "502 Bad Gateway".
func WithAPIProxy(proxy *url.URL) Option {
	return func(o *options) { o.apiProxy = proxy }
}

// withAPIOptions passes extra options to the Telegram client (used by tests).
func withAPIOptions(opts ...tgbot.Option) Option {
	return func(o *options) { o.api = append(o.api, opts...) }
}

// New creates a Bot that downloads links using the given providers and registers all handlers.
func New(token string, providers *downloader.Registry, log *slog.Logger, opts ...Option) (*Bot, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if o.maxUser <= 0 {
		o.maxUser = DefaultUserLimit
	}
	if o.maxTotal <= 0 {
		o.maxTotal = DefaultTotalLimit
	}

	b := &Bot{
		log:           log,
		providers:     providers,
		storageChatID: o.storageChatID,
		files:         newFileCache(maxCachedFiles),
		jobs:          newJobs(o.maxUser, o.maxTotal),
		progressEvery: progressInterval,
	}
	ids := statusEmojiIDs
	if o.statusEmoji != nil {
		ids = *o.statusEmoji
	}
	for k, id := range ids {
		b.emoji[k] = customEmoji{id: id, fallback: defaultEmoji[k]}
	}

	api, err := tgbot.New(token, append([]tgbot.Option{
		tgbot.WithDefaultHandler(b.handleDefault),
		tgbot.WithErrorsHandler(func(err error) {
			if strings.Contains(err.Error(), "Conflict") {
				// Telegram allows one poller per token: a second instance (a local run
				// next to the server, say) makes both fight and drop updates.
				log.Error("telegram client error", "err", err, "hint", "another instance is polling with this bot token; stop it")
				return
			}
			log.Error("telegram client error", "err", err)
		}),
		tgbot.WithMiddlewares(b.logUpdates),
		tgbot.WithHTTPClient(apiPollTimeout, newAPIClient(o.apiProxy)),
		// Be explicit: if omitted, Telegram silently reuses whatever list was last
		// set for this token, which can hide inline updates.
		tgbot.WithAllowedUpdates(tgbot.AllowedUpdates{
			models.AllowedUpdateMessage,
			models.AllowedUpdateInlineQuery,
			models.AllowedUpdateChosenInlineResult,
			models.AllowedUpdateCallbackQuery,
		}),
	}, o.api...)...)
	if err != nil {
		return nil, fmt.Errorf("create telegram client: %w", err)
	}
	b.api = api

	api.RegisterHandlerMatchFunc(isCommand("start"), b.handleStart)
	api.RegisterHandlerMatchFunc(isCommand("help"), b.handleHelp)
	api.RegisterHandlerMatchFunc(isCommand("cancel"), b.handleCancel)
	api.RegisterHandlerMatchFunc(isCommand("emoji"), b.handleEmoji)
	api.RegisterHandler(tgbot.HandlerTypeCallbackQueryData, cancelData, tgbot.MatchTypeExact, b.handleCancelButton)
	api.RegisterHandlerMatchFunc(isInlineQuery, b.handleInline)
	api.RegisterHandlerMatchFunc(isChosenInlineResult, b.handleChosenInline)

	if b.storageChatID == 0 {
		log.Info("STORAGE_CHAT_ID not set: inline mode only works for sites with direct video URLs")
	}
	return b, nil
}

// logUpdates logs the kind of every incoming update at debug level (LOG_LEVEL=debug).
func (b *Bot) logUpdates(next tgbot.HandlerFunc) tgbot.HandlerFunc {
	return func(ctx context.Context, api *tgbot.Bot, u *models.Update) {
		kind := "other"
		switch {
		case u.Message != nil:
			kind = "message"
		case u.InlineQuery != nil:
			kind = "inline_query"
		case u.ChosenInlineResult != nil:
			kind = "chosen_inline_result"
		case u.CallbackQuery != nil:
			kind = "callback_query"
		}
		b.log.Debug("update received", "kind", kind, "update_id", u.ID)
		next(ctx, api, u)
	}
}

// isCommand matches a message that starts with /name, also in the group form
// /name@botname. The library's own command matcher wants the name without the
// slash and doesn't understand the @botname suffix.
func isCommand(name string) tgbot.MatchFunc {
	return func(u *models.Update) bool {
		m := u.Message
		if m == nil {
			return false
		}
		for _, e := range m.Entities {
			if e.Type != models.MessageEntityTypeBotCommand || e.Offset != 0 || e.Length < 2 || e.Length > len(m.Text) {
				continue
			}
			cmd, _, _ := strings.Cut(m.Text[1:e.Length], "@")
			return strings.EqualFold(cmd, name)
		}
		return false
	}
}

// loadUsername fetches the bot's @username so /help can show how to use inline mode.
func (b *Bot) loadUsername(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, editTimeout)
	defer cancel()

	me, err := b.api.GetMe(ctx)
	if err != nil {
		b.log.Warn("could not fetch bot username; /help will use a placeholder", "err", err)
		return
	}
	b.username = me.Username
}

// checkStorageChat warns at startup if the storage chat can't be reached, so a
// wrong STORAGE_CHAT_ID shows up right away instead of on a user's first pick.
func (b *Bot) checkStorageChat(ctx context.Context) {
	if b.storageChatID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, editTimeout)
	defer cancel()

	chat, err := b.api.GetChat(ctx, &tgbot.GetChatParams{ChatID: b.storageChatID})
	if err != nil {
		b.log.Warn("storage chat is not reachable: inline mode for YouTube will fail",
			"chat_id", b.storageChatID, "err", err,
			"hint", "channel IDs start with -100; add the bot as an admin that can post, or send /start to the bot if it's a private chat")
		return
	}
	b.log.Info("storage chat ok", "chat_id", b.storageChatID, "title", chat.Title)
}

// Run starts long polling and blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) {
	b.loadUsername(ctx)
	b.loadEmoji(ctx)
	b.checkStorageChat(ctx)
	b.log.Info("bot started")
	b.api.Start(ctx)
	b.log.Info("bot stopped")
}
