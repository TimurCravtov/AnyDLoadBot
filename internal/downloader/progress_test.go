package downloader

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestReportWithoutCallbackIsANoOp(t *testing.T) {
	t.Parallel()
	Report(context.Background(), Progress{Stage: StageDownloading, Done: 1}) // must not panic
}

func TestGetMediaReportsProgress(t *testing.T) {
	t.Parallel()

	body := bytes.Repeat([]byte("x"), 100_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http only adds Content-Length by itself for small bodies; a
		// larger one is sent chunked, i.e. with unknown size (/chunked).
		if r.URL.Path != "/chunked" {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	var mu sync.Mutex
	var reports []Progress
	ctx := WithProgress(context.Background(), func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, p)
	})
	f := NewFetcher(func(string) bool { return true })

	data, err := f.GetMedia(ctx, srv.URL, MaxSize, "video/mp4")
	if err != nil || len(data) != len(body) {
		t.Fatalf("GetMedia = %d bytes, %v", len(data), err)
	}
	if len(reports) == 0 {
		t.Fatal("no progress reported")
	}
	last := reports[len(reports)-1]
	if last.Stage != StageDownloading || last.Done != int64(len(body)) || last.Total != int64(len(body)) {
		t.Errorf("last report = %+v, want all %d bytes done of %d", last, len(body), len(body))
	}
	for i := 1; i < len(reports); i++ {
		if reports[i].Done < reports[i-1].Done {
			t.Errorf("progress went backwards: %+v then %+v", reports[i-1], reports[i])
		}
	}

	// Without a size the download still reports bytes, just no total.
	reports = nil
	if _, err := f.GetMedia(ctx, srv.URL+"/chunked", MaxSize, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	if last := reports[len(reports)-1]; last.Done != int64(len(body)) || last.Total != 0 {
		t.Errorf("chunked: last report = %+v, want all bytes done and total unknown (0)", last)
	}

	// Plain Get (pages, API responses) is not a download and reports nothing.
	reports = nil
	if _, err := f.Get(ctx, srv.URL, MaxSize, "text/html"); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 0 {
		t.Errorf("Get reported %d progress updates, want none", len(reports))
	}
}

func TestStageTimer(t *testing.T) {
	t.Parallel()

	var forwarded int
	ctx := WithProgress(context.Background(), func(Progress) { forwarded++ })
	ctx, timer := WithStageTimer(ctx)

	start := timer.since
	timer.observe(StageDownloading, start.Add(4*time.Second)) // 4s of lookup
	timer.observe(StageDownloading, start.Add(5*time.Second)) // same stage: nothing changes
	timer.observe(StageUploading, start.Add(7*time.Second))   // 3s of download
	Report(ctx, Progress{Stage: StageUploading})

	got := fmt.Sprint(timer.attrsAt(start.Add(8 * time.Second)))
	if got != "[lookup 4s download 3s upload 1s]" {
		t.Errorf("attrs = %s", got)
	}
	if forwarded != 1 {
		t.Errorf("progress forwarded %d times, want 1", forwarded)
	}
}
