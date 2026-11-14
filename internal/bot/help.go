package bot

import (
	"fmt"
	"html"
	"strings"

	"anydloadbot/internal/downloader"
)

// startText is the short greeting; /help has the details.
func (b *Bot) startText() string {
	text := "Hi! 👋 Send me a link and I'll send the video or image straight back.\nSupported sites: " + b.supported() + "."
	if audio := b.audioSites(); audio != "" {
		text += "\n\nAdd \"mp3\" after a " + audio + " link to get just the audio."
	}
	return text + "\n\nUse /help to see everything I can do."
}

// helpText builds the /help message (HTML) from what the registered providers
// actually support, so it stays accurate as sites are added.
func (b *Bot) helpText() string {
	var sb strings.Builder
	handle := html.EscapeString(b.handle())

	sb.WriteString("<b>📥 Download videos, images and audio</b>\nSend me a link and I'll send the media straight back.\n")

	sb.WriteString("\n<b>Supported sites</b>\n")
	for _, p := range b.providers.All() {
		fmt.Fprintf(&sb, "• <b>%s</b>", html.EscapeString(p.Name()))
		if d, ok := p.(downloader.Describer); ok {
			sb.WriteString(" — " + html.EscapeString(d.Description()))
		}
		if b.inlineSupported(p) {
			sb.WriteString(" · ⚡ inline")
		}
		sb.WriteString("\n")
	}

	if audio := b.audioSites(); audio != "" {
		fmt.Fprintf(&sb, "\n<b>🎵 MP3</b>\nAdd <code>mp3</code> after a link to get just the audio (%s):\n<code>https://youtu.be/… mp3</code>\nThe file has the title, artist and cover art embedded.\n", html.EscapeString(audio))
	}

	fmt.Fprintf(&sb, "\n<b>⚡ Inline mode</b>\nUse me in any chat without opening this one: type <code>%s</code> and a link, then pick a result.\n<code>%s https://…</code>\n", handle, handle)
	if b.hasPlaceholderSites() {
		sb.WriteString("Some sites need a moment: the message shows ⏳ and turns into the file when it's ready. ")
	}
	if audio := b.audioSites(); audio != "" {
		sb.WriteString("For " + html.EscapeString(audio) + " you get two results: the video and 🎵 MP3.")
	}
	sb.WriteString("\n")

	fmt.Fprintf(&sb, "\n<b>Limits</b>\n• Files up to %d MB (Telegram's limit for bots)\n• Up to %d downloads at once per person\n• Public content only\n", downloader.MaxSize>>20, b.jobs.maxUser)

	sb.WriteString("\n/start — greeting\n/cancel — stop your running downloads\n/help — this message")
	return sb.String()
}

// handle is the bot's @username, or a placeholder if Telegram didn't tell us.
func (b *Bot) handle() string {
	if b.username == "" {
		return "@botname"
	}
	return "@" + b.username
}

// inlineSupported reports whether p works in inline mode: either it has a
// direct URL, or it can show a placeholder and a storage chat is configured.
func (b *Bot) inlineSupported(p downloader.Provider) bool {
	if _, ok := p.(downloader.Resolver); ok {
		return true
	}
	_, ok := p.(downloader.Previewer)
	return ok && b.storageChatID != 0
}

// hasPlaceholderSites reports whether any inline result needs the ⏳ placeholder flow.
func (b *Bot) hasPlaceholderSites() bool {
	for _, p := range b.providers.All() {
		if _, direct := p.(downloader.Resolver); direct {
			continue
		}
		if b.inlineSupported(p) {
			return true
		}
	}
	return false
}
