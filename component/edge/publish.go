// component/edge/publish.go
package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	"path"
	"sort"

	"github.com/znasllc-io/memql/integrations/azureblob"
)

// Bundle is the set of files a build produced, keyed by their path within the
// site (e.g. "index.html", "assets/app.js").
type Bundle map[string][]byte

// BlobWriter is the write half of object storage. Separate from BlobClient so
// the serving path cannot write, which is a real boundary and not just tidiness.
type BlobWriter interface {
	Put(ctx context.Context, key string, data []byte) error
}

// SiteStore is the one mutation the publisher performs.
type SiteStore interface {
	UpdateBundleRef(ctx context.Context, siteID, bundleRef string) error
}

// Result is what a successful publish produced.
type Result struct {
	Version   string
	BundleRef string
}

// Publisher is the write side of a site's bundle -- the whole reason this
// package exports anything beyond a read-only serving Handler.
//
// component/edge is named after the edge node, but Publisher is deliberately
// a LIBRARY here, not a node-mounted endpoint: the bff imports it and mounts
// the HTTP handler over it (memql#3713's controller ruling for
// POST /sites/{id}/bundles). That is a slightly unusual direction for a
// package named after a node type, so it is worth naming both why, and the
// alternative rejected:
//
//   - There is no coherent address for a site-agnostic publish endpoint on
//     the edge. The edge is wildcard-routed BY SITE HOSTNAME (resolve.go's
//     Resolver), so "POST /sites/{id}/bundles" would have to arrive at some
//     arbitrary site's own origin to reach an endpoint that has nothing to
//     do with that site.
//   - Neither half of the work needs the edge node. Publish writes to
//     object storage and flips one graph row; it never serves a byte.
//
// The rejected alternative is mounting the publish route on the edge's own
// HTTP mount alongside Handler. That mount is declared as exactly "/"
// (component/server.EdgePaths(), edge build tag only) and its boot check
// refuses any route not accounted for -- adding a second route there would
// be a real change to a security-relevant declaration, not a wiring detail,
// and it would buy nothing: the edge would still just be forwarding to the
// same Publish call the bff can invoke directly.
type Publisher struct {
	blobs BlobWriter
	sites SiteStore
}

func NewPublisher(blobs BlobWriter, sites SiteStore) *Publisher {
	return &Publisher{blobs: blobs, sites: sites}
}

// version derives the version id from the bundle's CONTENT, not from a clock.
//
// Two reasons, and the second is the load-bearing one. A content hash makes a
// republish of identical bytes a no-op rather than a new version accumulating
// storage forever. And it makes this function deterministic, so the tests
// above are repeatable and a version id can be verified against the bytes it
// names -- a timestamp can be neither.
func version(b Bundle) string {
	names := make([]string, 0, len(b))
	for name := range b {
		names = append(names, name)
	}
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(b[name]))
		h.Write(b[name])
	}
	return "v" + hex.EncodeToString(h.Sum(nil))[:12]
}

// Publish uploads the whole bundle under a NEW version prefix and only then
// flips the row.
//
// THE ORDER IS THE FEATURE. A failure at any point during the upload leaves
// the row pointing at the previous version, whose bytes are untouched -- so a
// half-uploaded bundle is never reachable, and there is no cleanup path to get
// wrong. Overwriting a prefix in place would make a deploy non-atomic AND
// destroy the bytes rollback needs.
func (p *Publisher) Publish(ctx context.Context, siteID string, b Bundle) (Result, error) {
	if len(b) == 0 {
		return Result{}, fmt.Errorf("edge: refusing to publish an empty bundle to %s", siteID)
	}
	if _, ok := b["index.html"]; !ok {
		// A bundle with no index.html serves nothing at "/" and nothing at
		// any spa-fallback path. Refusing here turns a broken build into a
		// failed publish rather than a live site that 404s its own homepage.
		return Result{}, fmt.Errorf("edge: bundle for %s has no index.html", siteID)
	}

	v := version(b)
	prefix := fmt.Sprintf("sites/%s/%s/", siteID, v)

	names := make([]string, 0, len(b))
	for name := range b {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic upload order, so a failure is reproducible

	for _, name := range names {
		if err := p.blobs.Put(ctx, prefix+name, b[name]); err != nil {
			return Result{}, fmt.Errorf("edge: uploading %s for %s: %w", name, siteID, err)
		}
	}

	ref := "blob://" + prefix
	if err := p.sites.UpdateBundleRef(ctx, siteID, ref); err != nil {
		// The bytes are uploaded and orphaned. That is the RIGHT failure:
		// storage is cheap, and the alternative -- flipping the row first --
		// serves a bundle that may not be fully there.
		return Result{}, fmt.Errorf("edge: pointing %s at %s: %w", siteID, ref, err)
	}

	return Result{Version: v, BundleRef: ref}, nil
}

// AzureUploader is the one method this adapter needs from an Azure blob
// client -- the write-half mirror of blob.go's AzureDownloader, narrowed the
// same way so a test can fake the write without an Azure SDK client or a
// live account. The assertion below pins *azureblob.AzureBlobUploader (the
// same client blob.go's AzureDownloader assertion pins for reads) as
// satisfying it today.
type AzureUploader interface {
	Upload(ctx context.Context, container, objectName string, data []byte, contentType string) (url string, err error)
}

var _ AzureUploader = (*azureblob.AzureBlobUploader)(nil)

// azureBlobWriter adapts an AzureUploader to BlobWriter, scoped to one
// container -- the write-half mirror of blob.go's azureBlobClient.
//
// THIS IS NOT A SECOND AZURE CLIENT, for the same reason azureBlobClient
// isn't one either: connecting, auth, and the read half (Download, behind
// BlobClient) stay owned entirely by integrations/azureblob and blob.go
// respectively; this type only ever calls Upload.
type azureBlobWriter struct {
	uploader  AzureUploader
	container string
}

// NewAzureBlobWriter adapts an already-constructed Azure blob uploader -- in
// production, *azureblob.AzureBlobUploader -- to BlobWriter. container
// should be the SAME value passed to NewAzureBlobClient
// (azureblob.ContainerFromEnv(), MEMQL_AZURE_BLOB_CONTAINER): a publish and
// the reads that follow it must land in the same container, or nothing an
// operator uploads is ever reachable.
func NewAzureBlobWriter(u AzureUploader, container string) BlobWriter {
	return &azureBlobWriter{uploader: u, container: container}
}

// Put uploads data to key under the configured container.
func (a *azureBlobWriter) Put(ctx context.Context, key string, data []byte) error {
	if _, err := a.uploader.Upload(ctx, a.container, key, data, bundleFileContentType(key, data)); err != nil {
		return fmt.Errorf("edge: uploading %q to container %q: %w", key, a.container, err)
	}
	return nil
}

// bundleFileContentType picks a MIME type for one file in a published
// bundle, the same way integrations/workbench's detectMimeType picks one for
// an uploaded file: extension first (mime.TypeByExtension), then content
// sniffing (http.DetectContentType) on the bytes, falling back to
// application/octet-stream. Extension wins because sniffing alone reports a
// generic text/plain (or worse, application/octet-stream) for structured
// text formats a browser needs the real Content-Type for.
func bundleFileContentType(name string, data []byte) string {
	if ext := path.Ext(name); ext != "" {
		if mt := mime.TypeByExtension(ext); mt != "" {
			return mt
		}
	}
	if len(data) > 0 {
		return http.DetectContentType(data)
	}
	return "application/octet-stream"
}
