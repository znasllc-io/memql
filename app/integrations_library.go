package app

import (
	"context"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/fileprocessor"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/integrations/library"
)

// The registered Library instance handles document review calls. It must read
// the same stored bytes as artifact downloads, including on another replica.
func (a *App) wireLibraryIntegration(uploader server.FileUploader, bucket string) {
	if a.engine == nil {
		return
	}
	integ, ok := a.engine.IntegrationByName("library").(*library.Integration)
	if !ok {
		return
	}
	if work := a.lookupWorkIntegration(); work != nil {
		integ.SetReviewGoals(work)
	}
	// The store's file-version gate runs on the gate pool (memql#6022), not on the
	// request pool the engine under it works on.
	imageStore := server.NewEngineLibraryStore(&AttachmentEngineAdapter{Engine: a.engine}, a.gateDBGetter())
	integ.SetRevisionFiles(&libraryRevisionFiles{uploader: uploader, bucket: bucket, store: imageStore, authority: a.engine})
	integ.SetImageAssets(&libraryImageAssets{engine: &AttachmentEngineAdapter{Engine: a.engine}, store: imageStore, uploader: uploader, bucket: bucket})
	if reader, ok := uploader.(library.BlobFetcher); ok {
		integ.SetBlobFetcher(reader)
	}
	// A recipient's copy of a shared file is written as a new, verified object
	// in their own library/{owner}/ prefix (file sharing, 2026-11-18).
	if writer, ok := uploader.(library.ShareCopyStore); ok {
		integ.SetShareCopyStorage(writer, bucket)
	}
	// ...and read like an upload, so search by meaning finds it.
	a.configureLibraryReading(integ)
}

// configureLibraryReading gives a Library integration what the reading pass
// needs: the logger, the extractor and the work journal. The upload route's
// analyzer and the registered instance (a saved copy of a shared file is read
// too, file sharing Nov 18) are both configured here, so the two cannot drift.
func (a *App) configureLibraryReading(lib *library.Integration) {
	lib.SetLogger(a.Logger)
	lib.SetExtractor(fileprocessor.NewDefaultProcessor(a.resolveVisionProvider(context.Background())))
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
}
