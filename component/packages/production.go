package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// production.go is where the seams meet the cluster. Everything here is thin
// on purpose: the state machine holds the rules and these hold the plumbing,
// which is what lets the D6 ordering law be asserted end to end with no cluster
// at all.

// Site lifecycle statuses, mirroring the enum on v1:platform:site. Duplicated
// from component/memql's guard rather than shared, because that package cannot
// import this one and this one has no business importing the executor.
const (
	siteStatusDraft    = "draft"
	siteStatusLive     = "live"
	siteStatusDisabled = "disabled"
	siteStatusArchived = "archived"
)

// ---------------------------------------------------------------------------
// fetch
// ---------------------------------------------------------------------------

func newProductionFetcher(s *store, logger *slog.Logger, gh *githubapp.Client) Fetcher {
	blobs := &blobReader{}
	return &githubFetcher{
		http: &http.Client{Timeout: 5 * time.Minute},
		// The same resolver the D11 poll uses (Deps.Credentials), wired here
		// as well so the fetcher is complete on its own: a fetch resolves its
		// credential under the package owner's actor, never through a
		// cluster-wide secret (epic memql#4885).
		credentials: s.resolveCredential,
		// The SAME client the store refreshes user tokens through, so a node
		// mints installation tokens and renews grants against one app and one
		// token cache (epic memql#4912). Two clients would be two caches, and
		// two caches means twice the mints against the same rate limit for no
		// reason a reader could find.
		github: gh,
		artifactBytes: func(ctx context.Context, artifactId string) ([]byte, string, error) {
			return s.artifactBytes(ctx, artifactId, blobs.read)
		},
	}
}

// blobRead reads one object out of the cluster's object storage by its
// container-relative key. The seam artifactBytes reads bytes through, so a
// test hands it a map and production hands it the blob store.
type blobRead func(ctx context.Context, key string) ([]byte, error)

// blobReader is the production blobRead: the same lazy resolve blobStager and
// enginePublisher do, for the same reason -- the plug-in factory runs on every
// node type, and building an Azure client eagerly would fail the factory on a
// cluster that hosts no packages at all.
type blobReader struct {
	uploader  *azureblob.AzureBlobUploader
	container string
}

func (b *blobReader) read(ctx context.Context, key string) ([]byte, error) {
	if b.uploader == nil {
		container := azureblob.ContainerFromEnv()
		if strings.TrimSpace(container) == "" {
			return nil, refuse(CodeSourceUnreadable,
				"this cluster has no object storage configured (MEMQL_AZURE_BLOB_CONTAINER), so a Library zip cannot be read")
		}
		up, err := azureblob.New(ctx)
		if err != nil {
			return nil, err
		}
		b.uploader, b.container = up, container
	}
	return b.uploader.DownloadWithLimit(ctx, b.container, key, DefaultLimits().MaxSourceBytes)
}

// zipMimeTypes is the closed set of types a Library file may carry and still
// be a zip. sitePublishFromArtifact's own set, copied rather than shared for
// the reason rows.go records about memqlRows: the two packages sit at
// different module tiers, and five strings are not worth a dependency.
var zipMimeTypes = map[string]struct{}{
	"application/zip":              {},
	"application/x-zip":            {},
	"application/x-zip-compressed": {},
	"application/zip-compressed":   {},
	"multipart/x-zip":              {},
}

// artifactBytes resolves a Library zip artifact to its bytes, the way
// sitePublishFromArtifact does and under the same rule: the index row through
// libraryArtifactById, the backing file through libraryFileById -- both
// owner-scoped named queries run under the CALLER's actor, so a caller who
// does not own the artifact resolves zero rows and is refused by name -- and
// the bytes from object storage at the file row's own blobUrl.
//
// It used to call `libraryArtifactBytes`, a builtin nothing in the tree
// declares, so a zip source could never be read on a real cluster; every fake
// answered it and nothing parsed it. The two reads here are driven through
// the real front end by render_parse_test.go.
func (s *store) artifactBytes(ctx context.Context, artifactId string, read blobRead) ([]byte, string, error) {
	artifact, err := s.queryOne(ctx, fmt.Sprintf("query libraryArtifactById(artifactId: %s)", langparser.QuoteString(artifactId)))
	if err != nil {
		return nil, "", fmt.Errorf("libraryArtifactById: %w", err)
	}
	if artifact == nil {
		// Zero rows means "no artifact by this id that YOU may read" -- one
		// answer to two questions on purpose, so this is not an existence
		// oracle over other people's files.
		return nil, "", refuse(CodeSourceUnreadable, "no artifact %q is readable by this caller", artifactId)
	}
	if rowBool(artifact, "archived") {
		return nil, "", refuse(CodeSourceUnreadable, "artifact %q is archived; restore it before deploying from it", artifactId)
	}
	if kind := rowString(artifact, "kind"); kind != "file" {
		return nil, "", refuse(CodeSourceUnreadable, "artifact %q is kind %q; only a file artifact carries bytes to deploy", artifactId, kind)
	}
	fileId, ok := fileIdFromSourceRef(rowString(artifact, "sourceConceptRef"))
	if !ok {
		return nil, "", refuse(CodeSourceUnreadable,
			"artifact %q names backing row %q, which is not a v1:library:file", artifactId, rowString(artifact, "sourceConceptRef"))
	}

	file, err := s.queryOne(ctx, fmt.Sprintf("query libraryFileById(fileId: %s)", langparser.QuoteString(fileId)))
	if err != nil {
		return nil, "", fmt.Errorf("libraryFileById: %w", err)
	}
	if file == nil {
		return nil, "", refuse(CodeSourceUnreadable,
			"artifact %q names backing file %q, which is not visible to this caller", artifactId, fileId)
	}
	if rowBool(file, "archived") {
		return nil, "", refuse(CodeSourceUnreadable, "file %q is archived; restore it before deploying from it", fileId)
	}
	// Zip by the row's own recorded type, BEFORE a byte is fetched: OpenZip
	// would refuse a text file anyway, after reading it in full.
	mime := normalizeMimeType(rowString(file, "mimeType"))
	if _, ok := zipMimeTypes[mime]; !ok {
		return nil, "", refuse(CodeSourceUnreadable,
			"file %q is %q; a package source must be a zip of the tree", fileId, rowString(file, "mimeType"))
	}
	key := storageKey(rowString(file, "blobUrl"))
	if key == "" {
		return nil, "", refuse(CodeSourceUnreadable, "file %q records no storage location", fileId)
	}
	if read == nil {
		return nil, "", refuse(CodeSourceUnreadable, "this node cannot read object storage, so a Library zip cannot be deployed from here")
	}
	raw, rerr := read(ctx, key)
	if rerr != nil {
		if RefusalCode(rerr) != "" {
			return nil, "", rerr
		}
		return nil, "", refuse(CodeSourceUnreadable, "reading %q from object storage: %v", key, rerr)
	}
	return raw, mime, nil
}

// fileIdFromSourceRef reads the bare file id out of an artifact's
// sourceConceptRef. The field is an outgoing @relationship, so the engine
// stores it canonical (v1:library:file:<id>); a bare id is accepted too, for
// a row read through a projection that bare-ified it. A reference naming any
// OTHER concept is refused: the Library's index row can point at a note or a
// document, and neither carries bytes to deploy.
func fileIdFromSourceRef(ref string) (string, bool) {
	const prefix = "v1:library:file:"
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, prefix) {
		id := strings.TrimPrefix(ref, prefix)
		return id, id != ""
	}
	if ref != "" && !strings.Contains(ref, ":") {
		return ref, true
	}
	return "", false
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
// key the blob reader wants: the field is a STORAGE PATH, and the only
// normalization is the two prefixes a writer might have stored.
func storageKey(blobUrl string) string {
	key := strings.TrimSpace(blobUrl)
	key = strings.TrimPrefix(key, "blob://")
	return strings.TrimPrefix(key, "/")
}

// ---------------------------------------------------------------------------
// stage
// ---------------------------------------------------------------------------

// blobStager writes content-addressed DSL trees into the cluster's own object
// storage and rewrites the active-set pointer.
type blobStager struct {
	uploader  *azureblob.AzureBlobUploader
	container string
}

func newBlobStager() Stager { return &blobStager{} }

func (b *blobStager) resolve(ctx context.Context) error {
	if b.uploader != nil {
		return nil
	}
	container := azureblob.ContainerFromEnv()
	if strings.TrimSpace(container) == "" {
		return refuse(CodeSourceUnreadable,
			"this cluster has no object storage configured (MEMQL_AZURE_BLOB_CONTAINER), so a package's DSL cannot be staged")
	}
	up, err := azureblob.New(ctx)
	if err != nil {
		return err
	}
	b.uploader, b.container = up, container
	return nil
}

func (b *blobStager) StageDomain(ctx context.Context, domain string, tree fs.FS) (string, error) {
	if err := b.resolve(ctx); err != nil {
		return "", err
	}
	hash, files, err := hashTree(tree)
	if err != nil {
		return "", err
	}
	prefix := fmt.Sprintf("packages/%s/%s/", domain, hash)
	for name, data := range files {
		if _, err := b.uploader.Upload(ctx, b.container, prefix+name, data, "text/plain"); err != nil {
			return "", err
		}
	}
	return prefix, nil
}

func (b *blobStager) ReadActiveSet(ctx context.Context) (map[string]string, error) {
	if err := b.resolve(ctx); err != nil {
		return nil, err
	}
	raw, err := b.uploader.Download(ctx, b.container, ActiveSetPath)
	if err != nil {
		// A cluster that has never staged a package has no pointer, and that
		// is the ordinary first-deploy state rather than a fault. Returning
		// empty is what makes the first deploy identical to every later one.
		return map[string]string{}, nil
	}
	set := map[string]string{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &set); err != nil {
			return nil, fmt.Errorf("packages: the active-set pointer at %s is not readable: %w", ActiveSetPath, err)
		}
	}
	return set, nil
}

func (b *blobStager) WriteActiveSet(ctx context.Context, set map[string]string) error {
	if err := b.resolve(ctx); err != nil {
		return err
	}
	raw, err := marshalActiveSet(set)
	if err != nil {
		return err
	}
	_, err = b.uploader.Upload(ctx, b.container, ActiveSetPath, raw, "application/json")
	return err
}

// ---------------------------------------------------------------------------
// roll
// ---------------------------------------------------------------------------

// RollTargetsEnv names the workloads a DSL roll restarts.
//
// A LIST rather than "every Deployment", because a roll is a restart and the
// blast radius must be a decision somebody made rather than whatever happens
// to be running. Unset means the roll refuses instead of guessing: restarting
// the wrong set is worse than not restarting.
const RollTargetsEnv = "MEMQL_PACKAGES_ROLL_TARGETS"

type deployControlRoller struct {
	logger *slog.Logger
	// restart is the effect seam. Production drives the cluster's own deploy
	// control surface; a test substitutes a recorder.
	restart func(ctx context.Context, workload string) error
}

// newDeployControlRoller wires the roll to the cluster's own API server.
//
// NOT a capability script, and that is the whole point (epic memql#4794). The
// engine's runtime image is DISTROLESS -- no shell, no kubectl, and the
// scripts/ tree is not even in the build context -- so a script here would
// pass every local test and never run on a deployed cluster. What `kubectl
// rollout restart` actually does is one strategic-merge PATCH against the pod
// template, which component/deploycontrol now exposes as RestartDeployment.
func newDeployControlRoller(logger *slog.Logger) Roller {
	return &deployControlRoller{
		logger: logger,
		restart: func(ctx context.Context, workload string) error {
			ns := deploycontrol.NamespaceFromEnv()
			if ns == "" {
				return refuse(CodeSourceUnreadable,
					"this node cannot tell which namespace it is in, so it cannot roll %s", workload)
			}
			return deploycontrol.RestartDeployment(ctx, ns, workload)
		},
	}
}

func (r *deployControlRoller) Roll(ctx context.Context, reason string) error {
	targets := rollTargets()
	if len(targets) == 0 {
		return refuse(CodeSourceUnreadable,
			"this cluster names no workloads to roll (%s is unset), so staged DSL cannot be made live. A roll restarts pods, so the set it restarts is a decision rather than a default.",
			RollTargetsEnv)
	}
	if r.restart == nil {
		return refuse(CodeSourceUnreadable,
			"this node has no rollout surface, so the staged DSL cannot be made live from here")
	}
	for _, t := range targets {
		if err := r.restart(ctx, t); err != nil {
			return fmt.Errorf("packages: rolling %s: %w", t, err)
		}
		r.logger.Info("packages: rolled workload for a package deploy",
			"component", "packages.roll", "workload", t, "reason", reason)
	}
	return nil
}

func rollTargets() []string {
	raw := strings.TrimSpace(envValue(RollTargetsEnv))
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// stores
// ---------------------------------------------------------------------------

// resolveStore is the production Deps.Stores: the myshopify.com domain a
// manifest names, answered as the bare v1:shopify:store row id (epic
// memql#5530, issue memql#5540).
//
// UNDER THE CALLER'S OWN ACTOR, with nothing borrowed and nothing stamped, and
// that is the whole authorization shape of the feature. storeByDomain is a
// cluster-owner-tier read, so a caller who may not read stores resolves ZERO
// ROWS and the publish is refused by name -- the same answer
// updateSiteStoreBinding's Go guard gives. Borrowing the package OWNER's
// authority here (the pattern resolveCredential uses, for a credential the
// owner holds) would let anyone who can deploy a package point a storefront at
// any merchant on the cluster, which is the one thing this seam must not do.
func (s *store) resolveStore(ctx context.Context, domain string) (string, error) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return "", nil
	}
	row, err := s.queryOne(ctx, fmt.Sprintf("query storeByDomain(domain: %s)", langparser.QuoteString(domain)))
	if err != nil {
		return "", err
	}
	// ZERO ROWS IS "", NOT AN ERROR. The read cannot tell a store that does
	// not exist from one this caller may not read, and that is the design:
	// both are the same refusal, and answering which would tell somebody
	// outside the tier what is on the cluster.
	return rowString(row, "id"), nil
}

// ---------------------------------------------------------------------------
// publish
// ---------------------------------------------------------------------------

type enginePublisher struct {
	engine    Engine
	store     *store
	logger    *slog.Logger
	uploader  *azureblob.AzureBlobUploader
	container string
	blobs     edge.BlobWriter
}

func newEnginePublisher(engine Engine, s *store, logger *slog.Logger) SitePublisher {
	return &enginePublisher{engine: engine, store: s, logger: logger}
}

func (p *enginePublisher) resolve(ctx context.Context) error {
	if p.blobs != nil {
		return nil
	}
	if p.uploader != nil {
		p.blobs = edge.NewAzureBlobWriter(p.uploader, p.container)
		return nil
	}
	container := azureblob.ContainerFromEnv()
	if strings.TrimSpace(container) == "" {
		return refuse(CodeSourceUnreadable,
			"this cluster has no object storage configured (MEMQL_AZURE_BLOB_CONTAINER), so a bundle cannot be published")
	}
	up, err := azureblob.New(ctx)
	if err != nil {
		return err
	}
	p.uploader, p.container = up, container
	p.blobs = edge.NewAzureBlobWriter(up, container)
	return nil
}

// EnsureSite finds this deployable's existing site, or creates one in DRAFT.
//
// DRAFT, deliberately: a first deploy that went straight to live would put a
// stranger's code on a hostname the moment it built, with nobody having looked
// at it. The person publishes it by enabling it, which is one click and a
// decision.
func (p *enginePublisher) EnsureSite(ctx context.Context, req EnsureSiteRequest) (string, string, bool, error) {
	sites, err := p.store.sitesForPackage(ctx, req.PackageId)
	if err != nil {
		return "", "", false, err
	}
	for _, row := range sites {
		if rowString(row, "packageDeployableName") == req.DeployableName {
			if err := requireSiteRowAction(ctx, p.store, row, "deploy"); err != nil {
				return "", "", false, err
			}
			return rowString(row, "id"), rowString(row, "hostname"), false, nil
		}
	}

	if strings.TrimSpace(req.AccountId) == "" {
		return "", "", false, refuse("organization_required", "a new deployable needs its owning organization before publication")
	}
	if err := p.requireAccountAction(ctx, req.AccountId, "deploy"); err != nil {
		return "", "", false, err
	}
	siteId := newRowId("v1:platform:site")
	var b strings.Builder
	b.WriteString("mutation createSite(siteId: ")
	b.WriteString(langparser.QuoteString(siteId))
	b.WriteString(", hostname: ")
	b.WriteString(langparser.QuoteString(req.Hostname))
	if req.AccountId != "" {
		b.WriteString(", accountId: ")
		b.WriteString(langparser.QuoteString(req.AccountId))
	}
	b.WriteString(", kind: ")
	b.WriteString(langparser.QuoteString(req.Kind))
	// A site row must carry a bundleRef, and there is nothing to point at
	// until the first publish lands. The draft status is what makes that
	// harmless: a draft resolves for nobody.
	b.WriteString(", bundleRef: ")
	b.WriteString(langparser.QuoteString(""))
	b.WriteString(", status: ")
	b.WriteString(langparser.QuoteString(siteStatusDraft))
	// THE BINDING IS A REFERENCE, written as {storeId} (epic memql#5530).
	// json.Marshal of a map[string]string handles the quoting, which is the
	// same reason it was used for the old two-field shape.
	if req.StoreId != "" {
		raw, _ := json.Marshal(map[string]string{"storeId": req.StoreId})
		b.WriteString(", binding: ")
		b.Write(raw)
	}
	// OMITTED WHEN EMPTY, never written as "". createSite accepts this field
	// rather than stamping it, so an omitted argument is dropped from the
	// payload and the row carries no value -- which is the state that means
	// "the kind decides". Passing an explicit empty string would write one,
	// and the enum would refuse it.
	if ResolutionTailIsSet(req.ResolutionTail) {
		b.WriteString(", resolutionTail: ")
		b.WriteString(langparser.QuoteString(req.ResolutionTail))
	}
	b.WriteString(")")

	// createSite runs under the CALLER's actor, never stamped: ownerUserId is
	// stamped from actor.userId inside the mutation, which is what preserves
	// memql#4344 per-user site ownership for an SPAs-only deploy.
	if _, err := p.engine.Execute(ctx, b.String()); err != nil {
		return "", "", false, err
	}
	return siteId, req.Hostname, true, nil
}

func (p *enginePublisher) PublishBundle(ctx context.Context, siteId string, bundle edge.Bundle) (PublishResult, error) {
	if err := p.requireSiteAction(ctx, siteId, "deploy"); err != nil {
		return PublishResult{}, err
	}
	if err := p.resolve(ctx); err != nil {
		return PublishResult{}, err
	}
	publisher := edge.NewPublisher(
		p.blobs,
		packageSiteWriter{publisher: p},
	)
	res, err := publisher.Publish(ctx, siteId, bundle)
	if err != nil {
		return PublishResult{}, err
	}
	return PublishResult{SiteId: siteId, BundleRef: res.BundleRef, Version: res.Version}, nil
}

func requireSiteRowAction(ctx context.Context, s *store, row map[string]any, action string) error {
	if row == nil {
		return refuse(CodeSourceUnreadable, "the target deployable is not readable by this caller")
	}
	authority, ok := s.engine.(interface {
		OrganizationCapable(context.Context, string, string, string) bool
	})
	if !ok || !authority.OrganizationCapable(ctx, rowString(row, "accountId"), auth.VerbUpdate, auth.ResourceData) {
		return refuse("organization_forbidden", "the deployable's organization does not permit changing its data")
	}
	return (&enginePublisher{engine: s.engine, store: s}).requireAccountAction(ctx, rowString(row, "accountId"), action)
}

// A package and its deployable may serve different organizations. Authorize
// both targets; a source's permission never grants its client's permission.
func (p *enginePublisher) requireAccountAction(ctx context.Context, accountID, action string) error {
	authority, ok := p.engine.(interface {
		OrganizationCapable(context.Context, string, string, string) bool
	})
	if !ok || !authority.OrganizationCapable(ctx, accountID, auth.VerbExecute, "app:deployables/"+action) {
		return refuse("organization_forbidden", "the deployable's organization does not permit this %s action", action)
	}
	return nil
}

func (p *enginePublisher) requireSiteAction(ctx context.Context, siteID, action string) error {
	row, err := p.store.siteById(ctx, siteID)
	if err != nil {
		return err
	}
	if row == nil {
		return refuse(CodeSourceUnreadable, "no deployable %q is readable by this caller", siteID)
	}
	return requireSiteRowAction(ctx, p.store, row, action)
}

// The package pipeline has a real caller. Preserve that actor through the
// final write and recheck authority after uploads, rather than using the edge
// publisher's synthetic service-account writer.
type packageSiteWriter struct{ publisher *enginePublisher }

func (s packageSiteWriter) UpdateBundleRef(ctx context.Context, siteID, bundleRef string) error {
	if err := s.publisher.requireSiteAction(ctx, siteID, "deploy"); err != nil {
		return err
	}
	_, err := s.publisher.engine.Execute(ctx, fmt.Sprintf("mutation updateSiteBundle(siteId: %s, bundleRef: %s)", langparser.QuoteString(siteID), langparser.QuoteString(bundleRef)))
	return err
}

// BindSiteToStore re-points an existing site at the store its manifest names.
//
// Under the CALLER's actor, unstamped, exactly as createSite and the two
// placement writes are: updateSiteStoreBinding carries a capability gate and a
// Go guard that refuses a store the caller cannot read
// (component/memql/platform_site_binding_guard.go), and a deploy gains no
// bypass of either. A person who may deploy this package but may not bind a
// storefront is refused here, by that guard, in the same sentence the Store
// panel would have given them.
func (p *enginePublisher) BindSiteToStore(ctx context.Context, siteId, storeId string) error {
	_, err := p.engine.Execute(ctx, fmt.Sprintf("mutation updateSiteStoreBinding(siteId: %s, storeId: %s)",
		langparser.QuoteString(siteId), langparser.QuoteString(storeId)))
	return err
}

// RepointSite is the rollback write: updateSiteBundle pointed back at a
// version whose bytes are still there.
func (p *enginePublisher) RepointSite(ctx context.Context, siteId, bundleRef string) error {
	if err := p.requireSiteAction(ctx, siteId, "publish"); err != nil {
		return err
	}
	_, err := p.engine.Execute(ctx, fmt.Sprintf("mutation updateSiteBundle(siteId: %s, bundleRef: %s)",
		langparser.QuoteString(siteId), langparser.QuoteString(bundleRef)))
	return err
}

// StoreSnapshot lands a fetched archive as a content-addressed Library
// artifact (D8) through the engine's own Library capability.
func (p *enginePublisher) StoreSnapshot(ctx context.Context, packageId, version string, raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if err := p.resolve(ctx); err != nil {
		return "", err
	}
	key := fmt.Sprintf("packages/snapshots/%s/%s.tar.gz", shortId(packageId), version)
	if _, err := p.uploader.Upload(ctx, p.container, key, raw, "application/gzip"); err != nil {
		return "", err
	}
	return "blob://" + key, nil
}

// ReadSnapshot fetches a stored snapshot back.
//
// It accepts only the `blob://<object>` form StoreSnapshot mints. A ref in any
// other shape -- a Library artifact id, a URL somebody pasted -- is refused by
// name rather than best-effort resolved: a retry that silently fetched
// something else would deploy bytes nobody chose.
func (p *enginePublisher) ReadSnapshot(ctx context.Context, ref string) ([]byte, error) {
	object, ok := strings.CutPrefix(strings.TrimSpace(ref), "blob://")
	if !ok || object == "" {
		return nil, refuse(CodeSnapshotUnavailable,
			"the earlier run's snapshot reference %q is not one this cluster stores", ref)
	}
	if err := p.resolve(ctx); err != nil {
		return nil, err
	}
	// A repository can pass the package source limit while exceeding the
	// attachment reader's smaller default. Confirm and retry must read the
	// complete archive that analysis stored, under the same source budget.
	raw, err := p.uploader.DownloadWithLimit(ctx, p.container, object, DefaultLimits().MaxSourceBytes)
	if err != nil {
		return nil, refuse(CodeSnapshotUnavailable,
			"the earlier run's snapshot could not be read back: %v", err)
	}
	return raw, nil
}

func shortId(canonical string) string {
	if i := strings.LastIndexByte(canonical, ':'); i >= 0 && i+1 < len(canonical) {
		return canonical[i+1:]
	}
	return canonical
}

// engineAdapter bridges this package's narrow Engine (which returns a typed
// *memql.ExecuteResult) to edge's (which returns `any`). One method, no
// behaviour -- the two interfaces differ only in how much they say about the
// value, and edge's site store only needs it to be non-nil.
type engineAdapter struct{ engine Engine }

func (a engineAdapter) Execute(ctx context.Context, query string) (any, error) {
	return a.engine.Execute(ctx, query)
}

// envValue reads an environment variable. Named so the roll targets have one
// read site a grep finds.
func envValue(key string) string { return os.Getenv(key) }
