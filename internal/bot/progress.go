package bot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"anydloadbot/internal/downloader"
)

// progressInterval is how often the status message is updated. Telegram
// rate-limits message edits, so progress moves in steps, not smoothly.
const progressInterval = 3 * time.Second

const progressBarWidth = 16

// progressTracker keeps the latest progress snapshot and a smoothed speed.
type progressTracker struct {
	mu         sync.Mutex
	latest     downloader.Progress
	have       bool
	stageSince time.Time // when the current stage started
	lastStage  downloader.Stage
	lastDone   int64
	lastAt     time.Time
	speed      float64 // bytes per second, smoothed
}

func (t *progressTracker) update(p downloader.Progress) { t.updateAt(p, time.Now()) }

func (t *progressTracker) updateAt(p downloader.Progress, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.have || p.Stage != t.latest.Stage {
		t.stageSince = now
	}
	t.latest, t.have = p, true
}

// snapshot returns the latest progress, the current speed estimate and how
// long the current stage has been running.
func (t *progressTracker) snapshot(now time.Time) (downloader.Progress, float64, time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.have {
		return downloader.Progress{}, 0, 0, false
	}

	p := t.latest
	if p.Stage == downloader.StageDownloading || p.Stage == downloader.StageUploading {
		switch {
		case t.lastAt.IsZero() || p.Done < t.lastDone || p.Stage != t.lastStage:
			// The first sample, a new stream (yt-dlp fetches the video and
			// then the audio), or downloading turned into uploading: no speed yet.
			t.speed = 0
		default:
			if dt := now.Sub(t.lastAt).Seconds(); dt > 0 {
				current := float64(p.Done-t.lastDone) / dt
				if t.speed == 0 {
					t.speed = current
				} else {
					t.speed = 0.5*t.speed + 0.5*current
				}
			}
		}
		t.lastStage, t.lastDone, t.lastAt = p.Stage, p.Done, now
	}
	return p, t.speed, now.Sub(t.stageSince), true
}

// renderProgress is the status message text for a progress snapshot. Stages
// without byte counts show how long they have been running, so the message
// visibly moves even when a site's slow part is looking the media up.
func renderProgress(p downloader.Progress, speed float64, elapsed time.Duration) string {
	label := "⏳ Downloading…"
	switch p.Stage {
	case downloader.StagePreparing:
		return "⏳ Preparing…" + elapsedSuffix(elapsed)
	case downloader.StageProcessing:
		return "🔧 Processing…" + elapsedSuffix(elapsed)
	case downloader.StageUploading:
		if p.Total <= 0 {
			return "📤 Uploading to Telegram…" + elapsedSuffix(elapsed)
		}
		label = "📤 Uploading to Telegram…"
	}

	if p.Total <= 0 { // download of unknown size: no percentage
		text := "⏳ Downloading… " + formatBytes(p.Done)
		if speed > 0 {
			text += " · " + formatBytes(int64(speed)) + "/s"
		}
		return text
	}

	done := min(p.Done, p.Total)
	pct := int(done * 100 / p.Total)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %d%%\n%s %s / %s", label, pct, progressBar(pct), formatBytes(done), formatBytes(p.Total))
	if speed > 0 {
		fmt.Fprintf(&sb, " · %s/s", formatBytes(int64(speed)))
		if left := float64(p.Total-done) / speed; left >= 1 {
			fmt.Fprintf(&sb, " · ~%s left", formatDuration(time.Duration(left*float64(time.Second))))
		}
	}
	return sb.String()
}

func elapsedSuffix(d time.Duration) string {
	if d < time.Second {
		return ""
	}
	return " " + formatDuration(d)
}

func progressBar(pct int) string {
	filled := min(max(pct, 0), 100) * progressBarWidth / 100
	return strings.Repeat("▓", filled) + strings.Repeat("░", progressBarWidth-filled)
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
}

// startProgress shows live progress by editing a status message through edit.
// It returns the context the download must use (progress is reported through
// it) and a stop function. stop must be called, and must have returned, before
// the status message is replaced or removed; calling it again is harmless.
func (b *Bot) startProgress(ctx context.Context, edit func(ctx context.Context, kind statusKind, text string)) (context.Context, func()) {
	every := b.progressEvery
	if every <= 0 {
		every = progressInterval
	}

	t := &progressTracker{}
	t.update(downloader.Progress{Stage: downloader.StagePreparing}) // until the provider reports
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(every)
		defer tick.Stop()
		last := ""
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case now := <-tick.C:
				p, speed, elapsed, ok := t.snapshot(now)
				if !ok {
					continue
				}
				// Only edit when the text changes: Telegram rejects no-op edits.
				if text := renderProgress(p, speed, elapsed); text != last {
					edit(ctx, kindOf(p), text)
					last = text
				}
			}
		}
	}()

	var once sync.Once
	stop := func() { once.Do(func() { close(done); <-stopped }) }
	return downloader.WithProgress(ctx, t.update), stop
}

// editChatStatus shows text in a chat status message, keeping its Cancel
// button, with kind's custom emoji if one is set.
func (b *Bot) editChatStatus(ctx context.Context, api *tgbot.Bot, chatID int64, messageID int, kind statusKind, text string) {
	text, entities := b.statusMessage(kind, text)
	if err := b.withEmoji(entities, func(entities []models.MessageEntity) error {
		_, err := api.EditMessageText(ctx, &tgbot.EditMessageTextParams{
			ChatID: chatID, MessageID: messageID, Text: text, Entities: entities, ReplyMarkup: cancelKeyboard(),
		})
		return err
	}); err != nil {
		b.log.Debug("progress update", "err", err)
	}
}

// editInlineProgress shows text in an inline placeholder, keeping its buttons.
func (b *Bot) editInlineProgress(ctx context.Context, api *tgbot.Bot, inlineMessageID, link, text string) {
	if _, err := api.EditMessageText(ctx, &tgbot.EditMessageTextParams{
		InlineMessageID: inlineMessageID, Text: text, ReplyMarkup: placeholderKeyboard(link),
	}); err != nil {
		b.log.Debug("inline progress update", "err", err)
	}
}
