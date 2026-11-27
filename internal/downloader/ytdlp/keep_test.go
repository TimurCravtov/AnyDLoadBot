package ytdlp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"anydloadbot/internal/downloader"
)

func TestFetchKeepsFilesOnDisk(t *testing.T) {
	t.Parallel()

	media := t.TempDir()
	stale := filepath.Join(media, "ytdl-old")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}

	r := &recorder{}
	c := NewWithRunner(r.run, 1)
	if err := c.KeepFiles(media, 300<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("leftover of an earlier run not removed: %v", err)
	}
	if c.MaxSize() != 300<<20 {
		t.Errorf("MaxSize() = %d", c.MaxSize())
	}

	f, err := c.Fetch(context.Background(), "https://example.com/v",
		Request{Format: "bv*[filesize<" + VideoSize + "]+ba/b[filesize<" + FileSize + "]", Sort: "res"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Data != nil || filepath.Dir(filepath.Dir(f.Path)) != media {
		t.Fatalf("file not kept in the media dir: %+v", f)
	}
	data, err := os.ReadFile(f.Path)
	if err != nil || string(data) != "video" {
		t.Fatalf("kept file = %q, %v", data, err)
	}
	if runtime.GOOS != "windows" { // the local Bot API server, another user, reads it
		info, _ := os.Stat(f.Path)
		dir, _ := os.Stat(filepath.Dir(f.Path))
		if info.Mode().Perm() != 0o644 || dir.Mode().Perm() != 0o755 {
			t.Errorf("permissions: file %v, dir %v", info.Mode(), dir.Mode())
		}
	}

	// The size limit is filled into the format and --max-filesize.
	args := r.calls[0]
	if got := args[slices.Index(args, "-f")+1]; got != "bv*[filesize<240M]+ba/b[filesize<288M]" {
		t.Errorf("format = %q", got)
	}
	if got := args[slices.Index(args, "--max-filesize")+1]; got != "1200M" { // room to compress
		t.Errorf("--max-filesize = %q", got)
	}

	m := f.Media(downloader.KindVideo)
	if m.Path != f.Path || m.Size() != 5 {
		t.Errorf("media = %+v, size %d", m, m.Size())
	}
	m.Close()
	m.Close()
	if _, err := os.Stat(filepath.Dir(f.Path)); !os.IsNotExist(err) {
		t.Errorf("Close left the file: %v", err)
	}
}

func TestFetchInMemoryLimits(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	f, err := NewWithRunner(r.run, 1).Fetch(context.Background(), "https://example.com/v",
		Request{Format: "b[filesize<" + FileSize + "]/bv*[filesize<" + VideoSize + "]"})
	if err != nil || string(f.Data) != "video" || f.Path != "" {
		t.Fatalf("Fetch() = %+v, %v", f, err)
	}
	f.Media(downloader.KindVideo).Close() // nothing on disk: a no-op

	args := r.calls[0]
	if got := args[slices.Index(args, "-f")+1]; got != "b[filesize<48M]/bv*[filesize<40M]" {
		t.Errorf("format = %q", got)
	}
	if got := args[slices.Index(args, "--max-filesize")+1]; got != "50M" {
		t.Errorf("--max-filesize = %q", got)
	}
}

func TestCompressPlan(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		duration float64
		height   int
		ok       bool
	}{
		{600, 720, true},     // 10 min in 300 MB: plenty
		{3600, 360, true},    // an hour: ~500 kbps
		{4 * 3600, 0, false}, // too long to be watchable at that size
	} {
		plan, ok := compressPlan(tt.duration, 300<<20)
		if ok != tt.ok || plan.height != tt.height {
			t.Errorf("compressPlan(%v) = %+v, %v; want height %d, %v", tt.duration, plan, ok, tt.height, tt.ok)
		}
	}
}

func TestCompressUnknownDurationIsTooLarge(t *testing.T) {
	t.Parallel()

	// No ffprobe answer (missing tool or not a video): refused, not crashed.
	in := filepath.Join(t.TempDir(), "x.mp4")
	if err := os.WriteFile(in, []byte("not a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := compress(context.Background(), in, 0, 1<<20); !errors.Is(err, downloader.ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}
