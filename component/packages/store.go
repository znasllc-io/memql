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
	return s.writeInternal(writeCtx, fmt.Sprintf(
		"mutation openPackageDeployment(deploymentId: %s, packageId: %s, sourceVersion: %s, requestedBy: %s, startedAt: %s)",
		langparser.QuoteString(d.DeploymentId),
		langparser.QuoteString(d.PackageId),
		langparser.QuoteString(d.SourceVersion),
		langparser.QuoteString(d.RequestedBy),
		langparser.QuoteString(d.StartedAt.UTC().Format(time.RFC3339)),
	))
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
		"mutation closePackageDeployment(deploymentId: %s, status: %s, deployables: %s, dslVersion: %s, buildLogTail: %s, error: %s, finishedAt: %s)",
		langparser.QuoteString(c.DeploymentId),
		langparser.QuoteString(c.Status),
		jsonLiteral(c.Deployables),
		langparser.QuoteString(c.DslVersion),
		langparser.QuoteString(c.BuildLogTail),
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
	if _, err := s.engine.Execute(ctx, fmt.Sprintf(
		"mutation revokeSourceCredential(credentialId: %s)", langparser.QuoteString(credentialId))); err != nil {
		return fmt.Errorf("revokeSourceCredential: %w", err)
	}
	return nil
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

type deploymentSeed struct {
	DeploymentId  string
	PackageId     string
	OwnerUserId   string
	SourceVersion string
	RequestedBy   string
	StartedAt     time.Time
}

type deploymentClose struct {
	DeploymentId string
	Status       string
	Deployables  []DeployableOutcome
	DslVersion   string
	BuildLogTail string
	Error        *Problem
	FinishedAt   time.Time
}

// DeployableOutcome is one deployable's result on a deployment row. It is the
// shape the OS reads to link a deployable to the site it produced.
type DeployableOutcome struct {
	Name      string   `json:"name"`
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
