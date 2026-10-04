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
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
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

// Target is which of a site's two versions a publish writes (memql#5601).
//
// A site has a SERVING version (bundleRef), which every visitor is served, and
// may have a CANDIDATE version (candidateRef), which only a request carrying a
// preview grant is served (preview.go). Every publish route used to write the
// serving version, so the only way to put version N+1 beside a live N was to
// serve N+1 to the public and roll back. A target is what lets a publish say
// which field it writes -- and it chooses a FIELD, never an environment, which
// is why TestNoEnvironmentBranchingInEngineCode has nothing to say about it.
type Target string

const (
	// TargetServing writes bundleRef: the publish every route made before a
	// target existed, and what every visitor is served.
	TargetServing Target = "serving"
	// TargetCandidate writes candidateRef and leaves bundleRef -- and so the
	// public view -- exactly where it was.
	TargetCandidate Target = "candidate"
)

// ParseTarget reads a target off the wire. EMPTY IS THE SERVING VERSION,
// because that is what every caller meant before the field existed; anything
// unrecognised is REFUSED rather than read as serving, because the one
// direction this must never fail in is putting in front of shoppers a version
// somebody asked to keep away from them.
func ParseTarget(raw string) (Target, error) {
	switch t := Target(strings.TrimSpace(raw)); t {
	case "":
		return TargetServing, nil
	case TargetServing, TargetCandidate:
		return t, nil
	}
	return "", fmt.Errorf("edge: unknown publish target %q -- a publish writes the %q version or the %q one", raw, TargetServing, TargetCandidate)
}

// valid reports whether t is one of the two targets. The zero value is NOT:
// a Go caller names its target, and only the wire defaults one (ParseTarget).
func (t Target) valid() bool { return t == TargetServing || t == TargetCandidate }

// SiteStore is the one mutation the publisher performs: pointing one of a
// site's two versions at a bundle that is already uploaded.
//
// THREE STORES IMPLEMENT IT, ONE PER ROUTE, and they differ only in WHOSE
// authority the write runs under: the CI route's synthetic owner
// (engineSiteStore below), the Library deploy's caller
// (component/sitepublish), and the package pipeline's caller after its
// organization checks (component/packages). Which FIELD a target writes is not
// theirs to decide -- every one of them issues PointVersionStatement.
type SiteStore interface {
	PointVersion(ctx context.Context, siteID string, target Target, ref string) error
}

// Result is what a successful publish produced.
type Result struct {
	Version string
	// BundleRef is the uploaded version's reference (blob://sites/<id>/<v>/).
	// It is the site's bundleRef after a TargetServing publish and its
	// candidateRef after a TargetCandidate one -- Target says which.
	BundleRef string
	Target    Target
}

// PointVersionStatement renders the one write that points a site's target
// version at ref: updateSiteBundle for the serving version, setSiteCandidate
// for the candidate. artifactID is optional provenance, passed through to the
// mutation, which accepts it; empty omits it so the read-merge keeps what is
// stored.
//
// ONE FUNCTION FOR EVERY ROUTE, so "a candidate publish never writes
// bundleRef" is true in one place rather than three. Both mutations are
// update()s and need the site row to exist, which is what every route already
// requires.
func PointVersionStatement(siteID string, target Target, ref, artifactID string) (string, error) {
	var b strings.Builder
	switch target {
	case TargetServing:
		b.WriteString("mutation updateSiteBundle(siteId: ")
		b.WriteString(langparser.QuoteString(siteID))
		b.WriteString(", bundleRef: ")
	case TargetCandidate:
		// setSiteCandidate, not updateSiteBundle with a second field: its
		// guard (component/memql/platform_site_preview_guard.go) is what
		// refuses a candidate equal to the serving version and a candidate on
		// a storefront, and its capability is `preview` -- preparing a version
		// is one grant, putting it in front of the public is another.
		b.WriteString("mutation setSiteCandidate(siteId: ")
		b.WriteString(langparser.QuoteString(siteID))
		b.WriteString(", candidateRef: ")
	default:
		return "", fmt.Errorf("edge: unknown publish target %q", target)
	}
	b.WriteString(langparser.QuoteString(ref))
	if strings.TrimSpace(artifactID) != "" {
		b.WriteString(", artifactId: ")
		b.WriteString(langparser.QuoteString(artifactID))
	}
	b.WriteString(")")
	return b.String(), nil
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
// points the target version at it.
//
// THE ORDER IS THE FEATURE. A failure at any point during the upload leaves
// the row pointing at the previous version, whose bytes are untouched -- so a
// half-uploaded bundle is never reachable, and there is no cleanup path to get
// wrong. Overwriting a prefix in place would make a deploy non-atomic AND
// destroy the bytes rollback needs.
//
// The target is checked FIRST, before a byte moves: an unknown one is a caller
// bug, and uploading a bundle only to refuse the row write would leave bytes
// nobody asked to keep.
func (p *Publisher) Publish(ctx context.Context, siteID string, b Bundle, target Target) (Result, error) {
	if !target.valid() {
		return Result{}, fmt.Errorf("edge: refusing to publish to %s: unknown target %q", siteID, target)
	}
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
	if err := p.sites.PointVersion(ctx, siteID, target, ref); err != nil {
		// The bytes are uploaded and orphaned. That is the RIGHT failure:
		// storage is cheap, and the alternative -- flipping the row first --
		// serves a bundle that may not be fully there.
		return Result{}, fmt.Errorf("edge: pointing %s's %s version at %s: %w", siteID, target, ref, err)
	}

	return Result{Version: v, BundleRef: ref, Target: target}, nil
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

// systemEdgePublishActor is a synthetic cluster-owner identity for the one
// clusterOwner-tier write this file issues: PointVersionStatement's
// updateSiteBundle, or setSiteCandidate for a candidate publish
// (dsl/platform/mutations.memql), which engineSiteStore.PointVersion calls --
// the site concept carries @rowAuthz(clusterOwner) (dsl/platform/concepts.memql),
// same tier as the siteByHostname read edge.go issues.
//
// Deliberately a SEPARATE identity from edge.go's systemEdgeActor, not a
// reuse of it. That constant's own comment says "never used for anything
// else," scoped tightly to the one read SiteByHostname issues, and
// component/edge/edge.go is held by a concurrent task in this epic, so this
// file has no way to widen that comment to cover a second operation even if
// reuse were otherwise desirable. Same shape, same tier, same concept,
// different named operation -- kept distinct so each synthetic identity's
// audit trail (the "sub" claim an engine-side log or trigger would see)
// names exactly the one thing it does. See edge.go's file-level note for
// the fuller reasoning this mirrors.
//
// Safe to stamp unconditionally on every call: by the time
// engineSiteStore.PointVersion runs, the calling HTTP handler
// (component/server.SiteBundleHandler) has already verified the request
// carries a class="service_account" credential. That check is the actual
// authorization gate; this identity exists only to carry the clusterOwner
// authority the concept's row-authz tier requires to perform a write the
// gate already authorized -- the same division of labour SiteByHostname's
// synthetic actor has relative to whatever authorized the resolver's caller.
const systemEdgePublishActor = "system:edge-publish"

// engineSiteStore is the CI route's SiteStore: it points a site's serving or
// candidate version at a new bundle by issuing PointVersionStatement through
// the engine.
type engineSiteStore struct {
	engine Engine
}

// NewEngineSiteStore wraps engine as a SiteStore. The concrete counterpart to
// the fakeSiteStore the tests use, mirroring NewEngineExecutor's shape in
// edge.go (same Engine interface, same "one narrow named call" scope).
func NewEngineSiteStore(engine Engine) SiteStore {
	return &engineSiteStore{engine: engine}
}

// PointVersion issues PointVersionStatement under a synthetic cluster-owner
// actor -- see systemEdgePublishActor's comment for why a second synthetic
// identity exists here rather than reusing edge.go's.
//
// The invocation keyword is "mutation", the word the .memql constructs are
// declared with too (`mutation site updateSiteBundle { ... }`, D13;
// component/campaigns/store.go's call() helper uses the same keyword). The
// call does not repeat the bound concept name -- that binding is resolved
// from the construct's own signature at load time, the same way edge.go's
// "query siteByHostname(...)" names no concept either.
func (s *engineSiteStore) PointVersion(ctx context.Context, siteID string, target Target, ref string) error {
	q, err := PointVersionStatement(siteID, target, ref, "")
	if err != nil {
		return err
	}
	if _, err := s.engine.Execute(publishActorContext(ctx), q); err != nil {
		return fmt.Errorf("edge: pointing the %s version of %s: %w", target, siteID, err)
	}
	return nil
}

// publishActorContext stamps systemEdgePublishActor onto ctx using the same
// three-surface pattern edge.go's systemActorContext documents (claims,
// TokenInfo, AccessContext) and for the same reason: createdBy and
// actor.userId read different surfaces (memql#2989), so a synthetic actor
// that sets only one of them silently resolves to nobody on whichever the
// mutation happens to read. Role is RoleOwner specifically because
// AccessContext.IsClusterOwner() reads Role == RoleOwner, which is what the
// site concept's @rowAuthz(clusterOwner) tier checks.
func publishActorContext(ctx context.Context) context.Context {
	claims := map[string]any{"sub": systemEdgePublishActor, "role": "owner"}
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: systemEdgePublishActor,
		Role:   auth.RoleOwner,
	})
}
