// Package bot wires the Telegram client to the application's handlers.
package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	maxFileSize   int64         // the size limit users are told about
	username      string        // set once in Run, before polling starts
	progressEvery time.Duration // how often progress is shown; tests shorten it
	apiRetry      time.Duration // first waitForAPI retry delay; tests shorten it

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
	maxFileSize       int64
	apiProxy          *url.URL
	statusEmoji       *[numStatusKinds]string // nil: statusEmojiIDs
	api               []tgbot.Option
}

// WithLimits caps how many downloads may run at once: per user, and across the
// whole bot. Values <= 0 keep the defaults.
func WithLimits(perUser, total int) Option {
	return func(o *options) { o.maxUser, o.maxTotal = perUser, total }
}

// WithMaxFileSize sets the size limit shown in /help and in "too large"
// replies (default downloader.MaxSize). It doesn't enforce anything: the
// providers do.
func WithMaxFileSize(n int64) Option {
	return func(o *options) { o.maxFileSize = n }
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

// WithAPIServer makes the bot talk to a local Telegram Bot API server (for
// example http://telegram-bot-api:8081) instead of api.telegram.org. Uploads
// then only travel to that server, which sends them on to Telegram itself.
// The bot must have been logged out of api.telegram.org once (LogOutCloud).
func WithAPIServer(serverURL string) Option {
	return func(o *options) { o.api = append(o.api, tgbot.WithServerURL(serverURL)) }
}

// LogOutCloud logs the bot out of Telegram's public Bot API server, which a
// bot must do once before it uses a local Bot API server. Afterwards it can't
// log back in to the public server for 10 minutes. proxy may be nil.
func LogOutCloud(ctx context.Context, token string, proxy *url.URL) error {
	return logOut(ctx, cloudAPI, token, newAPIClient(proxy))
}

const cloudAPI = "https://api.telegram.org"

func logOut(ctx context.Context, server, token string, client *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/bot"+token+"/logOut", nil)
	if err != nil {
		return errors.New("invalid bot token") // never echo the URL: it holds the token
	}
	resp, err := client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err // drop the URL, which holds the token
		}
		return fmt.Errorf("log out: %w", err)
	}
	defer resp.Body.Close()

	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r); err != nil {
		return fmt.Errorf("log out: %s", resp.Status)
	}
	if !r.OK {
		return fmt.Errorf("log out: %s", r.Description)
	}
	return nil
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
	if o.maxFileSize <= 0 {
		o.maxFileSize = downloader.MaxSize
	}

	b := &Bot{
		log:           log,
		providers:     providers,
		storageChatID: o.storageChatID,
		maxFileSize:   o.maxFileSize,
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
		// The token is checked in Run, with retries (waitForAPI): a local Bot API
		// server may still be starting, and Telegram may be briefly unreachable.
		tgbot.WithSkipGetMe(),
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

// Retry delays for waitForAPI.
const (
	apiRetryFirst = time.Second
	apiRetryMax   = 30 * time.Second
	getMeTimeout  = 30 * time.Second
)

// waitForAPI checks the token with getMe and remembers the bot's @username
// (for /help). While Telegram, or a local Bot API server that is still
// starting, can't be reached, it logs why and tries again, so the bot stays up
// and says what is wrong. Only a rejected token is fatal.
func (b *Bot) waitForAPI(ctx context.Context) error {
	delay := b.apiRetry
	if delay <= 0 {
		delay = apiRetryFirst
	}
	for attempt := 1; ; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, getMeTimeout)
		me, err := b.api.GetMe(cctx)
		cancel()
		switch {
		case err == nil:
			b.username = me.Username
			if attempt > 1 {
				b.log.Info("Telegram API reachable", "attempts", attempt)
			}
			return nil
		case errors.Is(err, tgbot.ErrorUnauthorized):
			return fmt.Errorf("telegram rejected BOT_TOKEN: %w", err)
		case ctx.Err() != nil:
			return ctx.Err()
		}

		b.log.Warn("Telegram API not reachable yet, retrying", "attempt", attempt, "err", err, "retry_in", delay.String())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		delay = min(delay*2, apiRetryMax)
	}
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

// Run starts long polling and blocks until ctx is cancelled. It returns early
// only if Telegram rejects the token.
func (b *Bot) Run(ctx context.Context) error {
	b.log.Info("connecting to Telegram")
	if err := b.waitForAPI(ctx); err != nil {
		if ctx.Err() != nil {
			return nil // stopped while waiting
		}
		return err
	}
	b.loadEmoji(ctx)
	b.checkStorageChat(ctx)
	b.log.Info("bot started")
	b.api.Start(ctx)
	b.log.Info("bot stopped")
	return nil
}
