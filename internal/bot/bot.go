// Package bot wires the Telegram client to the application's handlers.
package bot

import (
	"context"
	"fmt"
	"log/slog"

	tgbot "github.com/go-telegram/bot"

	"anydloadbot/internal/downloader"
)

// Bot is the application's Telegram bot.
type Bot struct {
	api       *tgbot.Bot
	log       *slog.Logger
	providers *downloader.Registry
}

// New creates a Bot that downloads links using the given providers and registers all handlers.
func New(token string, providers *downloader.Registry, log *slog.Logger) (*Bot, error) {
	b := &Bot{log: log, providers: providers}

	api, err := tgbot.New(token,
		tgbot.WithDefaultHandler(b.handleDefault),
		tgbot.WithErrorsHandler(func(err error) { log.Error("telegram client error", "err", err) }),
	)
	if err != nil {
		return nil, fmt.Errorf("create telegram client: %w", err)
	}
	b.api = api

	api.RegisterHandler(tgbot.HandlerTypeMessageText, "/start", tgbot.MatchTypeCommand, b.handleStart)
	api.RegisterHandler(tgbot.HandlerTypeMessageText, "/help", tgbot.MatchTypeCommand, b.handleHelp)

	return b, nil
}

// Run starts long polling and blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) {
	b.log.Info("bot started")
	b.api.Start(ctx)
	b.log.Info("bot stopped")
}
