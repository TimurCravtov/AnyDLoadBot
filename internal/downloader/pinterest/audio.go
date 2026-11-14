package pinterest

import (
	"context"
	"strings"

	"anydloadbot/internal/downloader"
	"anydloadbot/internal/downloader/ytdlp"
)

// WithAudio adds MP3 extraction to a Pinterest provider. Pinterest itself only
// serves MP4s, so the pin's video goes through yt-dlp and ffmpeg to get the
// audio track. Without yt-dlp the plain provider is used and MP3 isn't offered.
func WithAudio(p *Provider, yt *ytdlp.Client) downloader.Provider {
	return &audioProvider{Provider: p, yt: yt}
}

type audioProvider struct {
	*Provider
	yt *ytdlp.Client
}

var (
	_ downloader.Resolver        = (*audioProvider)(nil)
	_ downloader.AudioDownloader = (*audioProvider)(nil)
)

// Description implements downloader.Describer.
func (*audioProvider) Description() string {
	return `video pins (pinterest.com and pin.it links); add "mp3" after the link for the sound`
}

// DownloadAudio implements downloader.AudioDownloader.
func (a *audioProvider) DownloadAudio(ctx context.Context, link string) (*downloader.Media, error) {
	src, err := a.Resolve(ctx, link) // a direct MP4 on Pinterest's CDN (checked by Resolve)
	if err != nil {
		return nil, err
	}

	release, err := a.yt.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	file, err := a.yt.Fetch(ctx, src.URL, ytdlp.Request{Audio: true})
	if err != nil {
		return nil, err
	}
	return &downloader.Media{
		Kind:     downloader.KindAudio,
		Filename: strings.TrimSuffix(file.Name, ".mp3") + ".mp3",
		Data:     file.Data,
		Title:    "Pinterest audio",
	}, nil
}
