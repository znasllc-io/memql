//go:build bff

package app

import (
	"context"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/fileprocessor"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/server/uploadsession"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/integrations/library"
)

// mountLibraryArtifactEndpoints wires the Library's two byte-bearing HTTP
// routes (memql#4341) onto the bff's mux:
//
//	POST /artifacts                 -- upload any file; the actor owns it
//	GET  /artifacts/{id}/content    -- export a file's bytes, or a note /
//	                                   generated output / memory's body
//
// BFF-ONLY, unlike resolveBlobStore' `bff || agent`. The Library is a
// user-facing surface the portal dials; nothing on the agent uploads to it,
// and mounting a route on a node no client addresses is how a declaration
// stops meaning anything.
//
// uploader/container are the Azure Blob client resolveBlobStore
// already constructed for the SAME storage account -- reused rather than
// building a second one, the same reuse mountSiteBundleEndpoints does for the
// publish path.
//
// # Why the downloader comes from a type assertion
//
// resolveBlobStore returns server.FileUploader and a container name;
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
					"resolveBlobStore built; the export route needs it to stream "+
					"stored bytes back")
		}
	}

	// The chunked-transfer collaborators (memql#4782) come off the SAME blob
	// client, by the same checked-assertion pattern the downloader uses: the
	// Azure uploader implements block staging and range streaming, a fake or
	// alternative backend may not, and either half absent leaves the one-shot
	// route fully working while the session routes answer 501 honestly.
	var blocks server.BlockStore
	var streamer server.StreamDownloader
	if uploader != nil {
		if b, ok := uploader.(server.BlockStore); ok {
			blocks = b
		} else {
			a.Logger.Error("library: the configured blob uploader cannot stage blocks, so " +
				"chunked uploads (POST /artifacts/uploads) will answer 501")
		}
		if s, ok := uploader.(server.StreamDownloader); ok {
			streamer = s
		} else {
			a.Logger.Warn("library: the configured blob uploader cannot stream, so " +
				"GET /artifacts/{id}/content serves buffered bytes with no Range support")
		}
	}

	handler := server.NewArtifactHandler(server.ArtifactHandlerOptions{
		Logger:     a.Logger,
		Bucket:     container,
		Uploader:   uploader,
		Downloader: downloader,
		Store:      server.NewEngineLibraryStore(&AttachmentEngineAdapter{Engine: a.engine}),
		Analyzer:   a.newLibraryAnalyzer(uploader),
		Sessions:   uploadsession.NewStore(&AttachmentEngineAdapter{Engine: a.engine}),
		Blocks:     blocks,
		Streamer:   streamer,
	})

	// BOTH spellings, both verbs -- and PUT (memql#4782). server.ArtifactPaths()
	// returns /artifacts and /artifacts/ (plus their base-prefixed forms) and
	// the reason is in its own doc comment: registering only the subtree
	// pattern makes ServeMux answer POST /artifacts with a 301, and a 301 on a
	// POST loses the body. The handler dispatches on method and path shape, so
	// an unmatched combination (POST to the content path, GET to the
	// collection) is its 404 to give. PUT carries exactly one route -- the
	// chunk upload -- and rides the same registration because the session
	// paths live under the /artifacts prefix.
	for _, path := range server.ArtifactPaths() {
		a.handleRoute("POST "+path, handler) // upload, session init/complete
		a.handleRoute("GET "+path, handler)  // download / export, session inventory
		a.handleRoute("PUT "+path, handler)  // session chunks
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
func (a *App) newLibraryAnalyzer(uploader server.FileUploader) server.LibraryAnalyzer {
	lib := library.NewIntegration(a.engine)
	lib.SetLogger(a.Logger)
	lib.SetExtractor(fileprocessor.NewDefaultProcessor(a.engine.VisionProvider()))
	// The blob fetcher (memql#4782): the pass streams a chunked upload's
	// committed blob once, for the hash always and for extraction when
	// readable. Same checked-assertion pattern as the transport's own
	// collaborators; absent, chunked files keep an absent sha256, which the
	// field documents as "not measured".
	if fetcher, ok := uploader.(library.BlobFetcher); ok {
		lib.SetBlobFetcher(fetcher)
	}
	// The work spine's journal (epic memql#4970, spec section G): the pass
	// becomes a system-origin goal with one run per attempt and a step per
	// stage, which is what the Training app's feed reads. Absent, the pass
	// runs exactly as it did before -- the file row's `status` is then the
	// only account of it, which is what the app had.
	// MEMQL_NODE_ID straight from the environment rather than from
	// a.nodeIdentity: transport is wired before the cluster phase runs, so
	// the identity is not populated yet, and the env var is what the pod
	// carries (fieldRef: metadata.name) in every topology.
	lib.SetWorkJournal(workjournal.New(
		workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
			return a.engine.Execute(ctx, q)
		}),
		a.Logger,
		strings.TrimSpace(os.Getenv("MEMQL_NODE_ID")),
	))
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
		// The chunked pair (memql#4782): where the committed bytes live,
		// and whether anyone has hashed them yet. With Data present and
		// Sha256 known -- every one-shot upload -- both are inert.
		BlobUrl: req.BlobUrl,
		Sha256:  req.Sha256,
	})
}
