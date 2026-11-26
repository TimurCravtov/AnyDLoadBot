//go:build !unix

package ytdlp

// startServer is Unix-only (it passes file descriptors over a Unix socket);
// elsewhere every run starts yt-dlp normally.
func startServer(_ []string, fallback Runner) Runner { return fallback }
