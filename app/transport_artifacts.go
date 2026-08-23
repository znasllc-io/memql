//go:build bff

package app

import (
	"context"

	"github.com/znasllc-io/memql/component/fileprocessor"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/library"
)

// mountLibraryArtifactEndpoints wires the Library's two byte-bearing HTTP
// routes (memql#4341) onto the bff's mux:
//
//	POST /artifacts                 -- upload any file; the actor owns it
//	GET  /artifacts/{id}/content    -- export a file's bytes, or a note /
//	                                   generated output / memory's body
//
// BFF-ONLY, unlike mountAttachmentEndpoints' `bff || agent`. The Library is a
// user-facing surface the portal dials; nothing on the agent uploads to it,
// and mounting a route on a node no client addresses is how a declaration
// stops meaning anything.
//
// uploader/container are the Azure Blob client mountAttachmentEndpoints
// already constructed for the SAME storage account -- reused rather than
// building a second one, the same reuse mountSiteBundleEndpoints does for the
// publish path.
//
// # Why the downloader comes from a type assertion
//
// mountAttachmentEndpoints returns server.FileUploader and a container name;
// it holds the DOWNLOADER too, but does not return it, and widening its
// signature would edit the shared bff+agent file for a bff-only consumer.
// *azureblob.AzureBlobUploader satisfies both interfaces -- it is the very
// same value transport_attachments.go passes as its own Downloader -- so the
// assertion recovers it with no second client and no cross-build churn.
//
// The assertion is CHECKED AND SHOUTED ABOUT rather than silently dropped. A
// nil downloader is not an error at boot (an operator with no storage
// configured is a supported state, and the route still mounts), but a
// CONFIGURED uploader that does not download would leave every export
// answering 404 with nothing in the logs to say why -- the silent-degradation
// shape this tree keeps closing. So the two cases are distinguished: nothing
// configured is expected, a configured client that cannot download is loud.
func (a *App) mountLibraryArtifactEndpoints(uploader server.FileUploader, container string) {
	var downloader server.FileDownloader
	if uploader != nil {
		if dl, ok := uploader.(server.FileDownloader); ok {
			downloader = dl
		} else {
			a.Logger.Error("library: the configured blob uploader cannot download, so "+
				"GET /artifacts/{id}/content will answer 404 for every file artifact",
				"detail", "server.FileDownloader is not implemented by the uploader "+
					"mountAttachmentEndpoints built; the export route needs it to stream "+
					"stored bytes back")
		}
	}

	handler := server.NewArtifactHandler(server.ArtifactHandlerOptions{
		Logger:     a.Logger,
		Bucket:     container,
		Uploader:   uploader,
		Downloader: downloader,
		Store:      server.NewEngineLibraryStore(&AttachmentEngineAdapter{Engine: a.engine}),
		Analyzer:   a.newLibraryAnalyzer(),
	})

	// BOTH spellings, both verbs. server.ArtifactPaths() returns /artifacts and
	// /artifacts/ (plus their base-prefixed forms) and the reason is in its own
	// doc comment: registering only the subtree pattern makes ServeMux answer
	// POST /artifacts with a 301, and a 301 on a POST loses the body. The
	// handler dispatches on method and path shape, so an unmatched combination
	// (POST to the content path, GET to the collection) is its 404 to give.
	for _, path := range server.ArtifactPaths() {
		a.handleRoute("POST "+path, handler) // upload
		a.handleRoute("GET "+path, handler)  // download / export
	}
}

// newLibraryAnalyzer builds the adapter that lets the upload route hand a
// stored file to memql#4342's analysis pass (extract -> summarise -> chunk ->
// embed), translating server.LibraryAnalysisRequest into
// library.AnalyzeFileParams. app/ is in the unsplit root module and can import
// both sides, which is why the translation lives here and not in either --
// the sitePublisherAdapter shape in transport_sites.go.
//
// A SECOND *library.Integration IS DELIBERATE. The plug-in registry already
// builds one, but it is reached through the DSL executor rather than as a Go
// value, and PluginContext hands out no accessor for it. The type holds an
// engine handle plus two optional collaborators, so a second instance costs a
// pointer and shares all the state that matters -- the graph.
//
// The extractor is the SAME fileprocessor.DefaultProcessor the attachment
// route analyses with (transport_attachments.go), so "the known types" means
// one set across both upload paths rather than two that drift.
func (a *App) newLibraryAnalyzer() server.LibraryAnalyzer {
	lib := library.NewIntegration(a.engine)
	lib.SetLogger(a.Logger)
	lib.SetExtractor(fileprocessor.NewDefaultProcessor(a.engine.VisionProvider()))
	return libraryAnalyzerAdapter{lib: lib}
}

// libraryAnalysisRunner is the one method the adapter needs from
// *library.Integration. Declared as an interface purely so the field mapping
// below is assertable: a struct-to-struct copy is exactly the code where a
// field added upstream gets forgotten, and the symptom -- an empty Name, an
// empty MimeType -- is a worse summary rather than a failure anybody sees.
type libraryAnalysisRunner interface {
	AnalyzeFile(ctx context.Context, params library.AnalyzeFileParams) error
}

type libraryAnalyzerAdapter struct{ lib libraryAnalysisRunner }

// AnalyzeFile calls the SYNCHRONOUS pass, never StartFileAnalysis. The handler
// has already detached, and the ctx it hands over already carries the file
// owner's actor (auth.ContextWithUserActor) -- a second detach would build a
// fresh background context and discard that identity, and every write in the
// pass is owner-scoped.
//
// The error is dropped ON PURPOSE, and this is the one place it is safe to:
// the pass records its own failure on the row as status=failed with a
// failureReason before returning, so the reason is already where a person can
// see it. Logging here too would double-report it; returning it has nowhere to
// go, because the HTTP response was written when the bytes became durable.
func (a libraryAnalyzerAdapter) AnalyzeFile(ctx context.Context, req server.LibraryAnalysisRequest) {
	_ = a.lib.AnalyzeFile(ctx, library.AnalyzeFileParams{
		FileId:      req.FileId,
		ArtifactId:  req.ArtifactId,
		OwnerUserId: req.OwnerUserId,
		Name:        req.Name,
		MimeType:    req.MimeType,
		Data:        req.Data,
	})
}
