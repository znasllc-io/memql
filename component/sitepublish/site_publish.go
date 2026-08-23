// site_publish.go -- deploy a Library artifact to a hosted site (memql#4345).
//
// `sitePublishFromArtifact(siteId, artifactId)` is the portal's Deployables
// button: the bytes are already in the cluster (a zip a person uploaded to
// their Library), so the deploy is a SERVER-SIDE act on rows the system
// already holds rather than a second upload route. `POST /sites/{id}/bundles`
// stays exactly what it was -- CI's door -- and gains nothing here (design
// D6).
//
// # Why this lives in integrations/library rather than component/edge
//
// component/edge.Publisher is a LIBRARY, deliberately (see the package
// comment on component/edge/publish.go's Publisher type): the edge node is
// wildcard-routed by site hostname, so a site-agnostic publish operation has
// no coherent address there. That leaves the caller to compose it, and the
// composition this capability needs -- a Library artifact, its backing file
// row, that file's bytes in object storage -- is Library-shaped. So the
// capability is registered here and imports component/edge for the two
// halves it does not own: Publisher (upload-then-flip, atomic) and the Azure
// read/write adapters.
//
// # The one operation, and what each step is protecting
//
//  1. Resolve the site through `siteById`. That query is scoped
//     `ownerUserId==actor.userId || actor.isClusterOwner` (memql#4344), so a
//     cross-user call resolves ZERO rows and is refused by name here rather
//     than reaching the publisher. It is refused a second time at the write
//     (guardRowAuthzWrite against v1:platform:site's composite tier) -- this
//     check exists to make the refusal legible, not to be the gate.
//  2. Resolve the artifact through `libraryArtifactById`, gated the same way
//     on ownerUserId. THE CALLER MUST OWN BOTH, and they are two separate
//     ownership questions, not one.
//  3. Resolve the backing v1:library:file from the artifact's
//     sourceConceptRef. Never re-derived from a hash expression -- the index
//     row names its own backing row, and that is the seam #4340 built.
//  4. Refuse anything that is not a zip, by the file row's own mimeType.
//  5. Read the bytes, expand the zip, and enforce the SAME per-file / total
//     / count limits the CI route enforces (see siteBundleLimits below).
//  6. Hand the expanded bundle to edge.Publisher.Publish, which uploads
//     under a fresh content-addressed version prefix and only then flips
//     bundleRef. The order is the atomicity: a failure mid-upload leaves the
//     site serving exactly what it was serving.
//  7. Stamp artifactId on the row and write an audit event.
//
// Rollback is not here and does not need to be: it is `updateSiteBundle`
// pointed back at an earlier version's ref, which is why versions are stored
// under distinct prefixes rather than overwritten.
package sitepublish

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/edge"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// sitePublishIntegrationName is the integration this capability registers
// under; sitePublishCapability is the capability within it. Together they
// form the executor FQN `integration.sitePublish.fromArtifact` that
// dsl/platform/builtins.memql's `sitePublishFromArtifact` names.
//
// A SEPARATE registration from the `library` integration in library.go,
// rather than a sixth capability on it, for one reason: this operation
// writes to v1:platform:site and reaches object storage, neither of which
// the Library edit integration touches, and folding it in would widen that
// integration's documented scope ("the server-side edit path for Library
// documents") to cover a deploy. Same package because it reads the Library's
// own rows; different name because it does a different thing.
const (
	sitePublishIntegrationName = "sitePublish"
	sitePublishCapability      = "fromArtifact"
)

// ---------------------------------------------------------------------------
// Limits -- MIRRORED from component/server/site_bundle_handler.go:20-45.
// ---------------------------------------------------------------------------

// The CI route (POST /sites/{id}/bundles) and this capability publish into
// the SAME storage and are served by the SAME edge, so a bundle one accepts
// and the other refuses is an inconsistency a person would experience as
// "it deploys from CI but not from the portal". The constants are duplicated
// rather than imported because component/server is a tiered module with its
// own go.mod (memql#3228) and declares them unexported; the duplication is
// therefore load-bearing to keep honest, which is what
// TestSiteBundleLimitsMatchTheCIRoute does -- it reads
// site_bundle_handler.go and fails if the numbers there ever move without
// these moving with them.
const (
	// sitePublishMaxFileBytes caps ONE file inside the bundle. Enforced on
	// the DECOMPRESSED size, twice: once against the zip entry's declared
	// uncompressed size before a byte is read (so a zip bomb is refused
	// rather than materialized), and once against what was actually read
	// (so a lying header is caught too).
	sitePublishMaxFileBytes = 25 * 1024 * 1024 // 25 MB

	// sitePublishMaxTotalBytes caps the whole EXPANDED bundle. A bundle's
	// danger is the sum, not any one file -- and unlike the multipart route,
	// where the transport bounds the request body, here the compressed zip
	// says nothing useful about what it expands to.
	sitePublishMaxTotalBytes = 500 * 1024 * 1024 // 500 MB

	// sitePublishMaxFileCount bounds the file COUNT independently of size:
	// a degenerate bundle of hundreds of thousands of near-empty files stays
	// under the byte caps while still driving that many individual blob Put
	// calls inside Publisher.Publish.
	sitePublishMaxFileCount = 20000
)

// sitePublishZipMimeTypes is the closed set of MIME types this capability
// accepts as "a zip". `application/zip` is the IANA registration; the rest
// are what real clients send (Windows and several browsers say
// x-zip-compressed, some older tools say x-zip / multipart/x-zip).
//
// `application/octet-stream` is deliberately NOT here. It is what an upload
// with no usable type falls back to, so accepting it would turn "the artifact
// must be a zip" into "the artifact must be anything" -- the check would
// still be present, and would no longer be a check.
var sitePublishZipMimeTypes = map[string]struct{}{
	"application/zip":              {},
	"application/x-zip":            {},
	"application/x-zip-compressed": {},
	"application/zip-compressed":   {},
	"multipart/x-zip":              {},
}

// sitePublishIndexKinds names the site kinds whose bundle MUST carry an
// index.html at its root. A `static` site is not in the set here -- but
// edge.Publisher.Publish refuses an index-less bundle for every kind anyway,
// so the difference is only which refusal a caller sees, not whether one
// happens. Naming spa + shopify_storefront explicitly is what lets those two
// fail with a reason that says what is wrong.
var sitePublishIndexKinds = map[string]struct{}{
	"spa":                {},
	"shopify_storefront": {},
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

// publishRefusal is a refusal with a STABLE, machine-readable reason.
//
// Every failure path below names itself. That is not cosmetic: the portal
// surfaces the reason to the person who clicked deploy, and the audit row
// records it as failureReason -- a wrapped stringly error would leave both
// with prose that changes whenever someone edits a message.
type publishRefusal struct {
	Reason string
	Detail string
}

func (r *publishRefusal) Error() string {
	if r.Detail == "" {
		return fmt.Sprintf("sitePublishFromArtifact refused: %s", r.Reason)
	}
	return fmt.Sprintf("sitePublishFromArtifact refused: %s -- %s", r.Reason, r.Detail)
}

func refuse(reason, format string, a ...any) error {
	return &publishRefusal{Reason: reason, Detail: fmt.Sprintf(format, a...)}
}

// refusalReason reports the stable reason carried by err, or "" when err is
// not a refusal. Exposed to the tests so they assert on the NAME rather than
// on message text.
func refusalReason(err error) string {
	var r *publishRefusal
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

// The refusal vocabulary, in the order the operation can reach them.
const (
	reasonMissingArgument   = "missing_argument"
	reasonSiteNotFound      = "site_not_found"
	reasonArtifactNotFound  = "artifact_not_found"
	reasonArtifactArchived  = "artifact_archived"
	reasonArtifactNotAFile  = "artifact_not_a_file"
	reasonFileNotFound      = "file_not_found"
	reasonFileArchived      = "file_archived"
	reasonArtifactNotAZip   = "artifact_not_a_zip"
	reasonStorageNotReady   = "storage_not_configured"
	reasonBundleUnreadable  = "bundle_unreadable"
	reasonBundleNotAZip     = "bundle_not_a_zip"
	reasonBundlePathInvalid = "bundle_path_invalid"
	reasonBundleTooManyFile = "bundle_too_many_files"
	reasonBundleFileTooBig  = "bundle_file_too_large"
	reasonBundleTooLarge    = "bundle_too_large"
	reasonBundleEmpty       = "bundle_empty"
	reasonBundleNoIndex     = "bundle_missing_index"
	reasonPublishFailed     = "publish_failed"
)

// ---------------------------------------------------------------------------
// The integration
// ---------------------------------------------------------------------------

// publishEngine is the ONLY engine surface this capability needs: one
// method, the same narrow seam component/identity/audit_db.go and every
// other Go component in this tree uses to talk to the engine. Narrow on
// purpose -- a test fakes four named queries and nothing else.
type publishEngine interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

// bundleByteReader is the read half of object storage -- the mirror of
// edge.BlobWriter, and exactly edge.BlobClient's shape so the production
// value is edge.NewAzureBlobClient with no adapter.
type bundleByteReader interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

// siteObjectStore pairs the two halves. Both are scoped to the same
// container, which is the whole reason they are resolved together: a read
// from one container and a write to another produces a site whose bytes are
// never where the edge looks for them.
type siteObjectStore struct {
	reader bundleByteReader
	writer edge.BlobWriter
}

// SitePublishIntegration exposes `integration.sitePublish.fromArtifact`.
//
// Object storage is resolved LAZILY, once, on first use rather than at
// construction. The plug-in factory runs on every node type and receives
// only PluginContext, which carries no blob client; resolving eagerly would
// either build an Azure client on nodes that never publish, or make the
// factory fail on a cluster that hosts no blob:// site at all. Unconfigured
// storage is therefore a per-call refusal (storage_not_configured), not a
// boot failure.
type SitePublishIntegration struct {
	engine publishEngine
	logger *slog.Logger

	// newStore is the resolution seam. Production is
	// defaultSiteObjectStore; a test substitutes an in-memory pair.
	newStore func(ctx context.Context) (siteObjectStore, error)

	storeOnce sync.Once
	store     siteObjectStore
	storeErr  error
}

// NewSitePublishIntegration wires the engine handle. The factory is in
// init() below; this constructor is what the tests call with a stub engine
// and an in-memory store.
func NewSitePublishIntegration(engine publishEngine, logger *slog.Logger) *SitePublishIntegration {
	if logger == nil {
		logger = slog.Default()
	}
	return &SitePublishIntegration{
		engine:   engine,
		logger:   logger,
		newStore: defaultSiteObjectStore,
	}
}

// init self-registers the capability. Always on, on every node type: the
// builtin is declared in the core DSL tree (dsl/platform/builtins.memql),
// which every binary loads, and an integration that is registered NOWHERE
// only warns while one registered with the capability MISSING fails boot
// (AuditIntegrationExecutors). Registering everywhere is also honest -- the
// operation is a graph read plus object storage, neither of which is
// node-type-specific.
//
// Anchored by the same blank import of this package that plugin.go relies
// on (app/plugins_core.go).
// The name is spelled as a STRING LITERAL here rather than as
// sitePublishIntegrationName, and that is not an oversight. The taxonomy gate
// (module_taxonomy_test.go's TestEveryRegisteredPluginIsClassified) finds every
// registration by scanning the tree's source for exactly this literal, because
// a computed name could not be classified at PR time -- which is the whole
// property that gate exists to keep. A constant here would make this plugin
// INVISIBLE to it, and the gate would pass while covering one fewer plugin than
// it claims. TestSitePublishRegistrationNameIsTheLiteralTheGateScansFor asserts
// the literal and the constant still agree.
func init() {
	memql.RegisterPlugin("sitePublish", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewSitePublishIntegration(pctx.Engine, pctx.Logger), nil
	})
}

// IntegrationName implements memql.IntegrationProvider.
func (i *SitePublishIntegration) IntegrationName() string { return sitePublishIntegrationName }

// Capabilities implements memql.IntegrationProvider.
func (i *SitePublishIntegration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        sitePublishCapability,
			Description: "Deploy a Library zip artifact to a hosted site (memql#4345). The caller must own the site AND the artifact (or be a cluster owner for the site); the artifact must be a v1:library:file with a zip MIME type. The zip is read from object storage, expanded, and validated -- index.html at the root for spa / shopify_storefront, and the same per-file (25 MB), total (500 MB) and count (20000) limits POST /sites/{id}/bundles enforces -- then handed to component/edge's Publisher, which uploads under a fresh content-addressed version prefix and only then flips bundleRef. artifactId is stamped on the site row as provenance and a security audit event is written. Rollback is unchanged: updateSiteBundle pointed back at an earlier version's ref.",
			Handler:     i.handlePublishFromArtifact,
			ArgsSchema: map[string]string{
				"siteId":     "string (required) -- the v1:platform:site row id to publish to",
				"artifactId": "string (required) -- the v1:library:artifact index row id of the zip to publish",
			},
		},
	}
}

// defaultSiteObjectStore builds the production read/write pair from the same
// env the storage plug-in and the edge's own bundle opener already read
// (integrations/azureblob's ContainerFromEnv / New) -- never a second pair of
// storage variables, because a publish and the reads that follow it must land
// in one container.
func defaultSiteObjectStore(ctx context.Context) (siteObjectStore, error) {
	container := azureblob.ContainerFromEnv()
	if strings.TrimSpace(container) == "" {
		return siteObjectStore{}, fmt.Errorf("MEMQL_AZURE_BLOB_CONTAINER is not set on this node")
	}
	uploader, err := azureblob.New(ctx)
	if err != nil {
		return siteObjectStore{}, fmt.Errorf("azure blob client: %w", err)
	}
	return siteObjectStore{
		reader: edge.NewAzureBlobClient(uploader, container),
		writer: edge.NewAzureBlobWriter(uploader, container),
	}, nil
}

func (i *SitePublishIntegration) objectStore(ctx context.Context) (siteObjectStore, error) {
	i.storeOnce.Do(func() {
		if i.newStore == nil {
			i.storeErr = fmt.Errorf("no object-store resolver wired")
			return
		}
		i.store, i.storeErr = i.newStore(ctx)
	})
	return i.store, i.storeErr
}

// publishResult is the payload the capability returns. The portal reads
// version + bundleRef to render "deployed vN"; fileCount / totalBytes make a
// successful publish self-describing in a log line.
type publishResult struct {
	SiteId     string `json:"siteId"`
	ArtifactId string `json:"artifactId"`
	FileId     string `json:"fileId"`
	Version    string `json:"version"`
	BundleRef  string `json:"bundleRef"`
	FileCount  int    `json:"fileCount"`
	TotalBytes int    `json:"totalBytes"`
}

// handlePublishFromArtifact is the capability. Every step runs under the
// CALLER's ctx -- no synthetic actor anywhere in this file. That is the
// design: both reads are owner-scoped queries and the write is guarded
// against v1:platform:site's composite tier, so the caller's own authority
// is what admits or refuses each one. A synthetic cluster owner here would
// dissolve exactly the check the capability exists to make.
func (i *SitePublishIntegration) handlePublishFromArtifact(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.engine == nil {
		return nil, fmt.Errorf("sitePublishFromArtifact: integration not initialized")
	}

	siteId := strings.TrimSpace(asString(args["siteId"]))
	artifactId := strings.TrimSpace(asString(args["artifactId"]))

	res, err := i.publish(ctx, siteId, artifactId)
	i.audit(ctx, siteId, artifactId, res, err)
	if err != nil {
		return nil, err
	}

	payload, marshalErr := json.Marshal(res)
	if marshalErr != nil {
		return nil, fmt.Errorf("sitePublishFromArtifact: marshal result: %w", marshalErr)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("sitePublish:%s:%d", res.SiteId, time.Now().UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

func (i *SitePublishIntegration) publish(ctx context.Context, siteId, artifactId string) (publishResult, error) {
	if siteId == "" {
		return publishResult{}, refuse(reasonMissingArgument, "siteId is required")
	}
	if artifactId == "" {
		return publishResult{}, refuse(reasonMissingArgument, "artifactId is required")
	}

	// 1. The site. Zero rows means "no site by this id that YOU may write",
	//    which is one answer to two questions on purpose -- distinguishing
	//    "does not exist" from "is not yours" would be an existence oracle
	//    over other users' deployables.
	site, err := i.queryOne(ctx, fmt.Sprintf("query siteById(siteId: %s)", langparser.QuoteString(siteId)))
	if err != nil {
		return publishResult{}, fmt.Errorf("sitePublishFromArtifact: resolve site %q: %w", siteId, err)
	}
	if site == nil {
		return publishResult{}, refuse(reasonSiteNotFound,
			"no site %q is visible to this caller (it does not exist, is deleted, or belongs to another user)", siteId)
	}
	siteKind := stringField(site, "kind")
	// The row's own id, bare-ified, is what names the storage prefix -- the
	// same form POST /sites/{id}/bundles uses, so both routes version the
	// same site under one prefix and a rollback can cross between them.
	siteRowId := memql.BareShortId(stringField(site, "id"))
	if siteRowId == "" {
		siteRowId = siteId
	}

	// 2. The artifact index row. A SECOND ownership question, gated on
	//    ownerUserId==actor.userId by libraryArtifactById itself.
	artifact, err := i.queryOne(ctx, fmt.Sprintf("query libraryArtifactById(artifactId: %s)", langparser.QuoteString(artifactId)))
	if err != nil {
		return publishResult{}, fmt.Errorf("sitePublishFromArtifact: resolve artifact %q: %w", artifactId, err)
	}
	if artifact == nil {
		return publishResult{}, refuse(reasonArtifactNotFound,
			"no Library artifact %q is visible to this caller", artifactId)
	}
	if boolField(artifact, "archived") {
		return publishResult{}, refuse(reasonArtifactArchived,
			"artifact %q is archived; un-archive it before deploying from it", artifactId)
	}
	if kind := stringField(artifact, "kind"); kind != "file" {
		return publishResult{}, refuse(reasonArtifactNotAFile,
			"artifact %q is kind %q; only a file artifact carries bytes to deploy", artifactId, kind)
	}

	// 3. The backing file. Resolved from the index row's OWN sourceConceptRef
	//    -- never by re-deriving createArtifact's id expression in Go.
	fileId, ok := fileIdFromSourceRef(stringField(artifact, "sourceConceptRef"))
	if !ok {
		return publishResult{}, refuse(reasonArtifactNotAFile,
			"artifact %q names backing row %q, which is not a v1:library:file", artifactId, stringField(artifact, "sourceConceptRef"))
	}
	file, err := i.queryOne(ctx, fmt.Sprintf("query libraryFileById(fileId: %s)", langparser.QuoteString(fileId)))
	if err != nil {
		return publishResult{}, fmt.Errorf("sitePublishFromArtifact: resolve file %q: %w", fileId, err)
	}
	if file == nil {
		return publishResult{}, refuse(reasonFileNotFound,
			"artifact %q names backing file %q, which is not visible to this caller", artifactId, fileId)
	}
	if boolField(file, "archived") {
		return publishResult{}, refuse(reasonFileArchived, "file %q is archived", fileId)
	}

	// 4. Zip by the row's own recorded type, before a byte is fetched.
	mimeType := normalizeMimeType(stringField(file, "mimeType"))
	if _, ok := sitePublishZipMimeTypes[mimeType]; !ok {
		return publishResult{}, refuse(reasonArtifactNotAZip,
			"file %q is %q; a site bundle must be a zip", fileId, stringField(file, "mimeType"))
	}

	// 5. The bytes.
	store, err := i.objectStore(ctx)
	if err != nil {
		return publishResult{}, refuse(reasonStorageNotReady,
			"object storage is not configured on this node: %v", err)
	}
	key := storageKey(stringField(file, "blobUrl"))
	if key == "" {
		return publishResult{}, refuse(reasonBundleUnreadable, "file %q records no storage location", fileId)
	}
	raw, err := store.reader.Get(ctx, key)
	if err != nil {
		return publishResult{}, refuse(reasonBundleUnreadable, "reading %q: %v", key, err)
	}

	bundle, totalBytes, err := expandSiteBundle(raw, siteKind)
	if err != nil {
		return publishResult{}, err
	}

	// 6. Upload the new version, then flip the row -- with artifactId
	//    riding the same write, so provenance and bundleRef can never
	//    disagree about which artifact produced the live bytes.
	publisher := edge.NewPublisher(store.writer, &artifactSiteStore{engine: i.engine, artifactId: artifactId})
	out, err := publisher.Publish(ctx, siteRowId, bundle)
	if err != nil {
		return publishResult{}, refuse(reasonPublishFailed, "%v", err)
	}

	return publishResult{
		SiteId:     siteRowId,
		ArtifactId: artifactId,
		FileId:     fileId,
		Version:    out.Version,
		BundleRef:  out.BundleRef,
		FileCount:  len(bundle),
		TotalBytes: totalBytes,
	}, nil
}

// ---------------------------------------------------------------------------
// The site write
// ---------------------------------------------------------------------------

// artifactSiteStore is the edge.SiteStore this capability hands the
// publisher: `updateSiteBundle` carrying artifactId alongside bundleRef.
//
// component/edge's own engineSiteStore cannot be reused, and the difference
// is not incidental. That one runs under a SYNTHETIC CLUSTER OWNER (it backs
// the CI route, whose authorization already happened at the service-account
// check) and passes no artifactId. Here the caller's own authority is the
// gate, so the write must run under the caller's ctx unchanged and be
// refused by guardRowAuthzWrite if the site is not theirs.
//
// artifactId sits in updateSiteBundle's accept{} block, so passing it writes
// it and omitting it inherits the stored value -- which is why this type
// always passes it: a publish that left it out would silently keep the
// PREVIOUS artifact's id on a row now serving different bytes.
type artifactSiteStore struct {
	engine     publishEngine
	artifactId string
}

func (s *artifactSiteStore) UpdateBundleRef(ctx context.Context, siteID, bundleRef string) error {
	q := fmt.Sprintf("mutation updateSiteBundle(siteId: %s, bundleRef: %s, artifactId: %s)",
		langparser.QuoteString(siteID), langparser.QuoteString(bundleRef), langparser.QuoteString(s.artifactId))
	if _, err := s.engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("mutation updateSiteBundle for %s: %w", siteID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Zip expansion + validation
// ---------------------------------------------------------------------------

// expandSiteBundle turns the zip bytes into the flat path->content map
// edge.Publisher.Publish wants, refusing anything the CI route would refuse.
// Returns the bundle and its total expanded size.
//
// Every limit is checked against the DECOMPRESSED size, and the per-file one
// twice: once against the entry's declared UncompressedSize64 before opening
// it, and once against the bytes actually read. The first is what stops a zip
// bomb from being materialized at all; the second is what stops a lying
// header from getting past the first.
func expandSiteBundle(raw []byte, siteKind string) (edge.Bundle, int, error) {
	if len(raw) == 0 {
		return nil, 0, refuse(reasonBundleEmpty, "the artifact holds no bytes")
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, 0, refuse(reasonBundleNotAZip, "%v", err)
	}

	bundle := edge.Bundle{}
	total := 0
	for _, f := range zr.File {
		name := path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if f.FileInfo().IsDir() || strings.HasSuffix(f.Name, "/") {
			continue
		}
		if skipZipEntry(name) {
			continue
		}
		// fs.ValidPath is the refusal, not a sanitising rewrite: the names
		// come out of a file a user uploaded and are joined onto a storage
		// prefix, and there is no legitimate bundle entry that needs to
		// escape its own bundle. It rejects "..", leading slashes and empty
		// segments outright -- the same check blobFS.Open applies on the way
		// back out (component/edge/blob.go).
		if !fs.ValidPath(name) || name == "." {
			return nil, 0, refuse(reasonBundlePathInvalid, "zip entry %q is not a valid bundle path", f.Name)
		}
		if len(bundle) >= sitePublishMaxFileCount {
			return nil, 0, refuse(reasonBundleTooManyFile,
				"the bundle holds more than %d files", sitePublishMaxFileCount)
		}
		if f.UncompressedSize64 > uint64(sitePublishMaxFileBytes) {
			return nil, 0, refuse(reasonBundleFileTooBig,
				"%q expands to %d bytes, over the %d-byte per-file limit", name, f.UncompressedSize64, sitePublishMaxFileBytes)
		}

		rc, err := f.Open()
		if err != nil {
			return nil, 0, refuse(reasonBundleNotAZip, "opening %q: %v", name, err)
		}
		// One byte past the cap, so a file that is exactly at the limit is
		// accepted and one byte over is detected rather than truncated.
		data, err := io.ReadAll(io.LimitReader(rc, int64(sitePublishMaxFileBytes)+1))
		closeErr := rc.Close()
		if err != nil {
			return nil, 0, refuse(reasonBundleUnreadable, "reading %q from the zip: %v", name, err)
		}
		if closeErr != nil {
			return nil, 0, refuse(reasonBundleUnreadable, "reading %q from the zip: %v", name, closeErr)
		}
		if len(data) > sitePublishMaxFileBytes {
			return nil, 0, refuse(reasonBundleFileTooBig,
				"%q is over the %d-byte per-file limit", name, sitePublishMaxFileBytes)
		}
		total += len(data)
		if total > sitePublishMaxTotalBytes {
			return nil, 0, refuse(reasonBundleTooLarge,
				"the expanded bundle is over the %d-byte total limit", sitePublishMaxTotalBytes)
		}
		bundle[name] = data
	}

	if len(bundle) == 0 {
		return nil, 0, refuse(reasonBundleEmpty, "the zip holds no publishable files")
	}
	if _, needsIndex := sitePublishIndexKinds[siteKind]; needsIndex {
		if _, ok := bundle["index.html"]; !ok {
			return nil, 0, refuse(reasonBundleNoIndex,
				"a %s bundle needs index.html at its ROOT; the zip's top level is %s",
				siteKind, describeTopLevel(bundle))
		}
	}
	return bundle, total, nil
}

// skipZipEntry drops the two kinds of entry a desktop archiver adds and
// nobody wants served: the __MACOSX resource-fork sidecar tree Finder writes
// beside every real file, and .DS_Store. Dropped rather than refused --
// a person zipping a build folder on a Mac has not done anything wrong, and
// refusing their zip over metadata they cannot see would be inexplicable.
func skipZipEntry(name string) bool {
	if name == "__MACOSX" || strings.HasPrefix(name, "__MACOSX/") {
		return true
	}
	return path.Base(name) == ".DS_Store"
}

// describeTopLevel names what the bundle's root actually holds, so the
// commonest mistake -- zipping the FOLDER rather than its contents, which
// puts everything under "dist/" -- is legible from the refusal alone.
func describeTopLevel(bundle edge.Bundle) string {
	seen := map[string]struct{}{}
	var names []string
	for name := range bundle {
		top := name
		if i := strings.IndexByte(name, '/'); i >= 0 {
			top = name[:i] + "/"
		}
		if _, dup := seen[top]; dup {
			continue
		}
		seen[top] = struct{}{}
		names = append(names, top)
		if len(names) == 4 {
			names = append(names, "...")
			break
		}
	}
	return strings.Join(names, ", ")
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// audit records the attempt on the append-only security log
// (v1:identity:auditEvent), success and refusal alike.
//
// BEST EFFORT, and that direction is deliberate. On the success path the
// bytes are already live and the row already flipped, so returning an audit
// failure to the caller would report a deploy that happened as one that did
// not. The write is logged at WARN instead, which is the same posture
// component/identity's own SlogAuditLogger takes when its DB sink fails.
//
// targetType is left unset: it is a CLOSED enum on createAuditEvent
// (user / session / identity / ...) with no `site` member, and widening a
// closed enum in dsl/identity for one caller is a bigger change than this
// row is worth. targetId carries the site id, and detail carries the rest.
func (i *SitePublishIntegration) audit(ctx context.Context, siteId, artifactId string, res publishResult, publishErr error) {
	if i == nil || i.engine == nil {
		return
	}

	// The RESOLVED row id when there is one, the caller's argument when the
	// call never got that far. A refusal has no resolved id by definition,
	// and recording the argument is what makes a run of failed attempts
	// legible afterwards.
	if strings.TrimSpace(res.SiteId) != "" {
		siteId = res.SiteId
	}

	detail := map[string]any{"artifactId": artifactId}
	outcome := "success"
	failureReason := ""
	if publishErr != nil {
		outcome = "blocked"
		failureReason = refusalReason(publishErr)
		if failureReason == "" {
			outcome = "failure"
			failureReason = "internal_error"
		}
	} else {
		detail["fileId"] = res.FileId
		detail["version"] = res.Version
		detail["bundleRef"] = res.BundleRef
		detail["fileCount"] = res.FileCount
		detail["totalBytes"] = res.TotalBytes
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		detailJSON = []byte("{}")
	}

	eventId, err := newSitePublishAuditId()
	if err != nil {
		i.logger.Warn("sitePublishFromArtifact: could not mint an audit event id",
			"component", "library", "error", err)
		return
	}

	var b strings.Builder
	b.WriteString("mutation createAuditEvent(eventId: ")
	b.WriteString(langparser.QuoteString(eventId))
	b.WriteString(", occurredAt: ")
	b.WriteString(langparser.QuoteString(time.Now().UTC().Format(time.RFC3339Nano)))
	b.WriteString(", category: \"configuration\", action: \"site_publish_from_artifact\"")
	if ac, ok := auth.AccessFromContext(ctx); ok {
		b.WriteString(", actorUserId: ")
		b.WriteString(langparser.QuoteString(ac.UserId))
		if ac.PrimaryEmail != "" {
			b.WriteString(", actorEmail: ")
			b.WriteString(langparser.QuoteString(ac.PrimaryEmail))
		}
		if ac.Role != "" {
			b.WriteString(", actorRole: ")
			b.WriteString(langparser.QuoteString(string(ac.Role)))
		}
		if ac.IdentityId != "" {
			b.WriteString(", actorIdentityId: ")
			b.WriteString(langparser.QuoteString(ac.IdentityId))
		}
	}
	b.WriteString(", targetId: ")
	b.WriteString(langparser.QuoteString(siteId))
	b.WriteString(", outcome: ")
	b.WriteString(langparser.QuoteString(outcome))
	if failureReason != "" {
		b.WriteString(", failureReason: ")
		b.WriteString(langparser.QuoteString(failureReason))
	}
	b.WriteString(", detail: ")
	b.Write(detailJSON)
	b.WriteString(")")

	if _, err := i.engine.Execute(ctx, b.String()); err != nil {
		i.logger.Warn("sitePublishFromArtifact: the audit event could not be written",
			"component", "library", "site", siteId, "artifact", artifactId,
			"outcome", outcome, "error", err)
	}
}

func newSitePublishAuditId() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "audit-" + hex.EncodeToString(buf[:]), nil
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// queryOne runs a named query and returns its first row, or nil for none.
// Zero rows is NOT an error here -- every read this capability issues is
// owner-scoped, so "no rows" is the ordinary answer for a row that is not
// the caller's, and each call site turns it into its own named refusal.
func (i *SitePublishIntegration) queryOne(ctx context.Context, query string) (map[string]any, error) {
	raw, err := i.engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	rows := extractRows(raw)
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// fileIdFromSourceRef pulls the file id out of a `v1:library:file:<id>`
// reference, reporting false for a ref naming any other concept.
func fileIdFromSourceRef(ref string) (string, bool) {
	const prefix = "v1:library:file:"
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(ref, prefix)
	if id == "" {
		return "", false
	}
	return id, true
}

// normalizeMimeType lowercases and drops any parameters ("application/zip;
// charset=binary"), so the closed set above is matched on the type alone.
func normalizeMimeType(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// storageKey normalizes v1:library:file.blobUrl into the container-relative
// key the blob reader wants. The field is documented as a STORAGE PATH
// ("library/{userId}/{fileId}/{name}") rather than a fetchable URL, so the
// only normalization is the two prefixes a caller might reasonably have
// stored -- a leading slash, or a blob:// scheme.
func storageKey(blobUrl string) string {
	key := strings.TrimSpace(blobUrl)
	key = strings.TrimPrefix(key, "blob://")
	return strings.TrimPrefix(key, "/")
}
