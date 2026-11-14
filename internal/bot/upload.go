package bot

import (
	"context"
	"io"
	"net/http"
	"time"

	"anydloadbot/internal/downloader"
)

// apiPollTimeout is the long-polling timeout for getUpdates (the library's default).
const apiPollTimeout = time.Minute

// uploadKey marks the context of an API request that uploads a file.
type uploadKey struct{}

// withUpload marks ctx so the request made with it reports upload progress.
// Only file uploads are marked: progress edits and other calls also carry the
// progress callback in their context, and must not show up as an upload.
func withUpload(ctx context.Context) context.Context {
	return context.WithValue(ctx, uploadKey{}, true)
}

// newAPIClient is the HTTP client for the Telegram Bot API. It differs from the
// library's default in two ways:
//   - requests marked withUpload report upload progress, counted as the request
//     body is actually sent (the library builds the whole body in memory first,
//     so counting reads from the file itself would jump straight to 100%);
//   - there is no overall request timeout. The library's default of one minute
//     would cut off a 50 MB upload on a slow link. Instead the response must
//     start within responseHeaderTimeout after the request was sent, which
//     still catches a dead connection, and downloads have their own deadline.
func newAPIClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = apiPollTimeout + 30*time.Second // getUpdates holds the response for up to apiPollTimeout
	return &http.Client{Transport: uploadProgressTransport{base: t}}
}

// uploadProgressTransport reports upload progress for requests marked withUpload.
type uploadProgressTransport struct{ base http.RoundTripper }

func (t uploadProgressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	marked, _ := req.Context().Value(uploadKey{}).(bool)
	if !marked || req.Body == nil || req.Body == http.NoBody || req.ContentLength <= 0 {
		return t.base.RoundTrip(req)
	}

	// A RoundTripper must not change the request it is given, so send a copy.
	out := req.Clone(req.Context())
	out.Body = &countingBody{ctx: req.Context(), rc: req.Body, total: req.ContentLength}
	if req.GetBody != nil { // used if the request has to be sent again
		out.GetBody = func() (io.ReadCloser, error) {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			return &countingBody{ctx: req.Context(), rc: body, total: req.ContentLength}, nil
		}
	}
	return t.base.RoundTrip(out)
}

// countingBody reports how much of a request body has been sent.
type countingBody struct {
	ctx         context.Context
	rc          io.ReadCloser
	done, total int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.done += int64(n)
		downloader.Report(c.ctx, downloader.Progress{Stage: downloader.StageUploading, Done: c.done, Total: c.total})
	}
	return n, err
}

func (c *countingBody) Close() error { return c.rc.Close() }
