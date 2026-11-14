package bot

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"anydloadbot/internal/downloader"
)

func TestRenderProgress(t *testing.T) {
	t.Parallel()

	const mb = 1 << 20
	tests := []struct {
		name    string
		p       downloader.Progress
		speed   float64
		elapsed time.Duration
		want    string
	}{
		{"preparing", downloader.Progress{Stage: downloader.StagePreparing}, 0, 0, "⏳ Preparing…"},
		{"preparing for a while", downloader.Progress{Stage: downloader.StagePreparing}, 0, 6 * time.Second, "⏳ Preparing… 6s"},
		{"processing", downloader.Progress{Stage: downloader.StageProcessing}, 0, 3 * time.Second, "🔧 Processing… 3s"},
		{"uploading, size unknown", downloader.Progress{Stage: downloader.StageUploading}, 0, 70 * time.Second, "📤 Uploading to Telegram… 1m 10s"},
		{
			"uploading with progress",
			downloader.Progress{Stage: downloader.StageUploading, Done: 1 * mb, Total: 4 * mb}, 1 * mb, 0,
			"📤 Uploading to Telegram… 25%\n▓▓▓▓░░░░░░░░░░░░ 1.0 MB / 4.0 MB · 1.0 MB/s · ~3s left",
		},
		{
			"half done with speed",
			downloader.Progress{Stage: downloader.StageDownloading, Done: 3 * mb, Total: 6 * mb}, 1.5 * mb, 9 * time.Second,
			"⏳ Downloading… 50%\n▓▓▓▓▓▓▓▓░░░░░░░░ 3.0 MB / 6.0 MB · 1.5 MB/s · ~2s left",
		},
		{
			"no speed yet",
			downloader.Progress{Stage: downloader.StageDownloading, Done: 0, Total: 4 * mb}, 0, 0,
			"⏳ Downloading… 0%\n░░░░░░░░░░░░░░░░ 0 B / 4.0 MB",
		},
		{
			"long wait",
			downloader.Progress{Stage: downloader.StageDownloading, Done: mb, Total: 91 * mb}, mb, 0,
			"⏳ Downloading… 1%\n░░░░░░░░░░░░░░░░ 1.0 MB / 91.0 MB · 1.0 MB/s · ~1m 30s left",
		},
		{
			"done beyond the estimate is capped",
			downloader.Progress{Stage: downloader.StageDownloading, Done: 5 * mb, Total: 4 * mb}, 0, 0,
			"⏳ Downloading… 100%\n▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓ 4.0 MB / 4.0 MB",
		},
		{
			"size unknown",
			downloader.Progress{Stage: downloader.StageDownloading, Done: 512 << 10}, 256 << 10, 0,
			"⏳ Downloading… 512 KB · 256 KB/s",
		},
	}
	for _, tt := range tests {
		if got := renderProgress(tt.p, tt.speed, tt.elapsed); got != tt.want {
			t.Errorf("%s:\n got %q\nwant %q", tt.name, got, tt.want)
		}
	}
}

func TestProgressTrackerSpeed(t *testing.T) {
	t.Parallel()

	var tr progressTracker
	if _, _, _, ok := tr.snapshot(time.Now()); ok {
		t.Fatal("snapshot before any update")
	}

	start := time.Now()
	tr.updateAt(downloader.Progress{Stage: downloader.StageDownloading, Done: 0, Total: 10 << 20}, start)
	if _, speed, _, _ := tr.snapshot(start); speed != 0 {
		t.Errorf("speed after one sample = %v, want 0", speed)
	}
	tr.updateAt(downloader.Progress{Stage: downloader.StageDownloading, Done: 2 << 20, Total: 10 << 20}, start.Add(time.Second))
	if _, speed, _, _ := tr.snapshot(start.Add(2 * time.Second)); speed != 1<<20 {
		t.Errorf("speed = %v, want 1 MB/s", speed)
	}

	// A new stream (audio after video) starts from zero again: no bogus speed.
	tr.updateAt(downloader.Progress{Stage: downloader.StageDownloading, Done: 100, Total: 1 << 20}, start.Add(3*time.Second))
	if _, speed, _, _ := tr.snapshot(start.Add(3 * time.Second)); speed != 0 {
		t.Errorf("speed after a restart = %v, want 0", speed)
	}
}

func TestProgressTrackerSpeedResetsWhenUploadStarts(t *testing.T) {
	t.Parallel()

	var tr progressTracker
	start := time.Now()
	tr.updateAt(downloader.Progress{Stage: downloader.StageDownloading, Done: 1 << 20, Total: 2 << 20}, start)
	tr.snapshot(start)
	tr.updateAt(downloader.Progress{Stage: downloader.StageDownloading, Done: 2 << 20, Total: 2 << 20}, start.Add(time.Second))
	tr.snapshot(start.Add(time.Second))

	// The upload's byte count has nothing to do with the download's: a speed
	// computed across the two would be nonsense.
	tr.updateAt(downloader.Progress{Stage: downloader.StageUploading, Done: 3 << 20, Total: 4 << 20}, start.Add(2*time.Second))
	if _, speed, _, _ := tr.snapshot(start.Add(2 * time.Second)); speed != 0 {
		t.Errorf("speed right after the upload started = %v, want 0", speed)
	}
}

func TestProgressTrackerStageElapsed(t *testing.T) {
	t.Parallel()

	var tr progressTracker
	start := time.Now()
	tr.updateAt(downloader.Progress{Stage: downloader.StagePreparing}, start)
	if _, _, elapsed, _ := tr.snapshot(start.Add(6 * time.Second)); elapsed != 6*time.Second {
		t.Errorf("preparing elapsed = %v, want 6s", elapsed)
	}
	// The clock restarts with each new stage, not with each report.
	tr.updateAt(downloader.Progress{Stage: downloader.StageProcessing}, start.Add(7*time.Second))
	tr.updateAt(downloader.Progress{Stage: downloader.StageProcessing}, start.Add(8*time.Second))
	if _, _, elapsed, _ := tr.snapshot(start.Add(10 * time.Second)); elapsed != 3*time.Second {
		t.Errorf("processing elapsed = %v, want 3s", elapsed)
	}
}

// progressProvider reports progress like a real download, then finishes.
type progressProvider struct{}

func (progressProvider) Name() string { return "Progress" }
func (progressProvider) FindURL(text string) (string, bool) {
	if strings.Contains(text, "prog://") {
		return "prog://x", true
	}
	return "", false
}

func (progressProvider) Download(ctx context.Context, _ string) (*downloader.Media, error) {
	downloader.Report(ctx, downloader.Progress{Stage: downloader.StageDownloading, Done: 512 << 10, Total: 1 << 20})
	time.Sleep(150 * time.Millisecond) // several progress ticks
	downloader.Report(ctx, downloader.Progress{Stage: downloader.StageProcessing})
	time.Sleep(150 * time.Millisecond)
	return &downloader.Media{Filename: "v.mp4", Data: []byte("video"), Title: "T"}, nil
}

func (progressProvider) Preview(context.Context, string) (*downloader.Preview, bool) {
	return &downloader.Preview{Title: "P"}, true
}

func TestChatShowsProgress(t *testing.T) {
	t.Parallel()

	b, tg := newServerBot(t, progressProvider{})
	b.progressEvery = 20 * time.Millisecond
	b.handleDefault(context.Background(), b.api, chatMessage(1, 5, "prog://x"))

	edits := tg.texts("editMessageText")
	if !anyContains(edits, "50%") || !anyContains(edits, "Processing") {
		t.Errorf("progress edits = %q, want a 50%% download and then processing", edits)
	}
	// Every progress edit keeps the Cancel button.
	tg.mu.Lock()
	for _, c := range tg.calls {
		if c.method == "editMessageText" && !strings.Contains(c.fields["reply_markup"], cancelData) {
			t.Errorf("progress edit without the Cancel button: %v", c.fields)
		}
	}
	tg.mu.Unlock()

	// No progress edit may land after the status message is removed.
	methods := tg.methods()
	del := slices.Index(methods, "deleteMessage")
	if del < 0 {
		t.Fatalf("status message never removed: %v", methods)
	}
	for _, m := range methods[del:] {
		if m == "editMessageText" {
			t.Errorf("progress edit after the status message was deleted: %v", methods)
		}
	}
}

func TestInlineShowsProgress(t *testing.T) {
	t.Parallel()

	b, tg := newServerBot(t, progressProvider{})
	b.progressEvery = 20 * time.Millisecond
	b.handleChosenInline(context.Background(), b.api, chosen("prog://x"))

	if edits := tg.texts("editMessageText"); !anyContains(edits, "50%") {
		t.Errorf("inline progress edits = %q, want a 50%% download", edits)
	}
	// The placeholder keeps its Source and Cancel buttons while showing progress.
	tg.mu.Lock()
	for _, c := range tg.calls {
		if c.method == "editMessageText" && (!strings.Contains(c.fields["reply_markup"], cancelData) || !strings.Contains(c.fields["reply_markup"], "Source")) {
			t.Errorf("inline progress edit lost its buttons: %v", c.fields)
		}
	}
	tg.mu.Unlock()

	// The media replaces the placeholder only after the last progress edit.
	methods := tg.methods()
	media := slices.Index(methods, "editMessageMedia")
	if media < 0 || slices.Contains(methods[media:], "editMessageText") {
		t.Errorf("progress edit after the media was sent: %v", methods)
	}
}

func TestStartProgressStopIsIdempotentAndQuiet(t *testing.T) {
	t.Parallel()

	b := newTestBot()
	b.progressEvery = 5 * time.Millisecond
	var mu sync.Mutex
	var texts []string
	ctx, stop := b.startProgress(context.Background(), func(_ context.Context, text string) {
		mu.Lock()
		defer mu.Unlock()
		texts = append(texts, text)
	})
	downloader.Report(ctx, downloader.Progress{Stage: downloader.StageProcessing})
	time.Sleep(40 * time.Millisecond)
	stop()
	stop() // harmless

	mu.Lock()
	before := len(texts)
	mu.Unlock()
	downloader.Report(ctx, downloader.Progress{Stage: downloader.StageUploading})
	time.Sleep(40 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if before == 0 || !strings.HasPrefix(texts[before-1], "🔧 Processing") {
		t.Errorf("edits before stop = %q, want them to end with the processing stage", texts)
	}
	for i := 1; i < before; i++ {
		if texts[i] == texts[i-1] {
			t.Errorf("the same text was sent twice in a row: %q", texts[i])
		}
	}
	if len(texts) != before {
		t.Errorf("edits after stop: %q, want none", texts[before:])
	}
}

func TestStartProgressShowsPreparingBeforeAnyReport(t *testing.T) {
	t.Parallel()

	b := newTestBot()
	b.progressEvery = 10 * time.Millisecond
	var mu sync.Mutex
	var texts []string
	_, stop := b.startProgress(context.Background(), func(_ context.Context, text string) {
		mu.Lock()
		defer mu.Unlock()
		texts = append(texts, text)
	})
	time.Sleep(1200 * time.Millisecond) // a provider still looking the media up, reporting nothing
	stop()

	mu.Lock()
	defer mu.Unlock()
	if !anyContains(texts, "⏳ Preparing… 1s") {
		t.Errorf("edits = %q, want a ticking \"Preparing… 1s\" while nothing is reported", texts)
	}
}
