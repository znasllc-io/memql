// Package httpstream supplies bounded HTTP response streaming across proxies.
package httpstream

import (
	"context"
	"errors"
	"net/http"
	"time"
)

const downloadLimit = 30 * time.Minute
const downloadWriteIdle = 30 * time.Second

// LongDownloadLimit is the ceiling for a download whose size has no cap. The
// Library caps a file at 4 GiB, which the 30-minute default carries at an
// ordinary rate; a finished account export has no such cap and can be many
// gigabytes, so its route, on the bff and on the edge's hop, uses this one. The
// idle write deadline is unchanged: it is what stops a stalled reader.
const LongDownloadLimit = 6 * time.Hour

// Download replaces a short whole-response server deadline with an idle write
// deadline and an overall transfer ceiling. Both the serving handler and every
// HTTP proxy hop must use it; extending only the origin leaves the proxy's
// ordinary WriteTimeout in force. Caller cancellation and earlier deadlines
// survive. It grants no authority and must only wrap a download route.
func Download(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request, context.CancelFunc, error) {
	return DownloadWithin(w, r, downloadLimit)
}

// DownloadWithin is Download with a caller-chosen transfer ceiling in place of
// the 30-minute default. Only the ceiling differs; the idle write deadline, the
// caller's cancellation and any earlier deadline behave exactly as in Download.
func DownloadWithin(w http.ResponseWriter, r *http.Request, limit time.Duration) (http.ResponseWriter, *http.Request, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(r.Context(), limit)
	writer := &downloadWriter{ResponseWriter: w, ctx: ctx}
	if err := writer.deadline(); err != nil {
		cancel()
		return w, r, func() {}, err
	}
	return writer, r.WithContext(ctx), cancel, nil
}

type downloadWriter struct {
	http.ResponseWriter
	ctx context.Context
}

func (w *downloadWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *downloadWriter) deadline() error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(downloadWriteIdle)
	if ceiling, ok := w.ctx.Deadline(); ok && ceiling.Before(deadline) {
		deadline = ceiling
	}
	err := http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
	// Recorders and non-network embedders may not expose a connection.
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func (w *downloadWriter) Write(p []byte) (int, error) {
	if err := w.deadline(); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(p)
}
