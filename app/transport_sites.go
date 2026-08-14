//go:build bff

package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/component/server"
)

// mountSiteBundleEndpoints wires the atomic bundle-publish endpoint
// (POST /sites/{id}/bundles, memql#3713) onto the bff's mux.
//
// component/edge is a LIBRARY here, not a node-mounted endpoint -- see the
// package comment on component/edge/publish.go's Publisher type for the
// full reasoning (the edge node is wildcard-routed by site hostname, so a
// site-agnostic publish endpoint has no coherent address there) and the
// alternative it rejects. This file is the bff half of that split: the
// same two-step shape mountEdgeEndpoints (transport_edge.go) uses to build
// a Resolver + Handler for the READ side of the same package, applied to
// build a Publisher + SiteBundleHandler for the write side.
//
// uploader/container are the Azure Blob client mountAttachmentEndpoints
// (transport_attachments.go) already constructed for the SAME account --
// reused rather than building a second client, per that function's own doc
// comment ("so the agent node can additionally hand them to the
// workbench/worker integrations"): this is that same reuse, on the bff,
// for the publish path instead. server.FileUploader and edge.AzureUploader
// are the identical method set (Upload(ctx, container, objectName string,
// data []byte, contentType string) (string, error)), so the same
// *azureblob.AzureBlobUploader instance satisfies both with no adapter.
//
// Reuses EdgeEngineAdapter (adapters.go) for the engine seam -- the same
// (any, error) adapter mountEdgeEndpoints already uses for
// edge.NewEngineExecutor -- rather than inventing a second way to reach
// the engine.
//
// Wraps the resulting *edge.Publisher in sitePublisherAdapter before handing
// it to server.NewSiteBundleHandler: component/server.BundlePublisher is
// declared in component/server's OWN types (map[string][]byte,
// SiteBundlePublishResponse), not component/edge's (Bundle, Result) --
// see BundlePublisher's doc comment for why component/server cannot import
// component/edge at all (it is a tiered module, memql#3228, that has no
// relative-path replace reaching the unsplit root module component/edge
// lives in). This file -- part of the unsplit root module itself -- is
// where the two sides meet.
func (a *App) mountSiteBundleEndpoints(uploader server.FileUploader, container string) {
	publisher := edge.NewPublisher(
		siteBundleBlobWriter(a.Logger, uploader, container),
		edge.NewEngineSiteStore(&EdgeEngineAdapter{Engine: a.engine}),
	)
	handler := server.NewSiteBundleHandler(server.SiteBundleHandlerOptions{
		Logger:    a.Logger,
		Publisher: sitePublisherAdapter{pub: publisher},
	})
	for _, path := range server.SitesBundlePaths() {
		a.handleRoute("POST "+path, handler)
	}
}

// sitePublisherAdapter adapts *edge.Publisher to server.BundlePublisher --
// the module-boundary crossing mountSiteBundleEndpoints' doc comment
// describes. A plain type conversion in both directions: edge.Bundle IS
// map[string][]byte under the hood, and edge.Result's two fields are
// SiteBundlePublishResponse's two fields, so there is no real translation
// work here, just satisfying two independently-declared interfaces that
// happen to describe the same shape from either side of the module split.
type sitePublisherAdapter struct{ pub *edge.Publisher }

func (a sitePublisherAdapter) Publish(ctx context.Context, siteID string, files map[string][]byte) (server.SiteBundlePublishResponse, error) {
	res, err := a.pub.Publish(ctx, siteID, edge.Bundle(files))
	if err != nil {
		return server.SiteBundlePublishResponse{}, err
	}
	return server.SiteBundlePublishResponse{Version: res.Version, BundleRef: res.BundleRef}, nil
}

// siteBundleBlobWriter adapts the already-constructed uploader to
// edge.BlobWriter, or -- when object storage wasn't configured at all
// (mountAttachmentEndpoints's uploader is then a nil interface value) --
// returns a writer that always fails cleanly rather than silently
// succeeding.
//
// UNCONFIGURED IS A HARD FAILURE HERE, unlike mountAttachmentEndpoints' own
// graceful "fall back to a local:// placeholder" posture for attachments.
// An attachment row is still useful with an undownloadable placeholder URL
// -- the metadata and AI summary survive regardless. A site publish with
// nowhere to put the bytes has no useful degraded mode at all: if this
// silently "succeeded", the site row would flip to a blob:// bundleRef
// whose bytes were never written, 404ing the site for everyone who visits
// it, and the CI caller that published it would see 201 Created. Failing
// loud here is SAFE precisely because of Publisher.Publish's own atomicity
// guarantee: UpdateBundleRef is only ever reached after every Put succeeds,
// so a Put that fails immediately never touches the row -- the site keeps
// serving whatever it was serving before.
//
// The route still MOUNTS either way. A route this handler answers only
// sometimes, depending on environment, is exactly the shape of defect
// memql#3713 exists to close (a declaration nothing serves); this keeps it
// declared, served, and honest about storage instead of undeclaring it.
func siteBundleBlobWriter(logger *slog.Logger, uploader server.FileUploader, container string) edge.BlobWriter {
	if uploader == nil || container == "" {
		logger.Warn("edge: object storage not configured; POST /sites/{id}/bundles will refuse every publish",
			"component", "edge")
		return unconfiguredBlobWriter{}
	}
	return edge.NewAzureBlobWriter(uploader, container)
}

// unconfiguredBlobWriter is what Publisher gets when object storage isn't
// configured on this node. Put always fails -- see siteBundleBlobWriter's
// comment for why that is the safe behaviour, not a silent success.
type unconfiguredBlobWriter struct{}

func (unconfiguredBlobWriter) Put(context.Context, string, []byte) error {
	return fmt.Errorf("edge: object storage is not configured on this node " +
		"(set MEMQL_AZURE_BLOB_CONTAINER and MEMQL_AZURE_STORAGE_CONNECTION_STRING to publish site bundles)")
}
