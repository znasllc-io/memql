package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/core/num"
)

// Engine is the ONLY engine surface the pipeline needs -- one method, the same
// narrow seam component/sitepublish and every other Go component in this tree
// uses. Narrow on purpose: a test fakes the named calls it cares about and
// nothing else.
type Engine interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

// store is the pipeline's graph access.
//
// EVERY WRITE HERE RUNS UNDER STAMPED INTERNAL ORIGIN, and every read does not.
// That split is the whole authorization shape:
//
//   - The @serverOnly pipeline mutations are unreachable from the wire and are
//     stamped internal here, in this package, which is what the
//     ContextWithInternalOrigin allowlist admits (component/auth) -- the stamp
//     is applied INLINE, as the argument to the one Execute that needs it, so
//     it dies at that call and cannot flow onward into a frame running
//     caller-supplied text. That is the memql#2879 rule.
//   - Reads run under whatever actor the caller already has. They must: the
//     concepts declare a composite owner tier, so a read under a blank actor
//     returns ZERO ROWS rather than an error -- silently, which is how a
//     pipeline reading its own package row under no actor would conclude the
//     package does not exist.
//
// Two exceptions, both in the source-credentials section at the bottom and
// both stated there rather than here: sourceCredentialSealedById is a STAMPED
// READ (the query is @serverOnly because it returns ciphertext, and the stamp
// admits the construct without widening the rows -- the actor still decides
// those), and revokeSourceCredential is an UNSTAMPED WRITE (an ordinary owned
// mutation the write guard decides for the caller; stamping it would hand the
// guard its internal-origin escape and let anyone revoke anything).
// internal_origin_test.go asserts all four lists.
type store struct {
	engine Engine
	// logger is for the one thing the store does on its own account -- the
	// best-effort heartbeat behind resolveCredential. Nil means slog.Default.
	logger *slog.Logger
	// github is the cluster's GitHub App client (epic memql#4912). NIL MEANS
	// THIS NODE HAS NO APP WIRED, and a github_app grant is then refused by
	// name (github_app_not_configured) rather than falling through to a token
	// this store cannot renew -- the same shape as a nil credential resolver
	// refusing rather than fetching anonymously. A token credential never
	// touches it.
	github *githubapp.Client
}

func (s *store) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// ---------------------------------------------------------------------------
// Reads (caller's actor)
// ---------------------------------------------------------------------------

func (s *store) queryOne(ctx context.Context, query string) (map[string]any, error) {
	res, err := s.engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	rows := memqlRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func (s *store) queryAll(ctx context.Context, query string) ([]map[string]any, error) {
	res, err := s.engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	return memqlRows(res), nil
}

func (s *store) packageById(ctx context.Context, id string) (map[string]any, error) {
	return s.queryOne(ctx, fmt.Sprintf("query packageById(packageId: %s)", langparser.QuoteString(id)))
}

func (s *store) deploymentById(ctx context.Context, id string) (map[string]any, error) {
	return s.queryOne(ctx, fmt.Sprintf("query packageDeploymentById(deploymentId: %s)", langparser.QuoteString(id)))
}

func (s *store) sitesForPackage(ctx context.Context, packageId string) ([]map[string]any, error) {
	return s.queryAll(ctx, fmt.Sprintf("query sitesForPackage(packageId: %s)", langparser.QuoteString(packageId)))
}

func (s *store) siteById(ctx context.Context, id string) (map[string]any, error) {
	return s.queryOne(ctx, fmt.Sprintf("query siteById(siteId: %s)", langparser.QuoteString(id)))
}

func (s *store) packagesByRepoUrl(ctx context.Context, url string) ([]map[string]any, error) {
	return s.queryAll(ctx, fmt.Sprintf("query packagesByRepoUrl(repoUrl: %s)", langparser.QuoteString(url)))
}

func (s *store) packagesTrackingRepos(ctx context.Context) ([]map[string]any, error) {
	return s.queryAll(ctx, "query packagesTrackingRepos()")
}

// lastSucceededDeployment is the newest run of this package that actually
// finished, or nil.
//
// Folded from the package's own timeline rather than asked for by a query of
// its own: packageDeployments is already newest-first and bounded at fifty,
// which is more than enough to find the last success -- and a fifty-run gap
// with no success in it is a package whose auto-deploy should be parking
// anyway.
func (s *store) lastSucceededDeployment(ctx context.Context, packageId string) (map[string]any, error) {
	rows, err := s.deploymentsForPackage(ctx, packageId)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if rowString(row, "status") == "succeeded" {
			return row, nil
		}
	}
	return nil, nil
}

// liveDeploymentsForPackage is every run of this package that has not reached
// a terminal status.
func (s *store) liveDeploymentsForPackage(ctx context.Context, packageId string) ([]map[string]any, error) {
	rows, err := s.deploymentsForPackage(ctx, packageId)
	if err != nil {
		return nil, err
	}
	var live []map[string]any
	for _, row := range rows {
		if !IsTerminal(rowString(row, "status")) {
			live = append(live, row)
		}
	}
	return live, nil
}

func (s *store) deploymentsForPackage(ctx context.Context, packageId string) ([]map[string]any, error) {
	return s.queryAll(ctx, fmt.Sprintf("query packageDeployments(packageId: %s)", langparser.QuoteString(packageId)))
}

// deploymentsInFlight is the abandoned sweep's read: every run at a
// non-terminal status the CALLER may see.
//
// Unstamped like every other read here, which is what makes the actor the
// sweep runs under decide the scope -- the maintenance actor is a cluster
// owner and sees the cluster, and any other caller sees their own. A stamped
// internal origin would widen it silently, which is the failure nobody
// notices.
func (s *store) deploymentsInFlight(ctx context.Context) ([]map[string]any, error) {
	return s.queryAll(ctx, "query packageDeploymentsInFlight()")
}

// ---------------------------------------------------------------------------
// Writes (stamped internal origin)
// ---------------------------------------------------------------------------

// executeInternal runs one @serverOnly construct under a context this function
// constructs. The stamp is applied here and nowhere else in the package --
// INLINE, as the argument to the one Execute that needs it, so it dies at that
// call (memql#2879; TestTheStampNeverEscapesItsCall counts this site and
// requires exactly one). Every stamped statement funnels through here: the
// pipeline's writes through writeInternal below, and the one stamped read,
// sourceCredentialSealedById.
func (s *store) executeInternal(ctx context.Context, query string) ([]map[string]any, error) {
	res, err := s.engine.Execute(auth.ContextWithInternalOrigin(ctx), query)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", firstToken(query), err)
	}
	return memqlRows(res), nil
}

// writeInternal runs one @serverOnly mutation. A wrapper over executeInternal
// rather than a second stamp, which is what keeps the call-site count at one.
func (s *store) writeInternal(ctx context.Context, query string) error {
	_, err := s.executeInternal(ctx, query)
	return err
}

// openDeployment is the ONE write in this package that runs under a BORROWED
// actor, and the reason is the row-authz declaration.
//
// v1:platform:packageDeployment declares @rowAuthz(owner="ownerUserId"), and a
// declared owner tier over a CALLER-SUPPLIED field records a guarantee nothing
// provides (TestDeclaredOwnerFieldsAreServerStamped). So the mutation stamps
// the field from actor.userId -- which means this write needs an actor that IS
// the owner, on a path where the person may be somebody else entirely (a
// cluster owner deploying a package that belongs to a colleague) or nobody at
// all (a stage advance on another node).
//
// The value is safe to borrow because of where it came from: it is read off a
// package row the STARTING caller already resolved under their own actor
// through the composite tier, so it can never name a user that caller could
// not act as. That is component/campaigns' argument exactly -- the engine
// borrows the owner's authority rather than out-ranking it.
//
// An EMPTY owner is left alone: it is the cluster-owned state, and stamping a
// synthetic actor over it would turn a meaningful empty into a false name.
func (s *store) openDeployment(ctx context.Context, d deploymentSeed) error {
	writeCtx := ctx
	if owner := strings.TrimSpace(d.OwnerUserId); owner != "" {
		writeCtx = auth.ContextWithUserActor(ctx, owner)
	}
	scoped := d.ScopedTo
	if scoped == nil {
		scoped = []string{}
	}
	return s.writeInternal(writeCtx, fmt.Sprintf(
		"mutation openPackageDeployment(deploymentId: %s, packageId: %s, sourceVersion: %s, requestedBy: %s, automatic: %t, nodeId: %s, scopedTo: %s, fromDeploymentId: %s, startedAt: %s)",
		langparser.QuoteString(d.DeploymentId),
		langparser.QuoteString(d.PackageId),
		langparser.QuoteString(d.SourceVersion),
		langparser.QuoteString(d.RequestedBy),
		d.Automatic,
		langparser.QuoteString(d.NodeId),
		jsonLiteral(scoped),
		langparser.QuoteString(d.FromDeploymentId),
		langparser.QuoteString(d.StartedAt.UTC().Format(time.RFC3339)),
	))
}

// heartbeatDeployment says a run's node is still running it.
func (s *store) heartbeatDeployment(ctx context.Context, deploymentId string, at time.Time) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation heartbeatPackageDeployment(deploymentId: %s, heartbeatAt: %s)",
		langparser.QuoteString(deploymentId),
		langparser.QuoteString(at.UTC().Format(time.RFC3339)),
	))
}

// abandonDeployment closes a stranded run. The ONE write in this package that
// closes a row the pipeline did not finish, which is why it is its own method
// and its own mutation rather than a status value passed to closeDeployment:
// the permission is meant to be findable.
func (s *store) abandonDeployment(ctx context.Context, deploymentId, stoppedAt string, problem *Problem, at time.Time) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation abandonPackageDeployment(deploymentId: %s, stoppedAt: %s, error: %s, finishedAt: %s)",
		langparser.QuoteString(deploymentId),
		langparser.QuoteString(stoppedAt),
		jsonLiteral(problem),
		langparser.QuoteString(at.UTC().Format(time.RFC3339)),
	))
}

// setAutoDeploy flips a source's auto-deploy switch.
//
// NOT stamped internal, and that is the whole authorization: setPackageAutoDeploy
// is an OWNED mutation, so the write guard resolves the row and admits its
// owner or a cluster owner. Stamping internal origin here would make the
// capability a way for anyone who can reach it to arm auto-deploy on somebody
// else's source.
func (s *store) setAutoDeploy(ctx context.Context, packageId string, on bool) error {
	_, err := s.engine.Execute(ctx, fmt.Sprintf(
		"mutation setPackageAutoDeploy(packageId: %s, autoDeploy: %t)",
		langparser.QuoteString(packageId), on))
	return err
}

func (s *store) advance(ctx context.Context, deploymentId, status string) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation advancePackageDeployment(deploymentId: %s, status: %s)",
		langparser.QuoteString(deploymentId), langparser.QuoteString(status)))
}

func (s *store) recordReport(ctx context.Context, deploymentId string, rep *Report, snapshotArtifactId string) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation recordPackageDeploymentReport(deploymentId: %s, report: %s, sourceVersion: %s, snapshotArtifactId: %s)",
		langparser.QuoteString(deploymentId), jsonLiteral(rep),
		langparser.QuoteString(rep.SourceVersion), langparser.QuoteString(snapshotArtifactId)))
}

// recordScope re-stamps which deployables a run is for (memql#4953).
//
// Called when a PARKED run is confirmed, because the gate is where a person
// answers which apps they meant: the compose gate opens with no placements at
// all and closes with the skips somebody ticked.
func (s *store) recordScope(ctx context.Context, deploymentId string, scopedTo []string) error {
	if scopedTo == nil {
		scopedTo = []string{}
	}
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation recordPackageDeploymentScope(deploymentId: %s, scopedTo: %s)",
		langparser.QuoteString(deploymentId), jsonLiteral(scopedTo)))
}

// closeDeployment writes the terminal row.
//
// ABSENT FIELDS ARE RENDERED AS WHAT THE SCHEMA ACCEPTS, never as null. The
// success path closes with no error, and a run that stops before publish
// closes with no outcomes; jsonLiteral sees an interface holding a typed nil
// (*Problem, []DeployableOutcome) as non-nil, marshals it, and writes the word
// null -- which v1:platform:packageDeployment refuses for both `deployables`
// (an array) and `error` (an object). A close that fails leaves the row
// non-terminal with its heartbeat gone, and the sweep closes it "abandoned":
// on aks-memql a deploy that had built, rolled and published reported "this
// cluster lost the node that was running it" while the site served. So
// deployables is always an array, and error is named only when there is one.
func (s *store) closeDeployment(ctx context.Context, c deploymentClose) error {
	deployables := c.Deployables
	if deployables == nil {
		deployables = []DeployableOutcome{}
	}
	errorArg := ""
	if c.Error != nil {
		errorArg = ", error: " + jsonLiteral(c.Error)
	}
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation closePackageDeployment(deploymentId: %s, status: %s, deployables: %s, dslVersion: %s, buildLogTail: %s, builtOn: %s%s, finishedAt: %s)",
		langparser.QuoteString(c.DeploymentId),
		langparser.QuoteString(c.Status),
		jsonLiteral(deployables),
		langparser.QuoteString(c.DslVersion),
		langparser.QuoteString(c.BuildLogTail),
		jsonLiteral(c.BuiltOn),
		errorArg,
		langparser.QuoteString(c.FinishedAt.UTC().Format(time.RFC3339)),
	))
}

func (s *store) recordDeployedVersion(ctx context.Context, packageId, version string, updateAvailable bool) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation recordPackageDeployedVersion(packageId: %s, deployedVersion: %s, updateAvailable: %t)",
		langparser.QuoteString(packageId), langparser.QuoteString(version), updateAvailable))
}

func (s *store) recordPackageName(ctx context.Context, packageId, name string) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation recordPackageName(packageId: %s, name: %s)",
		langparser.QuoteString(packageId), langparser.QuoteString(name)))
}

// recordPackageDeployables writes what the manifest declares onto the package.
//
// ALWAYS AN ARRAY, never null -- the same trap closeDeployment documents above:
// jsonLiteral sees an interface holding a typed nil slice as non-nil, marshals
// it and writes the word `null`, which the concept's []object refuses. A source
// whose manifest declares nothing is an empty list, which is a true statement;
// a refused write would leave the package claiming whatever the LAST analysis
// found, which is worse than saying nothing.
func (s *store) recordPackageDeployables(ctx context.Context, packageId string, declares []DeclaredDeployable) error {
	if declares == nil {
		declares = []DeclaredDeployable{}
	}
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation recordPackageDeployables(packageId: %s, declares: %s)",
		langparser.QuoteString(packageId), jsonLiteral(declares)))
}

func (s *store) recordUpstreamVersion(ctx context.Context, packageId, version string, updateAvailable bool) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation recordPackageUpstreamVersion(packageId: %s, latestKnownVersion: %s, updateAvailable: %t)",
		langparser.QuoteString(packageId), langparser.QuoteString(version), updateAvailable))
}

func (s *store) bindSiteToPackage(ctx context.Context, siteId, packageId, deployableName string) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation recordSitePackageOrigin(siteId: %s, packageId: %s, packageDeployableName: %s)",
		langparser.QuoteString(siteId), langparser.QuoteString(packageId), langparser.QuoteString(deployableName)))
}

func (s *store) setPackageStatus(ctx context.Context, packageId, status string) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation setPackageStatus(packageId: %s, status: %s)",
		langparser.QuoteString(packageId), langparser.QuoteString(status)))
}

func (s *store) setSiteStatus(ctx context.Context, siteId, status string) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation setSiteStatus(siteId: %s, status: %s)",
		langparser.QuoteString(siteId), langparser.QuoteString(status)))
}

// ---------------------------------------------------------------------------
// Source credentials (epic memql#4885, D10)
// ---------------------------------------------------------------------------

// sourceCredentialSealedById is THE ONE STAMPED READ in this package, and the
// exception to the header's rule is narrower than it looks. The query is
// @serverOnly -- it returns ciphertext, and a client-callable projection of
// encryptedValue would be a ciphertext oracle even for the row's own owner --
// so the stamp is what lets the engine reach the construct at all. What the
// stamp does NOT do is widen the read: origin decides whether the construct may
// be called, the actor decides which rows come back, and there is no
// internal-origin bypass on the read path. The caller (resolveCredential)
// passes a ctx already carrying the PACKAGE OWNER's actor, and the query's own
// owner term -- plus the tier the engine ANDs in -- admits exactly that owner's
// rows. Nil, not an error, for zero rows: "does not exist" and "belongs to
// somebody else" are the same answer here and the caller's sentence says so.
func (s *store) sourceCredentialSealedById(ctx context.Context, credentialId string) (map[string]any, error) {
	rows, err := s.executeInternal(ctx, fmt.Sprintf(
		"query sourceCredentialSealedById(credentialId: %s)", langparser.QuoteString(credentialId)))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// githubAppGrantForCaller reads the caller's own active grant, or nil.
//
// The SECOND stamped read, and for the same reason as the first: the query is
// @serverOnly because it carries the sealed shape, so the stamp is what lets
// the engine reach the construct at all -- and it widens nothing, because the
// read path has no internal-origin bypass and the query's own
// `ownerUserId==actor.userId` term decides the rows.
//
// It takes no argument, and the absence is the authorization: there is no
// value a caller could supply that would make this answer with somebody else's
// grant. Nil for zero rows -- a person who has not connected GitHub, or who
// disconnected -- which is what makes the surface offer Connect rather than a
// picker that would refuse every repository in it.
func (s *store) githubAppGrantForCaller(ctx context.Context) (map[string]any, error) {
	rows, err := s.executeInternal(ctx, "query githubAppGrantForCaller()")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// recordRefreshedGrantToken writes a user token the engine renewed on its own.
//
// Stamped internal because the mutation is @serverOnly -- and internal origin
// is also the write guard's escape, so what stops this reaching somebody
// else's row is NOT the guard but where the id came from: unsealCredential
// resolved it through the owner-scoped sealed read moments earlier, under the
// GRANT OWNER's borrowed actor, so an id that arrives here is one that read
// admitted. That is the same borrowed-authority argument openDeployment makes,
// and it is the whole of the protection.
func (s *store) recordRefreshedGrantToken(ctx context.Context, g grantTokenSeed) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation refreshGithubAppGrantToken(credentialId: %s, encryptedValue: %s, fingerprint: %s, refreshToken: %s, expiresAt: %s)",
		langparser.QuoteString(g.CredentialId),
		langparser.QuoteString(g.EncryptedValue),
		langparser.QuoteString(g.Fingerprint),
		langparser.QuoteString(g.RefreshToken),
		langparser.QuoteString(rfc3339OrEmpty(g.ExpiresAt)),
	))
}

// recordGrantInstallations replaces a grant's installation ids from what the
// caller has just read live from GitHub.
//
// REPLACED rather than merged, because the listing carries the whole set and a
// merge could never remove one -- and an installation that stays on the row
// after it was uninstalled is exactly the state that turns a clear
// repository_not_installed into an unexplained 404.
//
// Stamped internal, and as above the stamp opens the write guard rather than
// satisfying it: the credentialId reached here from a read the CALLER was
// admitted to (their own grant, through githubAppGrantForCaller or the
// owner-scoped sealed read), so it can only ever name a row they own.
//
// There is deliberately no webhook path writing this. A delivery names a
// GitHub identity rather than a MemQL user, so finding the grant it belongs to
// would be a cross-owner read past the concept's own tier -- and nothing reads
// installationIds on a hot path anyway, because the fetcher asks GitHub which
// installation covers a repository, live. The stored list is a display cache,
// which is exactly what an owner-actor refresh is good enough for.
func (s *store) recordGrantInstallations(ctx context.Context, credentialId string, installationIds []string) error {
	if installationIds == nil {
		installationIds = []string{}
	}
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation recordGithubAppInstallations(credentialId: %s, installationIds: %s)",
		langparser.QuoteString(credentialId), jsonLiteral(installationIds)))
}

// createSourceCredential lands a sealed credential under the CALLER's own
// actor: ownerUserId is stamped from actor.userId inside the mutation, so the
// ctx handed here must be the person's and never a borrowed one. Stamped
// internal because the mutation is @serverOnly -- the ciphertext is a value
// only the Go frame that sealed it can vouch for, and the stamp is origin, not
// identity, so the owner stays the person who asked.
func (s *store) createSourceCredential(ctx context.Context, c credentialSeed) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation createSourceCredential(credentialId: %s, host: %s, label: %s, encryptedValue: %s, fingerprint: %s)",
		langparser.QuoteString(c.CredentialId),
		langparser.QuoteString(c.Host),
		langparser.QuoteString(c.Label),
		langparser.QuoteString(c.EncryptedValue),
		langparser.QuoteString(c.Fingerprint),
	))
}

// touchSourceCredential stamps the heartbeat after an unseal -- the engine's
// own account of a fetch, which is why the mutation is @serverOnly and why the
// caller treats a failure here as bookkeeping rather than as a failed fetch.
func (s *store) touchSourceCredential(ctx context.Context, credentialId string, at time.Time) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation touchSourceCredential(credentialId: %s, lastUsedAt: %s)",
		langparser.QuoteString(credentialId),
		langparser.QuoteString(at.UTC().Format(time.RFC3339))))
}

// revokeSourceCredential is a CALLER-ACTOR write and is deliberately NOT
// stamped. The mutation is an ordinary owned one: the write guard resolves the
// target row and admits its owner, or a cluster owner through the explicit
// escape, and refuses everyone else. Stamping it internal would hand the guard
// its FIRST escape -- internal origin is trusted server-side Go -- and let any
// caller who can reach this capability revoke any credential on the cluster.
func (s *store) revokeSourceCredential(ctx context.Context, credentialId string) error {
	return s.writeAsCaller(ctx, fmt.Sprintf(
		"mutation revokeSourceCredential(credentialId: %s)", langparser.QuoteString(credentialId)))
}

// ---------------------------------------------------------------------------
// Placements (epic memql#4885, D8) -- caller-actor writes
// ---------------------------------------------------------------------------

// writeAsCaller runs one statement under whatever actor and origin the caller
// already has -- the plain Execute, named so a reader can tell it from
// writeInternal at a glance. Everything that goes through here is a call the
// PAGE could make, and is authorized by the guard behind the construct rather
// than by anything in this package; internal_origin_test.go's callerWrites
// list names each one and asserts the stamp is absent.
func (s *store) writeAsCaller(ctx context.Context, query string) error {
	if _, err := s.engine.Execute(ctx, query); err != nil {
		return fmt.Errorf("%s: %w", firstToken(query), err)
	}
	return nil
}

// setSiteAccount points a freshly created site at the client it is FOR. The
// same updateSiteAccount the site detail's account picker issues, under the
// caller's actor, so v1:platform:site's composite write guard admits the row's
// owner (or a cluster owner) and refuses everyone else -- exactly as it does
// from the page.
func (s *store) setSiteAccount(ctx context.Context, siteId, accountId string) error {
	return s.writeAsCaller(ctx, fmt.Sprintf(
		"mutation updateSiteAccount(siteId: %s, accountId: %s)",
		langparser.QuoteString(siteId), langparser.QuoteString(accountId)))
}

// addCustomDomain binds a client's own domain to a freshly created site. The
// same customDomainAdd builtin the Domains panel issues, under the caller's
// actor, so the three guards in platform_custom_domain_policy.go -- not under
// the cluster's own domain, not a collision, not past the per-site cap --
// decide exactly as they do from the page. Any error is the guard's own
// sentence, which the publish stage records on the outcome rather than
// failing over.
func (s *store) addCustomDomain(ctx context.Context, siteId, hostname string) error {
	return s.writeAsCaller(ctx, fmt.Sprintf(
		"builtin customDomainAdd(siteId: %s, hostname: %s)",
		langparser.QuoteString(siteId), langparser.QuoteString(hostname)))
}

// deleteSite stamps the soft-delete flag, which is what RELEASES THE HOSTNAME
// (epic memql#4937).
//
// UNDER THE CALLER'S ACTOR, not internal, and that is the whole authorization:
// `deleteSite` is an owned, client-reachable mutation, so guardRowAuthzWrite
// resolves the prior row and admits its owner with the cluster-owner path as
// the separate escape -- and validateSiteSystemOwnedDelete refuses the
// platform's own rows beside executeWrite whoever asks. Stamping internal
// origin here would turn the capability into a way for anyone who can reach it
// to delete somebody else's deployable.
//
// `deleted` is the ONLY field liveSiteIdsForHostname excludes on
// (platform_site_hostname_policy.go) -- it never reads `status` -- so this
// write, and nothing before it in the cascade, is what makes the name
// reusable.
func (s *store) deleteSite(ctx context.Context, siteId string) error {
	return s.writeAsCaller(ctx, fmt.Sprintf(
		"mutation deleteSite(siteId: %s)", langparser.QuoteString(siteId)))
}

// releaseDomainsForSite walks every live binding on a site to `removing` and
// answers how many it asked for.
//
// A BUILTIN CALL rather than a direct write, for the reason addCustomDomain
// above is one: the rows and their state machine belong to the custom-domain
// integration, and reaching into them from here would put the walk in two
// places that could disagree about what terminal means. @serverOnly, so it is
// stamped internal -- the caller was already authorized against the SITE,
// which is the row this cascade is about.
func (s *store) releaseDomainsForSite(ctx context.Context, siteId string) (int, error) {
	rows, err := s.executeInternal(ctx, fmt.Sprintf(
		"builtin customDomainReleaseForSite(siteId: %s)", langparser.QuoteString(siteId)))
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rowInt(rows[0], "requested"), nil
}

// requestDeploymentCancel flags a run as cancel-requested. Internal, because
// the mutation is @serverOnly: the capability resolved the deployment under
// the caller's own actor first, which is where the authorization happened.
func (s *store) requestDeploymentCancel(ctx context.Context, deploymentId string) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation requestPackageDeploymentCancel(deploymentId: %s)",
		langparser.QuoteString(deploymentId)))
}

// cancelRequestedFor re-reads the flag the running pipeline checks at each
// stage boundary. Internal: the pipeline is not a person and has no actor of
// its own on a stage advance.
func (s *store) cancelRequestedFor(ctx context.Context, deploymentId string) (bool, error) {
	row, err := s.deploymentById(ctx, deploymentId)
	if err != nil || row == nil {
		return false, err
	}
	return rowBool(row, "cancelRequested"), nil
}

// ---------------------------------------------------------------------------
// Row payloads
// ---------------------------------------------------------------------------

// credentialSeed is what createSourceCredential writes. No owner field, and
// that absence is the design: the mutation stamps ownerUserId from the actor,
// and a seed that could carry one would be the caller-supplied owner
// TestDeclaredOwnerFieldsAreServerStamped exists to refuse.
type credentialSeed struct {
	CredentialId   string
	Host           string
	Label          string
	EncryptedValue string
	Fingerprint    string
}

// grantTokenSeed is what recordRefreshedGrantToken writes: FOUR TOKEN FIELDS
// AND NOTHING ELSE.
//
// Not the login, not the installations, not the status. A refresh is evidence
// about a token, and a writer that also touched the installations would be
// reporting a fact it did not observe -- the same argument credentialSeed
// makes for not carrying an owner.
type grantTokenSeed struct {
	CredentialId   string
	EncryptedValue string
	Fingerprint    string
	// RefreshToken is the SEALED rotated refresh token, or the sealed value
	// already on the row when GitHub did not rotate one. Never plaintext.
	RefreshToken string
	ExpiresAt    time.Time
}

type deploymentSeed struct {
	DeploymentId  string
	PackageId     string
	OwnerUserId   string
	SourceVersion string
	RequestedBy   string
	// Automatic marks a run the auto-deploy switch started (epic memql#4900,
	// task memql#4903).
	Automatic bool
	// NodeId is the replica opening the run -- the node the abandoned sweep
	// names when it closes it.
	NodeId string
	// ScopedTo names the deployables this run is FOR; empty is the whole
	// source (memql#4953).
	ScopedTo []string
	// FromDeploymentId is the run this one was started from, when it is a
	// retry (memql#4955).
	FromDeploymentId string
	StartedAt        time.Time
}

type deploymentClose struct {
	DeploymentId string
	Status       string
	Deployables  []DeployableOutcome
	DslVersion   string
	BuildLogTail string
	BuiltOn      BuiltOn
	Error        *Problem
	FinishedAt   time.Time
}

// DeployableOutcome is one deployable's result on a deployment row. It is the
// shape the OS reads to link a deployable to the site it produced.
type DeployableOutcome struct {
	Name string `json:"name"`
	// BuiltOn is where THIS app was built. Per-app as well as per-run,
	// because a package whose apps built on different surfaces is exactly
	// the case the run-level summary cannot describe.
	BuiltOn   BuiltOn  `json:"builtOn,omitzero"`
	SiteId    string   `json:"siteId,omitempty"`
	Hostname  string   `json:"hostname,omitempty"`
	BundleRef string   `json:"bundleRef,omitempty"`
	Version   string   `json:"version,omitempty"`
	Created   bool     `json:"created,omitempty"`
	Refusal   *Problem `json:"refusal,omitempty"`

	// The placement halves a first deploy applied (epic memql#4885, D8), and
	// the ones it could not. AccountId and OwnDomain are set only when the
	// write LANDED; a refusal is recorded on the sibling field with the
	// server's own sentence and is NOT fatal -- the site is live at its
	// cluster address regardless, and the Where-it-lives stop renders the
	// refusal in place.
	AccountId      string   `json:"accountId,omitempty"`
	OwnDomain      string   `json:"ownDomain,omitempty"`
	AccountRefusal *Problem `json:"accountRefusal,omitempty"`
	DomainRefusal  *Problem `json:"domainRefusal,omitempty"`
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// jsonLiteral renders v as the JSON object/array literal an engine.Execute
// object argument accepts. A nil or unmarshalable value yields an empty
// literal rather than an error: a report that failed to marshal must not be
// what stops a deployment recording that it failed.
func jsonLiteral(v any) string {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 {
		return "{}"
	}
	return string(b)
}

func firstToken(q string) string {
	q = strings.TrimSpace(q)
	if i := strings.IndexAny(q, " ("); i > 0 {
		rest := strings.TrimSpace(q[i:])
		if j := strings.IndexByte(rest, '('); j > 0 {
			return strings.TrimSpace(rest[:j])
		}
	}
	return "mutation"
}

func rowString(row map[string]any, key string) string {
	if row == nil {
		return ""
	}
	if v, ok := row[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// rowStrings reads a []string field off a row.
//
// It handles both spellings the engine can hand back -- a real []string, and
// the []any a structpb-backed bundle decodes to -- because a caller that
// handled only one would read an empty list on the other path and report a
// grant that reaches no installations.
func rowStrings(row map[string]any, key string) []string {
	if row == nil {
		return nil
	}
	switch v := row[key].(type) {
	case []string:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s := strings.TrimSpace(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}

// rfc3339OrEmpty renders a timestamp, answering the EMPTY STRING for a zero
// time rather than year one. A datetime field left empty is "not known", and
// "0001-01-01T00:00:00Z" is a date -- one the expiry comparison would read as
// long past and refresh against on every call.
func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// rowInt narrows a decoded payload number to a count.
//
// THROUGH core/num WITH A NAMED ANSWER, never a bare `int(v)`: an out-of-range
// conversion in a float64 or int64 arm is implementation-defined and answers
// with the integer indefinite value, which is why
// TestEveryPayloadNarrowingCarriesAnAnswer refuses one that declares none. The
// answer here is SATURATE, because every caller is a count of rows this
// process just wrote -- a value past the int range is not a real count, and
// the largest representable one is the honest reading of "more than you can
// hold" where zero would read as "none, and nothing happened".
func rowInt(row map[string]any, key string) int {
	if row == nil {
		return 0
	}
	switch v := row[key].(type) {
	case int:
		return v
	case int64:
		return num.ClampInt64(v)
	case float64:
		return num.ClampFloat64(v)
	}
	return 0
}

func rowBool(row map[string]any, key string) bool {
	if row == nil {
		return false
	}
	switch v := row[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return false
}
