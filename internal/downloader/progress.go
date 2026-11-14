package downloader

import (
	"context"
	"io"
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
