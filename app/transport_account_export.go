//go:build bff

package app

import (
	"context"
	"fmt"
	"io"

	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/accounts"
)

// mountAccountExportDownload wires GET /accounts/exports/{id}/content (account
// export design, section G; owner-approved HTTP exception 2026-11-12) onto the
// bff's mux. Bff-only, like the Library's byte routes: it is a signed-in
// person's download, and the front door routes /accounts/exports/ to bff-http.
//
// GET only. The handler refuses every other method itself (HEAD included,
// which ServeMux would otherwise admit under a GET pattern).
//
// THE PLUG-IN MAY BE ABSENT. The route still mounts, so a front-door rule never
// falls through to the h2c catch-all, and with a nil Downloads the handler
// answers 404 to everything. One line at startup says why, because "every
// export is not found" with nothing in the logs is the silent shape this tree
// keeps closing.
func (a *App) mountAccountExportDownload() {
	var downloads server.AccountExportDownloads
	if a.engine != nil {
		if provider, _ := a.engine.IntegrationByName(accounts.IntegrationName).(*accounts.Provider); provider != nil && provider.Exports != nil {
			downloads = accountExportDownloads{exports: provider.Exports}
		}
	}
	if downloads == nil {
		a.Logger.Warn("account export download not wired: the accounts plug-in did not materialize on this bff node, so GET /accounts/exports/{id}/content answers 404",
			"component", "accounts")
	}
	handler := server.NewAccountExportHandler(server.AccountExportHandlerOptions{
		Logger:    a.Logger,
		Downloads: downloads,
	})
	for _, path := range server.AccountExportPaths() {
		a.handleRoute("GET "+path, handler)
	}
}

// accountExportSource is the part of *accounts.Exports the route uses, named so
// the adapter can be tested against a fake.
type accountExportSource interface {
	ResolveExportForDownload(ctx context.Context, exportID string) (accounts.Export, bool, error)
	OpenExport(ctx context.Context, x accounts.Export, offset, length int64) (io.ReadCloser, error)
	RecordExportDownload(ctx context.Context, x accounts.Export, userID, rangeAsked string) error
}

// accountExportDownloads implements server.AccountExportDownloads over the
// accounts plug-in.
//
// WHY OPEN AND RECORDDOWNLOAD LOOK THE ROW UP AGAIN rather than keep what
// ResolveForDownload found. The handler passes back only an
// AccountExportFile, and the adapter is shared by every request, so a kept
// record would be a cache keyed by something: a stale or crossed entry would
// hand one export's bytes out under another's audit event. Looking the row up
// by the same id instead means the object opened and the event written are
// both of the row that id names now, and the row must still be the file the
// handler announced (same account, object key, size and digest), so a file
// removed or replaced between the two reads is refused, not served. Both
// refusals are errors, which the handler answers with 500 before any byte.
// The cost is two more lookups of one row per request.
type accountExportDownloads struct {
	exports accountExportSource
}

func (d accountExportDownloads) ResolveForDownload(ctx context.Context, exportID string) (server.AccountExportFile, bool, error) {
	x, ok, err := d.exports.ResolveExportForDownload(ctx, exportID)
	if err != nil || !ok {
		return server.AccountExportFile{}, false, err
	}
	return accountExportFile(x), true, nil
}

func (d accountExportDownloads) Open(ctx context.Context, f server.AccountExportFile, offset, length int64) (io.ReadCloser, error) {
	x, err := d.sameRow(ctx, f)
	if err != nil {
		return nil, err
	}
	return d.exports.OpenExport(ctx, x, offset, length)
}

func (d accountExportDownloads) RecordDownload(ctx context.Context, f server.AccountExportFile, userID, rangeAsked string) error {
	x, err := d.sameRow(ctx, f)
	if err != nil {
		return err
	}
	return d.exports.RecordExportDownload(ctx, x, userID, rangeAsked)
}

// sameRow looks the export up again by id and requires it to be the file f
// describes.
func (d accountExportDownloads) sameRow(ctx context.Context, f server.AccountExportFile) (accounts.Export, error) {
	x, ok, err := d.exports.ResolveExportForDownload(ctx, f.ID)
	if err != nil {
		return accounts.Export{}, err
	}
	if !ok {
		return accounts.Export{}, fmt.Errorf("account export %s is no longer available for download", f.ID)
	}
	if accountExportFile(x) != f {
		return accounts.Export{}, fmt.Errorf("account export %s changed while it was being downloaded", f.ID)
	}
	return x, nil
}

func accountExportFile(x accounts.Export) server.AccountExportFile {
	return server.AccountExportFile{
		ID: x.ID, AccountID: x.AccountID, FileName: x.FileName,
		ObjectKey: x.ObjectKey, SHA256: x.SHA256, SizeBytes: x.SizeBytes,
	}
}
