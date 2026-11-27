package bot

import (
	"context"
	"io"
	"net/http"
	"net/url"
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
//
// A non-nil proxy routes every API request (polling, sends, uploads) through
// it; otherwise the standard HTTPS_PROXY environment variables apply.
func newAPIClient(proxy *url.URL) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != nil {
		t.Proxy = http.ProxyURL(proxy)
	}
	t.ResponseHeaderTimeout = apiPollTimeout + 30*time.Second // getUpdates holds the response for up to apiPollTimeout
	// A local Bot API server answers a file send only after it has passed the
	// file on to Telegram, which takes minutes for a few hundred MB.
	upload := t.Clone()
	upload.ResponseHeaderTimeout = uploadResponseTimeout
	return &http.Client{Transport: uploadProgressTransport{base: t, upload: upload}}
}

// uploadResponseTimeout is how long a file send may take to be answered.
const uploadResponseTimeout = 15 * time.Minute

// uploadProgressTransport reports upload progress for requests marked
// withUpload, and sends them with the longer uploadResponseTimeout.
type uploadProgressTransport struct{ base, upload http.RoundTripper }

func (t uploadProgressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	marked, _ := req.Context().Value(uploadKey{}).(bool)
	if !marked {
		return t.base.RoundTrip(req)
	}
	if req.Body == nil || req.Body == http.NoBody || req.ContentLength <= 0 {
		return t.upload.RoundTrip(req)
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
	return t.upload.RoundTrip(out)
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
