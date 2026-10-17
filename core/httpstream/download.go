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

// Download replaces a short whole-response server deadline with an idle write
// deadline and an overall transfer ceiling. Both the serving handler and every
// HTTP proxy hop must use it; extending only the origin leaves the proxy's
// ordinary WriteTimeout in force. Caller cancellation and earlier deadlines
// survive. It grants no authority and must only wrap a download route.
func Download(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(r.Context(), downloadLimit)
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
