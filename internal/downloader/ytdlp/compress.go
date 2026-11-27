package ytdlp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"anydloadbot/internal/downloader"
)

const (
	// compressAudioKbps is the AAC bitrate of a compressed video.
	compressAudioKbps = 128
	// compressMinVideoKbps is the lowest video bitrate worth sending; a video
	// that would need less is refused as too large instead.
	compressMinVideoKbps = 150
	// compressMargin keeps the encoder's bitrate overshoot and the container
	// overhead within the limit.
	compressMargin = 0.9
)

// compress re-encodes the video at in so that it fits in limit bytes, and
// returns the new file's path (next to in, which is removed). duration is the
// video's length in seconds, or 0 if unknown (it is then read from the file).
// It returns downloader.ErrTooLarge if the video is too long to fit at a
// watchable bitrate.
func compress(ctx context.Context, in string, duration float64, limit int64) (string, error) {
	if duration <= 0 {
		d, err := probeDuration(ctx, in)
		if err != nil {
			return "", fmt.Errorf("%w: unknown duration, can't compress: %v", downloader.ErrTooLarge, err)
		}
		duration = d
	}
	plan, ok := compressPlan(duration, limit)
	if !ok {
		return "", fmt.Errorf("%w: too long to compress to %d MB", downloader.ErrTooLarge, limit>>20)
	}

	out := strings.TrimSuffix(in, filepath.Ext(in)) + ".small.mp4"
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", in,
		"-map", "0:v:0", "-map", "0:a:0?",
		"-c:v", "libx264", "-preset", "veryfast",
		"-b:v", plan.videoKbps + "k", "-maxrate", plan.maxKbps + "k", "-bufsize", plan.bufKbps + "k",
		"-vf", "scale=-2:'min(" + strconv.Itoa(plan.height) + ",ih)'", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", strconv.Itoa(compressAudioKbps) + "k",
		"-movflags", "+faststart", out,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(out)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("compress video: %w: %s", err, truncateErr(stderr.String()))
	}
	_ = os.Remove(in)

	info, err := os.Stat(out)
	if err != nil {
		return "", fmt.Errorf("stat compressed video: %w", err)
	}
	if info.Size() > limit {
		return "", fmt.Errorf("%w: still %d MB after compressing", downloader.ErrTooLarge, info.Size()>>20)
	}
	return out, nil
}

type encodePlan struct {
	videoKbps, maxKbps, bufKbps string
	height                      int
}

// compressPlan picks the video bitrate that fills limit bytes over duration
// seconds, and a resolution that bitrate can carry.
func compressPlan(duration float64, limit int64) (encodePlan, bool) {
	totalKbps := float64(limit) * 8 * compressMargin / duration / 1000
	video := int(totalKbps) - compressAudioKbps
	if video < compressMinVideoKbps {
		return encodePlan{}, false
	}
	height := 240
	switch {
	case video >= 1500:
		height = 720
	case video >= 800:
		height = 480
	case video >= 400:
		height = 360
	}
	return encodePlan{
		videoKbps: strconv.Itoa(video),
		maxKbps:   strconv.Itoa(video * 13 / 10),
		bufKbps:   strconv.Itoa(video * 2),
		height:    height,
	}, true
}

// probeDuration reads a media file's length in seconds with ffprobe.
func probeDuration(ctx context.Context, file string) (float64, error) {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-show_entries", "format=duration", "-of", "csv=p=0", file).Output()
	if err != nil {
		return 0, err
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("no duration in %q", strings.TrimSpace(string(out)))
	}
	return d, nil
}

func truncateErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxStderr {
		s = s[len(s)-maxStderr:]
	}
	return s
}
