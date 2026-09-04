package customdomain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// store.go -- the graph seam. Every read and write this package makes is a
// NAMED construct rendered as MemQL text and handed to the engine, which is the
// same shape component/edge, component/campaigns and component/identity all
// use. Nothing here reaches the database.
//
// # The actor, and why it is synthetic
//
// v1:platform:customDomain declares @rowAuthz(clusterOwner). The sweep is a
// service rather than a person, so it stamps a synthetic cluster-owner identity
// -- the same precedent component/edge's systemEdgeActor and
// component/campaigns' systemCampaignsActor set, including the reasoning: the
// identity is only as powerful as what it is asked to do, not as powerful as
// the role it carries, and the scope of what it is asked to do here is the six
// named constructs below over one concept.
//
// # The internal-origin stamp is load-bearing
//
// The five writers are @serverOnly. Call origin defaults to CLIENT, so without
// auth.ContextWithInternalOrigin every one of them is refused at runtime by the
// function validator -- and the failure surfaces as a WARN in the log with the
// row silently not moving, which is the shape of bug that survives a release.

// systemCustomDomainActor is the engine's own operator identity for this
// package's clusterOwner-tier reads and writes.
const systemCustomDomainActor = "system:customDomain"

// Engine is the narrow engine surface this package needs.
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

// Binding is the projection of one v1:platform:customDomain row the sweep and
// the add capability work with.
type Binding struct {
	ID            string
	SiteID        string
	Hostname      string
	AccountID     string
	Token         string
	Status        string
	FailureReason string
	FailureDetail string
	LastCheckedAt string
	VerifiedAt    string
	IssuedAt      string
	RemovedAt     string
}

// The status vocabulary. Six values, closed by the concept's own enum, and
// three of them are COMMANDS the sweep acts on rather than states it observes.
const (
	StatusPendingDNS = "pending_dns"
	StatusVerifying  = "verifying"
	StatusIssuing    = "issuing"
	StatusLive       = "live"
	StatusRemoving   = "removing"
	StatusRemoved    = "removed"
)

// NonTerminal reports whether a status is one the sweep still has work for.
//
// `live` and `removed` are the two settled states, so a cluster with a hundred
// bound domains and nothing in flight does no DNS lookups at all -- which is
// what makes a two-minute schedule affordable.
func NonTerminal(status string) bool {
	switch status {
	case StatusPendingDNS, StatusVerifying, StatusIssuing, StatusRemoving:
		return true
	default:
		return false
	}
}

// SystemActorContext stamps the synthetic cluster-owner identity plus internal
// origin onto ctx.
//
// It sets the same three surfaces auth.ContextWithUserActor sets for a real
// user -- claims, TokenInfo, AccessContext -- because createdBy and
// actor.userId read different ones (memql#2989), and a mutation or filter
// reading the wrong one is how a synthetic actor silently resolves to nobody.
// The Role is RoleOwner rather than RoleWriter because AccessContext.IsClusterOwner()
// reads Role == RoleOwner, and that bit is what every filter's
// `actor.isClusterOwner==true` conjunct is checking -- which is exactly why
// auth.ContextWithUserActor is not a substitute for this.
func SystemActorContext(ctx context.Context) context.Context {
	claims := map[string]any{"sub": systemCustomDomainActor, "role": "owner"}
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: systemCustomDomainActor,
		Role:   auth.RoleOwner,
	})
	return auth.ContextWithInternalOrigin(ctx)
}

// Store reads and writes custom-domain bindings through the engine.
type Store struct{ engine Engine }

// NewStore wraps an engine.
func NewStore(engine Engine) *Store { return &Store{engine: engine} }

// ToReconcile returns every binding the sweep still has work for.
//
// NOT `customDomainsAll`, which is paginated because it backs a list somebody
// scrolls. A sweep that read a page would silently never reconcile the
// bindings past it, and the symptom -- a domain that verifies for nobody, with
// nothing in any log -- is indistinguishable from a DNS problem on the
// client's side.
func (s *Store) ToReconcile(ctx context.Context) ([]Binding, error) {
	rows, err := s.rows(ctx, "query customDomainsToReconcile()")
	if err != nil {
		return nil, err
	}
	out := make([]Binding, 0, len(rows))
	for _, r := range rows {
		out = append(out, bindingFromRow(r))
	}
	return out, nil
}

// ForSite returns the bindings on one deployable, removed ones included.
func (s *Store) ForSite(ctx context.Context, siteID string) ([]Binding, error) {
	rows, err := s.rows(ctx, fmt.Sprintf(
		"query customDomainsForSite(siteId: %s)", langparser.QuoteString(siteID)))
	if err != nil {
		return nil, err
	}
	out := make([]Binding, 0, len(rows))
	for _, r := range rows {
		out = append(out, bindingFromRow(r))
	}
	return out, nil
}

// Create writes a new binding. The caller mints the token.
func (s *Store) Create(ctx context.Context, b Binding) error {
	var q strings.Builder
	q.WriteString("mutation createCustomDomain(domainId: ")
	q.WriteString(langparser.QuoteString(b.ID))
	q.WriteString(", siteId: ")
	q.WriteString(langparser.QuoteString(b.SiteID))
	q.WriteString(", hostname: ")
	q.WriteString(langparser.QuoteString(b.Hostname))
	q.WriteString(", token: ")
	q.WriteString(langparser.QuoteString(b.Token))
	if strings.TrimSpace(b.AccountID) != "" {
		q.WriteString(", accountId: ")
		q.WriteString(langparser.QuoteString(b.AccountID))
	}
	q.WriteString(")")
	return s.exec(ctx, q.String())
}

// RecordCheck records a failed verification pass.
func (s *Store) RecordCheck(ctx context.Context, domainID, reason, detail string, at time.Time) error {
	return s.exec(ctx, fmt.Sprintf(
		"mutation recordCustomDomainCheck(domainId: %s, failureReason: %s, failureDetail: %s, lastCheckedAt: %s)",
		langparser.QuoteString(domainID),
		langparser.QuoteString(reason),
		langparser.QuoteString(detail),
		langparser.QuoteString(stamp(at))))
}

// MarkVerified promotes a binding to `issuing`.
func (s *Store) MarkVerified(ctx context.Context, domainID string, at time.Time) error {
	return s.exec(ctx, fmt.Sprintf(
		"mutation markCustomDomainVerified(domainId: %s, verifiedAt: %s, lastCheckedAt: %s)",
		langparser.QuoteString(domainID),
		langparser.QuoteString(stamp(at)),
		langparser.QuoteString(stamp(at))))
}

// MarkLive closes the walk at `live`.
func (s *Store) MarkLive(ctx context.Context, domainID string, at time.Time) error {
	return s.exec(ctx, fmt.Sprintf(
		"mutation markCustomDomainLive(domainId: %s, issuedAt: %s, lastCheckedAt: %s)",
		langparser.QuoteString(domainID),
		langparser.QuoteString(stamp(at)),
		langparser.QuoteString(stamp(at))))
}

// RecordIssuanceFailure keeps a binding in `issuing` and records why.
func (s *Store) RecordIssuanceFailure(ctx context.Context, domainID, reason, detail string, at time.Time) error {
	return s.exec(ctx, fmt.Sprintf(
		"mutation recordCustomDomainIssuanceFailure(domainId: %s, failureReason: %s, failureDetail: %s, lastCheckedAt: %s)",
		langparser.QuoteString(domainID),
		langparser.QuoteString(reason),
		langparser.QuoteString(detail),
		langparser.QuoteString(stamp(at))))
}

// RecordIssuingProgress records a pass where the objects are applied and the
// certificate is not Ready yet -- the ordinary state for the first minute of an
// HTTP-01 order.
//
// It writes NO failureReason, and that distinction is the whole reason it is a
// separate call from RecordIssuanceFailure: waiting for ACME is not a failure,
// and a typed reason on the row would make a normal wait render in the panel as
// something a person should go and fix.
func (s *Store) RecordIssuingProgress(ctx context.Context, domainID, note string, at time.Time) error {
	return s.exec(ctx, fmt.Sprintf(
		"mutation recordCustomDomainIssuingProgress(domainId: %s, note: %s, lastCheckedAt: %s)",
		langparser.QuoteString(domainID),
		langparser.QuoteString(note),
		langparser.QuoteString(stamp(at))))
}

// RequestRemoval walks a binding to `removing`, which is what the deployable
// delete cascade asks for (epic memql#4937).
//
// The SAME mutation the Domains panel's Remove issues, deliberately: a domain
// coming down because its deployable was deleted and one coming down because
// somebody clicked Remove are the same journey, and giving the cascade a
// second write would be a second path the sweep would have to agree with.
//
// THE HOSTNAME STOPS RESOLVING AT THIS WRITE rather than at the Ingress
// deletion -- `liveCustomDomainByHostname` filters `status=="live"` -- so a
// deleted deployable stops answering on its client's domain immediately, and
// the certificate and route come down on the sweep's own schedule.
func (s *Store) RequestRemoval(ctx context.Context, domainID string) error {
	return s.exec(ctx, fmt.Sprintf(
		"mutation removeCustomDomain(domainId: %s)", langparser.QuoteString(domainID)))
}

// MarkRemoved closes the walk at `removed`. The row stays.
func (s *Store) MarkRemoved(ctx context.Context, domainID string, at time.Time) error {
	return s.exec(ctx, fmt.Sprintf(
		"mutation markCustomDomainRemoved(domainId: %s, removedAt: %s, lastCheckedAt: %s)",
		langparser.QuoteString(domainID),
		langparser.QuoteString(stamp(at)),
		langparser.QuoteString(stamp(at))))
}

// SiteAccountID returns the account a deployable is tied to, or "" when the
// cluster has no accounts concept, no tie, or no readable site.
//
// BEST-EFFORT BY DESIGN. Epic B (memql#4800) supplies `site.accountId`, and
// this epic consumes it as a SUGGESTION source and nothing more (design D9). On
// a cluster where that epic has not landed the field is simply absent from the
// projection, the binding records an empty accountId, and everything else
// behaves identically -- which is why this returns a value rather than an
// error, and why nothing downstream branches on whether it found one.
func (s *Store) SiteAccountID(ctx context.Context, siteID string) string {
	rows, err := s.callerRows(ctx, fmt.Sprintf(
		"query siteById(siteId: %s)", langparser.QuoteString(siteID)))
	if err != nil || len(rows) == 0 {
		return ""
	}
	return strings.TrimSpace(memql.BareShortId(rowString(rows[0], "accountId")))
}

// SiteExists reports whether the deployable a binding names is readable.
//
// The check is the reason a binding cannot name nothing: `siteId` is required
// by the concept, but "required" only means non-empty, and a hostname bound to
// an id no row carries is one the edge would resolve to a 404 that looks like
// a serving bug rather than a typo.
func (s *Store) SiteExists(ctx context.Context, siteID string) (bool, error) {
	rows, err := s.callerRows(ctx, fmt.Sprintf(
		"query siteById(siteId: %s)", langparser.QuoteString(siteID)))
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

// callerRows issues a read under the CALLER's own actor rather than the
// synthetic operator every other read here uses.
//
// The distinction is the whole reason this method exists. `siteById` carries
// v1:platform:site's composite tier, so read under the caller it answers
// "which deployables may YOU act on" -- and under the sweep's operator identity
// it answers "which exist", which is a different question and not one the add
// path should be asking. Reading as the operator made the existence check pass
// for a deployable the caller could not see, leaving the refusal to row authz
// on the write: correct in outcome, but late, and after a token had been
// minted for a binding that was never going to land.
//
// It stamps NO internal origin either. Nothing it reaches is @serverOnly, and
// stamping one would hand a caller-scoped read the engine's own escape.
func (s *Store) callerRows(ctx context.Context, query string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("customdomain: no engine wired")
	}
	res, err := s.engine.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("customdomain: %s: %w", firstWord(query), err)
	}
	return memql.MaterializeRows(res), nil
}

func (s *Store) rows(ctx context.Context, query string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("customdomain: no engine wired")
	}
	res, err := s.engine.Execute(SystemActorContext(ctx), query)
	if err != nil {
		return nil, fmt.Errorf("customdomain: %s: %w", firstWord(query), err)
	}
	return memql.MaterializeRows(res), nil
}

func (s *Store) exec(ctx context.Context, query string) error {
	if s == nil || s.engine == nil {
		return fmt.Errorf("customdomain: no engine wired")
	}
	if _, err := s.engine.Execute(SystemActorContext(ctx), query); err != nil {
		return fmt.Errorf("customdomain: %s: %w", firstWord(query), err)
	}
	return nil
}

// stamp renders a time the way every datetime arg in this tree is rendered.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func bindingFromRow(r map[string]any) Binding {
	return Binding{
		// BARE, like every other named-query reader in this tree: an id field
		// on a @relationship concept comes back canonicalized, and the
		// BARE-ids wire contract is what the client and every sibling
		// component already follow.
		ID:            memql.BareShortId(rowString(r, "id")),
		SiteID:        memql.BareShortId(rowString(r, "siteId")),
		Hostname:      rowString(r, "hostname"),
		AccountID:     memql.BareShortId(rowString(r, "accountId")),
		Token:         rowString(r, "token"),
		Status:        rowString(r, "status"),
		FailureReason: rowString(r, "failureReason"),
		FailureDetail: rowString(r, "failureDetail"),
		LastCheckedAt: rowString(r, "lastCheckedAt"),
		VerifiedAt:    rowString(r, "verifiedAt"),
		IssuedAt:      rowString(r, "issuedAt"),
		RemovedAt:     rowString(r, "removedAt"),
	}
}

func rowString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

func firstWord(q string) string {
	if i := strings.IndexAny(q, " ("); i > 0 {
		return q[:i]
	}
	return q
}
