package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
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
type store struct {
	engine Engine
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

// writeInternal runs one @serverOnly mutation under a context this function
// constructs. The stamp is applied here and nowhere else in the package.
func (s *store) writeInternal(ctx context.Context, query string) error {
	_, err := s.engine.Execute(auth.ContextWithInternalOrigin(ctx), query)
	if err != nil {
		return fmt.Errorf("%s: %w", firstToken(query), err)
	}
	return nil
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
	return s.writeInternal(writeCtx, fmt.Sprintf(
		"mutation openPackageDeployment(deploymentId: %s, packageId: %s, sourceVersion: %s, requestedBy: %s, automatic: %t, nodeId: %s, startedAt: %s)",
		langparser.QuoteString(d.DeploymentId),
		langparser.QuoteString(d.PackageId),
		langparser.QuoteString(d.SourceVersion),
		langparser.QuoteString(d.RequestedBy),
		d.Automatic,
		langparser.QuoteString(d.NodeId),
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

func (s *store) closeDeployment(ctx context.Context, c deploymentClose) error {
	return s.writeInternal(ctx, fmt.Sprintf(
		"mutation closePackageDeployment(deploymentId: %s, status: %s, deployables: %s, dslVersion: %s, buildLogTail: %s, builtOn: %s, error: %s, finishedAt: %s)",
		langparser.QuoteString(c.DeploymentId),
		langparser.QuoteString(c.Status),
		jsonLiteral(c.Deployables),
		langparser.QuoteString(c.DslVersion),
		langparser.QuoteString(c.BuildLogTail),
		jsonLiteral(c.BuiltOn),
		jsonLiteral(c.Error),
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
// Row payloads
// ---------------------------------------------------------------------------

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
	NodeId    string
	StartedAt time.Time
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
