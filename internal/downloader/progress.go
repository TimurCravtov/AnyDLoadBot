package downloader

import (
	"context"
	"io"
	"sync"
	"time"
)

// Stage is the phase a download is in.
type Stage int

// Download stages, in the order they normally happen.
const (
	StagePreparing   Stage = iota // looking up the media
	StageDownloading              // fetching bytes; see Progress.Done and Total
	StageProcessing               // merging or converting (ffmpeg), no byte counts
	StageUploading                // sending the file to Telegram
)

// Progress is a snapshot of a download.
type Progress struct {
	Stage Stage
	Done  int64 // bytes downloaded so far (StageDownloading)
	Total int64 // expected bytes, 0 if unknown (StageDownloading)
}

// ProgressFunc receives progress snapshots. It is called from the goroutine
// doing the work, so it must be quick and safe for concurrent use.
type ProgressFunc func(Progress)

type progressKey struct{}

// WithProgress returns a context whose downloads report progress to f.
// Providers don't take a callback; they report through the context they are
// given, so progress works without changing any interface.
func WithProgress(ctx context.Context, f ProgressFunc) context.Context {
	return context.WithValue(ctx, progressKey{}, f)
}

// Report sends p to the context's ProgressFunc, if it has one.
func Report(ctx context.Context, p Progress) {
	if f, ok := ctx.Value(progressKey{}).(ProgressFunc); ok && f != nil {
		f(p)
	}
}

// progressReader counts the bytes read through it and reports them.
type progressReader struct {
	ctx   context.Context
	r     io.Reader
	done  int64
	total int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.done += int64(n)
		Report(p.ctx, Progress{Stage: StageDownloading, Done: p.done, Total: p.total})
	}
	return n, err
}

// StageTimer measures how long a download spends in each stage, for logs.
type StageTimer struct {
	mu    sync.Mutex
	cur   Stage
	since time.Time
	spent [StageUploading + 1]time.Duration
}

// WithStageTimer returns a context whose progress reports also go to a new
// StageTimer (and still to any ProgressFunc ctx already has). The timer starts
// in StagePreparing.
func WithStageTimer(ctx context.Context) (context.Context, *StageTimer) {
	t := &StageTimer{since: time.Now()}
	prev, _ := ctx.Value(progressKey{}).(ProgressFunc)
	return WithProgress(ctx, func(p Progress) {
		t.observe(p.Stage, time.Now())
		if prev != nil {
			prev(p)
		}
	}), t
}

func (t *StageTimer) observe(s Stage, now time.Time) {
	if s < StagePreparing || s > StageUploading {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s != t.cur {
		t.spent[t.cur] += now.Sub(t.since)
		t.cur, t.since = s, now
	}
}

// LogAttrs returns the time spent in each stage so far as slog key-value
// pairs: lookup, download, process and upload (stages that never ran are left out).
func (t *StageTimer) LogAttrs() []any { return t.attrsAt(time.Now()) }

func (t *StageTimer) attrsAt(now time.Time) []any {
	t.mu.Lock()
	spent := t.spent
	spent[t.cur] += now.Sub(t.since)
	t.mu.Unlock()

	names := [...]string{"lookup", "download", "process", "upload"}
	var attrs []any
	for s, d := range spent {
		if d > 0 {
			attrs = append(attrs, names[s], d.Round(10*time.Millisecond).String())
		}
	}
	return attrs
}
