package ytdlp

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"anydloadbot/internal/downloader"
)

func TestScanOutput(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var got []downloader.Progress
	ctx := downloader.WithProgress(context.Background(), func(p downloader.Progress) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, p)
	})

	// Mixed \n and bare \r line endings, as yt-dlp may print them.
	out := "[youtube] abc: Downloading webpage\n" +
		processPrefix + "started\n" + // the metadata print after the lookup: not processing yet
		progressPrefix + "1024:4096:NA\r" +
		progressPrefix + "2048:NA:8192.5\n" + // total unknown: the estimate is used
		progressPrefix + "NA:NA:NA\n" + // nothing known yet
		processPrefix + "started\n" +
		"/tmp/ytdl-1/abc.mp4\n"

	var keep bytes.Buffer
	scanOutput(ctx, strings.NewReader(out), &keep, new(atomic.Bool))

	want := []downloader.Progress{
		{Stage: downloader.StageDownloading, Done: 1024, Total: 4096},
		{Stage: downloader.StageDownloading, Done: 2048, Total: 8192},
		{Stage: downloader.StageDownloading},
		{Stage: downloader.StageProcessing},
	}
	if !slices.Equal(got, want) {
		t.Errorf("progress = %+v\nwant %+v", got, want)
	}

	// Everything else is kept, so the printed file path is still the last line.
	if k := keep.String(); strings.Contains(k, progressPrefix) || strings.Contains(k, processPrefix) || lastLine(keep.Bytes()) != "/tmp/ytdl-1/abc.mp4" {
		t.Errorf("kept output = %q", k)
	}
}

func TestScanOutputLongLine(t *testing.T) {
	t.Parallel()

	// --dump-single-json prints the whole metadata as one long line.
	long := `{"id":"x","title":"` + strings.Repeat("a", 1<<20) + `"}`
	var keep bytes.Buffer
	scanOutput(context.Background(), strings.NewReader(long+"\n"), &keep, new(atomic.Bool))
	if strings.TrimSpace(keep.String()) != long {
		t.Errorf("long line was cut: kept %d bytes, want %d", len(strings.TrimSpace(keep.String())), len(long))
	}
}

func TestFetchAsksForProgress(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	c := NewWithRunner(r.run, 1)
	if _, err := c.Fetch(context.Background(), "https://example.com/v", Request{Format: "b", Sort: "res"}); err != nil {
		t.Fatal(err)
	}
	args := r.calls[0]
	if slices.Contains(args, "--no-progress") || !slices.Contains(args, "--progress") || !slices.Contains(args, "--newline") {
		t.Errorf("progress flags missing: %v", args)
	}
	var templates []string
	for i, a := range args {
		if a == "--progress-template" {
			templates = append(templates, args[i+1])
		}
	}
	if len(templates) != 2 || !strings.HasPrefix(templates[0], "download:"+progressPrefix) || !strings.HasPrefix(templates[1], "postprocess:"+processPrefix) {
		t.Errorf("progress templates = %q", templates)
	}
}
