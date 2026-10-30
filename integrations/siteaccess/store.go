package siteaccess

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"
)

// store.go -- the graph seam. Every read and write is a NAMED construct rendered
// as MemQL text and handed to the engine, the shape integrations/sitepreview and
// integrations/customdomain use. Nothing here reaches the database.
//
// # Three actors, and which is which is the whole of the authorization story
//
// AN OPERATOR'S ACT AUTHORIZES UNDER THE CALLER. The site and its package are read
// with no stamp, and that is what makes them checks rather than lookups: siteById and
// packageById carry the composite tier and the organization conjunct, so a caller who
// may not see either resolves zero rows and is refused before anything is minted. A
// stamp would hand a read that IS the check the engine's escape for trusted Go, which
// is integrations/customdomain's reason exactly. A code's rows are read with internal
// origin under the same untouched actor, because the concept is unreadable by any
// client whatever the tier says (clientUnreadableConcepts, component/memql): an
// unstamped read answers nothing for everybody, and the composite tier still decides
// every row. The builtin in front of these has already passed the part and the
// organization gate.
//
// THE STORE'S OWN READS AND UPDATES RUN AS THE CLUSTER ACTING: a synthetic actor with
// internal origin, as component/edge reads a preview grant, through the
// `actor.isClusterOwner` arm the queries already state. A ROW'S CURRENT OWNER MUST NOT
// DECIDE ANYTHING HERE. Whole-user transfer once moved a code to an heir while its tied
// site kept its owner, and a transfer of the site alone does the opposite, and either
// way a lookup as the site's owner missed a code that should open. Nor can an operator
// who only belongs to the site's organization read a session, whose tier has no account
// arm. So nothing here compares an owner: the bff's handler compares the edge's stamp
// with the site's own ownerUserId before it calls in, and THAT IS WHAT CHECKS THE OWNER.
// These reads answer for anybody who reaches them, which is why they are @serverOnly and
// why a caller outside this package holds only the seven functions below. A test persona
// is judged under the caller, at the reads above, and only asked after that whether its
// session is still live: see asCluster.
//
// THE TWO CREATES RUN AS THE SITE'S OWNER, so a row is stamped with the site's owner at
// birth and never the operator's. That stamp stays where it landed: ownership transfer
// skips both concepts (integrations/identity). The person who asked is recorded
// separately, in `lastChangedBy` and the audit event.
//
// # A witness never leaves the call that needs it
//
// Each write of a code carries the version of the row it was decided from
// (ContextWithRowVersionFence), because without it two regenerates that both read
// generation 1 both report success at generation 2, and the first operator holds a
// code that no longer works. A witness applies to every write under its context,
// and the write it guards has just moved the row it names, so a later write under
// the same context is refused. So the fenced context is a local of each write
// below and is never returned: the audit event, written after, cannot inherit it.
//
// # No digest in an error
//
// The statements here carry digests, and the engine's errors can repeat what they
// were given. Every error a statement can produce goes through scrubbed before it
// leaves this file, so a caller may log one or show it without reading it first.

const (
	siteConcept    = "v1:platform:site"
	codeConcept    = "v1:platform:siteAccessCode"
	sessionConcept = "v1:platform:siteAccessSession"
)

// systemActorName is what this package calls itself to auth.SystemActor. It is what a
// row's createdBy says when the store updates one, by name, so a version the cluster
// wrote cannot be mistaken for one a person did.
const systemActorName = "siteaccess"

// Engine is the narrow engine surface this package needs.
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

// Code is one v1:platform:siteAccessCode as the access decision needs it. Its ids
// are BARE: the view hands them back canonical, and the handler compares
// AccountID with the site's, where one spelling on each side would read as a move.
type Code struct {
	ID, SiteID, AccountID, Gate, Subject string
	Generation                           int
	ExpiresAt                            time.Time
	Revoked                              bool
}

// Session is one v1:platform:siteAccessSession as the access decision needs it.
type Session struct {
	ID, SiteID, CodeID string
	Generation         int
	IssuedAt           time.Time
	Ended              bool
}

// Site is one v1:platform:site as the access decision needs it, read as the cluster. Its
// ids are BARE, for Code's reason. The omissions are the point, as they are on the
// handler's own projection: what nobody can see cannot leak into a reply.
type Site struct {
	ID, OwnerUserID, AccountID, PackageID, DeployableName string
}

// GraphStore reads and writes access codes through the engine.
type GraphStore struct{ engine Engine }

// NewGraphStore wraps an engine.
func NewGraphStore(engine Engine) *GraphStore { return &GraphStore{engine: engine} }

// asCluster is the context the store's own reads and updates run under: the cluster
// acting, with internal origin. See the file note for why a row's owner has no say.
//
// THE CALLER'S FORWARDED AUTHORITY STAYS BEHIND. A stream stamps it on the context
// (component/grpc/work_authority.go), and under a test persona it is an assertion the
// service actor cannot honour: the engine would refuse every one of these calls as an actor
// who differs from its session, and a persona could not so much as list a site with a code
// on it. So this call presents none, as library review history and the fleet catalog do.
//
// THE PERSONA IS STILL ASKED, HERE, BECAUSE NOTHING AFTER THIS WILL. The engine judges a
// session on the context it is handed, and this one no longer names it, so the session that
// authorized an act could end before the act's write and the write would land. Reads under
// the caller have already judged what the persona may reach. This judges only that it is
// still there.
func asCluster(ctx context.Context) (context.Context, error) {
	if err := auth.ValidateRoleTestContext(ctx); err != nil {
		return nil, fmt.Errorf("siteaccess: the test persona behind this call: %w", err)
	}
	blank := auth.ContextWithForwardedAuthority(ctx, auth.ForwardedAuthority{})
	return auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(blank, systemActorName)), nil
}

// readAsCluster is one of the store's own reads.
//
// THE ROWS AND NOTHING ELSE. Every shape these name projects the row's own fields, and an
// engine read otherwise expands each relationship the row has for nobody to look at: this
// is the path a visitor's status check takes every five seconds.
//
// THE PERSONA IS ASKED AGAIN WITH THE ANSWER IN HAND, for the precedents' reason
// (integrations/library/reviewstore): a session that ended while this ran was the session
// this was for, and it gets nothing.
func (s *GraphStore) readAsCluster(ctx context.Context, what, call string, rendered error) ([]map[string]any, error) {
	cluster, err := asCluster(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.read(memql.ContextWithRootsOnlyRead(cluster), what, call, rendered)
	if err != nil {
		return nil, err
	}
	if err := auth.ValidateRoleTestContext(ctx); err != nil {
		return nil, fmt.Errorf("siteaccess: the test persona behind this call: %w", err)
	}
	return rows, nil
}

// asClusterAgainst is the context one of the store's updates of a code runs under: the
// cluster acting, carrying the version of the row it was decided from.
func asClusterAgainst(ctx context.Context, codeID string, version time.Time) (context.Context, error) {
	cluster, err := asCluster(ctx)
	if err != nil {
		return nil, err
	}
	return witnessed(cluster, codeConcept, codeID, version)
}

// asOwner is the context the two creates run under, so the row they write is stamped
// with the site's owner.
//
// A BLANK OWNER ANSWERS FALSE, because auth.ContextWithUserActor leaves a context
// unchanged for one: the row would belong to whoever the inbound caller happened to be.
func asOwner(ctx context.Context, owner string) (context.Context, bool) {
	owner = memql.BareShortId(owner)
	if owner == "" {
		return ctx, false
	}
	return auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, owner)), true
}

// SiteForAccess reads a site as the cluster acting, for a request that has already
// presented a credential whose digest found a row. A miss is (nil, nil).
//
// IT AUTHORIZES NOTHING. What makes the row mean anything is the handler's comparison of
// the edge's stamp with what comes back, the owner and the account both. It is a read as
// the cluster because the other way of asking, under the stamped owner, costs dozens of
// statements resolving who that person is and what they belong to, and this is what a
// status check does every five seconds. A read under the owner still stands in front of
// a gated call, which is where that authority is actually spent.
//
// FRESH, because a move of the site's organization or a change of owner has to count from
// the next request and not from when a cache entry ages out (D11).
func (s *GraphStore) SiteForAccess(ctx context.Context, siteID string) (*Site, error) {
	if strings.TrimSpace(siteID) == "" {
		return nil, nil
	}
	call, err := langparser.RenderCall("siteById", map[string]any{"siteId": siteID})
	rows, err := s.readAsCluster(memql.ContextWithFreshRead(ctx), "the deployable", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	return &Site{
		ID:             memql.BareShortId(rowString(r, "id")),
		OwnerUserID:    memql.BareShortId(rowString(r, "ownerUserId")),
		AccountID:      memql.BareShortId(rowString(r, "accountId")),
		PackageID:      memql.BareShortId(rowString(r, "packageId")),
		DeployableName: strings.TrimSpace(rowString(r, "packageDeployableName")),
	}, nil
}

// PackageDomainsForAccess reads the DSL domains a site's package ships, as the cluster
// acting, for a gated call. NO ROW AND NO LIST ARE ONE ANSWER, no domains, which reaches
// no gate: a source no deploy has written a list for fails closed. A read that failed is
// an error and never looks empty. Fresh, for SiteForAccess's reason.
func (s *GraphStore) PackageDomainsForAccess(ctx context.Context, packageID string) ([]string, error) {
	if strings.TrimSpace(packageID) == "" {
		return nil, nil
	}
	call, err := langparser.RenderCall("packageById", map[string]any{"packageId": packageID})
	rows, err := s.readAsCluster(memql.ContextWithFreshRead(ctx), "the deployable's package", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rowStrings(rows[0], "dslDomains"), nil
}

// CodeByHash finds a code by the digest of what a visitor typed, on one site. A
// miss is (nil, nil), and that covers a code of another site. A removed or expired
// code still answers: which of those means `invalid` and which `expired` is a decision
// about the reply, and it is the handler's.
//
// IT CHECKS NO OWNER, and neither does anything else on this path: it reads as the
// cluster acting (see the file note), and it is the handler's comparison of the edge's
// stamp with the site's ownerUserId, made before this is called, that checks the owner.
func (s *GraphStore) CodeByHash(ctx context.Context, siteID, codeHash string) (*Code, error) {
	call, err := langparser.RenderCall("siteAccessCodeByHash", map[string]any{"siteId": siteID, "codeHash": codeHash})
	rows, err := s.readAsCluster(ctx, "a code by its digest", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	code := codeFromRow(rows[0]).Code
	return &code, nil
}

// CodeByID resolves the code a session was opened with. A miss is (nil, nil). It reads
// as the cluster acting, for CodeByHash's reason.
func (s *GraphStore) CodeByID(ctx context.Context, codeID string) (*Code, error) {
	call, err := langparser.RenderCall("siteAccessCodeById", map[string]any{"codeId": codeID})
	rows, err := s.readAsCluster(ctx, "the access code", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	code := codeFromRow(rows[0]).Code
	return &code, nil
}

// SessionByTokenHash finds a session by the digest of the cookie presented. A
// miss is (nil, nil). An ended session still answers, for the query's reason: a
// cookie with no row is `ended` too, and a visitor must not tell the two apart. It
// reads as the cluster acting, for CodeByHash's reason.
func (s *GraphStore) SessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	call, err := langparser.RenderCall("siteAccessSessionByToken", map[string]any{"tokenHash": tokenHash})
	rows, err := s.readAsCluster(ctx, "a session by its digest", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	return &Session{
		ID:         memql.BareShortId(rowString(r, "id")),
		SiteID:     memql.BareShortId(rowString(r, "siteId")),
		CodeID:     memql.BareShortId(rowString(r, "codeId")),
		Generation: rowInt(r, "generation"),
		IssuedAt:   rowTime(r, "issuedAt"),
		Ended:      rowStamp(r, "endedAt") != "",
	}, nil
}

// CreateSession writes a session opened with a code, as the site's owner, so the row
// is stamped with them, and gives it an id when the caller left it without one.
//
// `issuedAt` IS WRITTEN IN ONE FORM, WHATEVER IT WAS GIVEN: UTC RFC 3339 at whole
// seconds. siteAccessSessionLatestForCode orders sessions by the TEXT of this
// field and the engine stores the value as given, so `...:20Z` sorts after
// `...:20.5Z` and an offset sorts by its digits, and an older session would read
// as the newest. The DSL cannot say so (@pattern takes strings only), so this is
// where it is enforced, rather than a thing every caller has to know. A zero time
// is refused, not written as the year 1: a session counts its 24 hours from it.
func (s *GraphStore) CreateSession(ctx context.Context, owner string, in Session, tokenHash string) error {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return errors.New("siteaccess: a session belongs to the site's owner, and this site has none")
	}
	if in.IssuedAt.IsZero() {
		return errors.New("siteaccess: a session needs the time it was opened")
	}
	sessionID := strings.TrimSpace(in.ID)
	if sessionID == "" {
		sessionID = id.NewShortId()
	}
	call, err := langparser.RenderCall("createSiteAccessSession", map[string]any{
		"sessionId":  sessionID,
		"siteId":     in.SiteID,
		"codeId":     in.CodeID,
		"generation": in.Generation,
		"tokenHash":  tokenHash,
		"issuedAt":   wholeSecond(in.IssuedAt),
	})
	return s.write(owned, "the session", call, err)
}

// EndSession marks a session as left, as the cluster acting: the row keeps the owner it
// was stamped with. Ending one that has already ended writes the time again, which
// nothing reads as a change.
func (s *GraphStore) EndSession(ctx context.Context, sessionID string, at time.Time) error {
	cluster, err := asCluster(ctx)
	if err != nil {
		return err
	}
	call, err := langparser.RenderCall("endSiteAccessSession", map[string]any{
		"sessionId": sessionID,
		"endedAt":   wholeSecond(at),
	})
	return s.write(cluster, "the end of the session", call, err)
}

// ---------------------------------------------------------------------------
// what the operator handlers read
// ---------------------------------------------------------------------------

// siteView is the projection of v1:platform:site a handler judges an act by.
type siteView struct {
	ID             string
	OwnerUserID    string
	AccountID      string
	PackageID      string
	DeployableName string
	SystemOwned    bool
	// Version is the row's createdAt, the time of its latest write: the witness a
	// mint carries, so a site that moved to another organization since this read
	// cannot end up with a code copying the one it left (D15).
	Version time.Time
}

// codeView is a Code with what only the operator's surface reads: the row's own
// words, as they were stored, and the version an act decides from.
type codeView struct {
	Code
	Label     string
	Hint      string
	IssuedAt  string
	ExpiresOn string // expiresAt as stored; Code.ExpiresAt is the same value parsed
	RevokedAt string
	Version   time.Time
}

func codeFromRow(r map[string]any) codeView {
	revokedAt := rowStamp(r, "revokedAt")
	return codeView{
		Code: Code{
			ID:         memql.BareShortId(rowString(r, "id")),
			SiteID:     memql.BareShortId(rowString(r, "siteId")),
			AccountID:  memql.BareShortId(rowString(r, "accountId")),
			Gate:       rowString(r, "gate"),
			Subject:    rowString(r, "subject"),
			Generation: rowInt(r, "generation"),
			ExpiresAt:  rowTime(r, "expiresAt"),
			Revoked:    revokedAt != "",
		},
		Label:     rowString(r, "label"),
		Hint:      rowString(r, "codeHint"),
		IssuedAt:  rowString(r, "issuedAt"),
		ExpiresOn: rowString(r, "expiresAt"),
		RevokedAt: revokedAt,
		Version:   rowTime(r, "createdAt"),
	}
}

// siteForCaller resolves a deployable UNDER THE CALLER. A miss is (siteView{},
// false, nil): a site this caller cannot read and a site that does not exist are
// the same answer, because telling them apart would tell a caller which ids exist.
//
// FRESH, because the row's version is the witness a mint writes against, and a
// cached row with an old version loses the fence every time, retry included.
func (s *GraphStore) siteForCaller(ctx context.Context, siteID string) (siteView, bool, error) {
	call, err := langparser.RenderCall("siteById", map[string]any{"siteId": siteID})
	rows, err := s.read(memql.ContextWithFreshRead(ctx), "the deployable", call, err)
	if err != nil || len(rows) == 0 {
		return siteView{}, false, err
	}
	r := rows[0]
	return siteView{
		ID:             memql.BareShortId(rowString(r, "id")),
		OwnerUserID:    memql.BareShortId(rowString(r, "ownerUserId")),
		AccountID:      memql.BareShortId(rowString(r, "accountId")),
		PackageID:      memql.BareShortId(rowString(r, "packageId")),
		DeployableName: strings.TrimSpace(rowString(r, "packageDeployableName")),
		SystemOwned:    rowBool(r, "systemOwned"),
		Version:        rowTime(r, "createdAt"),
	}, true, nil
}

// packageDomains reads the DSL domains a site's package ships, UNDER THE CALLER.
//
// A PACKAGE THAT CANNOT BE READ AND ONE WITH NO LIST ARE ONE ANSWER, no domains,
// which reaches no gates: a source that no deploy has written a list for reads the
// same as one the caller may not see, and both fail closed. An engine failure is
// not that answer, and comes back as an error: a failed read must not look empty.
func (s *GraphStore) packageDomains(ctx context.Context, packageID string) ([]string, error) {
	if strings.TrimSpace(packageID) == "" {
		return nil, nil
	}
	call, err := langparser.RenderCall("packageById", map[string]any{"packageId": packageID})
	rows, err := s.read(memql.ContextWithFreshRead(ctx), "the deployable's package", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rowStrings(rows[0], "dslDomains"), nil
}

// codeForCaller resolves one code with internal origin, under the caller's actor.
// A miss is (codeView{}, false, nil), for siteForCaller's reason.
func (s *GraphStore) codeForCaller(ctx context.Context, codeID string) (codeView, bool, error) {
	call, err := langparser.RenderCall("siteAccessCodeById", map[string]any{"codeId": codeID})
	rows, err := s.read(auth.ContextWithInternalOrigin(ctx), "the access code", call, err)
	if err != nil || len(rows) == 0 {
		return codeView{}, false, err
	}
	return codeFromRow(rows[0]), true, nil
}

// codesForSite lists every code of a site with internal origin, under the caller's
// actor, ended ones included. The tier decides each row.
func (s *GraphStore) codesForSite(ctx context.Context, siteID string) ([]codeView, error) {
	call, err := langparser.RenderCall("siteAccessCodeRowsForSite", map[string]any{"siteId": siteID})
	rows, err := s.read(auth.ContextWithInternalOrigin(ctx), "the deployable's access codes", call, err)
	if err != nil {
		return nil, err
	}
	out := make([]codeView, 0, len(rows))
	for _, r := range rows {
		out = append(out, codeFromRow(r))
	}
	return out, nil
}

// lastOpened is when a generation of a code was last opened, as the text the session
// stored, or "" when nobody has opened it. Read AS THE CLUSTER ACTING: the session's tier
// has no account arm, so it would answer nothing to a member of the site's organization,
// and a session keeps the owner it was stamped with, so it would answer nothing to the
// site's current owner either once the site had moved on. The list in front of this has
// already authorized the person asking, under the caller, and this says only when.
func (s *GraphStore) lastOpened(ctx context.Context, codeID string, generation int) (string, error) {
	call, err := langparser.RenderCall("siteAccessSessionLatestForCode", map[string]any{"codeId": codeID, "generation": generation})
	rows, err := s.readAsCluster(ctx, "when the code was last opened", call, err)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rowString(rows[0], "issuedAt"), nil
}

// ---------------------------------------------------------------------------
// the writes
// ---------------------------------------------------------------------------

// witnessed adds to a write's context the version of the row it was decided from.
//
// A MISSING VERSION FAILS HERE RATHER THAN BECOMING A ZERO WITNESS, which means
// something else: a zero version requires the row to be ABSENT, so it would refuse
// the write as a conflict, and a person would be told to try again for ever.
func witnessed(ctx context.Context, concept, rowID string, version time.Time) (context.Context, error) {
	if version.IsZero() {
		return nil, fmt.Errorf("siteaccess: the read of %s carried no version to write against", concept)
	}
	return memql.ContextWithRowVersionFence(ctx, concept, rowID, version), nil
}

// newCode is what a mint writes. The digest and the hint are all that is left of
// the code by the time it gets here: MintAccessCode hands them back together, and
// no shape of this call can store the plain value.
type newCode struct {
	ID, SiteID, AccountID, Label, Gate, Subject string
	Digest, Hint, ChangedBy                     string
	IssuedAt, ExpiresAt                         time.Time
}

// createCode writes a minted code as the site's owner, with a witness of the SITE:
// the account and the owner on this row were copied from the version of the site
// the handler read, and a move that landed since has to refuse this write.
func (s *GraphStore) createCode(ctx context.Context, owner string, c newCode, siteVersion time.Time) error {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return errors.New("siteaccess: a code belongs to the site's owner, and this site has none")
	}
	guarded, err := witnessed(owned, siteConcept, c.SiteID, siteVersion)
	if err != nil {
		return err
	}
	args := map[string]any{
		"codeId":        c.ID,
		"siteId":        c.SiteID,
		"label":         c.Label,
		"gate":          c.Gate,
		"codeHash":      c.Digest,
		"codeHint":      c.Hint,
		"issuedAt":      wholeSecond(c.IssuedAt),
		"expiresAt":     wholeSecond(c.ExpiresAt),
		"lastChangedBy": c.ChangedBy,
	}
	// Absent rather than empty: a code of a site with no organization carries no
	// copy, and an empty string would be a relationship to nothing.
	if c.AccountID != "" {
		args["accountId"] = c.AccountID
	}
	if c.Subject != "" {
		args["subject"] = c.Subject
	}
	call, err := langparser.RenderCall("createSiteAccessCode", args)
	return s.write(guarded, "the code", call, err)
}

// rotation is what a regenerate writes.
type rotation struct {
	CodeID, Digest, Hint, ChangedBy string
	Generation                      int
	IssuedAt, ExpiresAt             time.Time
}

// rotateCode replaces a code's secret, as the cluster acting, against the version it
// was decided from. The row keeps the owner it was stamped with.
func (s *GraphStore) rotateCode(ctx context.Context, r rotation, version time.Time) error {
	guarded, err := asClusterAgainst(ctx, r.CodeID, version)
	if err != nil {
		return err
	}
	call, err := langparser.RenderCall("rotateSiteAccessCode", map[string]any{
		"codeId":        r.CodeID,
		"codeHash":      r.Digest,
		"codeHint":      r.Hint,
		"generation":    r.Generation,
		"issuedAt":      wholeSecond(r.IssuedAt),
		"expiresAt":     wholeSecond(r.ExpiresAt),
		"lastChangedBy": r.ChangedBy,
	})
	return s.write(guarded, "the regenerated code", call, err)
}

// setCodeExpiry moves a code's expiry, as rotateCode writes.
func (s *GraphStore) setCodeExpiry(ctx context.Context, codeID string, expiresAt time.Time, changedBy string, version time.Time) error {
	guarded, err := asClusterAgainst(ctx, codeID, version)
	if err != nil {
		return err
	}
	call, err := langparser.RenderCall("setSiteAccessCodeExpiry", map[string]any{
		"codeId":        codeID,
		"expiresAt":     wholeSecond(expiresAt),
		"lastChangedBy": changedBy,
	})
	return s.write(guarded, "the new expiry", call, err)
}

// removeCode ends a code, as rotateCode writes.
func (s *GraphStore) removeCode(ctx context.Context, codeID string, at time.Time, changedBy string, version time.Time) error {
	guarded, err := asClusterAgainst(ctx, codeID, version)
	if err != nil {
		return err
	}
	call, err := langparser.RenderCall("removeSiteAccessCode", map[string]any{
		"codeId":        codeID,
		"revokedAt":     wholeSecond(at),
		"lastChangedBy": changedBy,
	})
	return s.write(guarded, "the removal", call, err)
}

// auditEntry is one decision for the audit log. `detail` never holds a code or a
// digest: the log outlives the code and is read by more people than the row is.
type auditEntry struct {
	Action     string
	CodeID     string
	ActorID    string
	ActorEmail string
	ActorRole  string
	Detail     map[string]any
}

// writeAudit writes one v1:identity:auditEvent, on the caller's own context with
// internal origin stamped for this one write: auditEvent's create admits a cluster
// owner or server code (memql#5624), and a developer minting a code is neither. It
// runs as the caller and not as the site's owner, because this row is the record of
// who asked, and its createdBy should say what its actorUserId says.
func (s *GraphStore) writeAudit(ctx context.Context, entry auditEntry) error {
	now := time.Now().UTC()
	call, err := langparser.RenderCall("createAuditEvent", map[string]any{
		"eventId":     "site-access-" + id.NewShortId(),
		"occurredAt":  now.Format(time.RFC3339Nano),
		"category":    "authorization",
		"action":      entry.Action,
		"actorUserId": entry.ActorID,
		"actorEmail":  entry.ActorEmail,
		"actorRole":   entry.ActorRole,
		"targetType":  "siteAccessCode",
		"targetId":    entry.CodeID,
		"outcome":     "success",
		"detail":      entry.Detail,
	})
	return s.write(auth.ContextWithInternalOrigin(ctx), "the audit event", call, err)
}

// read runs one rendered query. Every read and write here leaves through this or
// write, which is what makes "no digest in an error" a property of the file rather
// than something each call has to remember. `rendered` is RenderCall's own error,
// taken here so each caller says it once.
func (s *GraphStore) read(ctx context.Context, what, call string, rendered error) ([]map[string]any, error) {
	if rendered != nil {
		return nil, fmt.Errorf("siteaccess: render the read of %s: %w", what, scrubbed(rendered))
	}
	if s == nil || s.engine == nil {
		return nil, errNoEngine
	}
	res, err := s.engine.Execute(ctx, "query "+call)
	if err != nil {
		return nil, fmt.Errorf("siteaccess: read %s: %w", what, scrubbed(err))
	}
	return memql.MaterializeRows(res), nil
}

// write runs one rendered mutation, for read's reason.
func (s *GraphStore) write(ctx context.Context, what, call string, rendered error) error {
	if rendered != nil {
		return fmt.Errorf("siteaccess: render the write of %s: %w", what, scrubbed(rendered))
	}
	if s == nil || s.engine == nil {
		return errNoEngine
	}
	if _, err := s.engine.Execute(ctx, "mutation "+call); err != nil {
		return fmt.Errorf("siteaccess: write %s: %w", what, scrubbed(err))
	}
	return nil
}

var errNoEngine = errors.New("siteaccess: no engine")

// ---------------------------------------------------------------------------
// keeping digests out of errors
// ---------------------------------------------------------------------------

// digestRun is what a stored digest, or the front of one, looks like in text: a
// long run of lowercase hex. The plain code and the plain session token never
// reach the engine, so a digest is the only secret its errors can repeat, and an
// id is never caught by this: a short id is a UUID, whose longest run is 12.
var digestRun = regexp.MustCompile(`[0-9a-f]{16,}`)

// scrubbedError is an engine error with any digest taken out of its words. It
// keeps the error's identity, so errors.Is still finds a lost fence behind it.
type scrubbedError struct {
	text  string
	cause error
}

func (e *scrubbedError) Error() string { return e.text }
func (e *scrubbedError) Unwrap() error { return e.cause }

// scrubbed is applied at the one place an engine error enters this package's own
// errors: a statement here may have carried a digest, and an error may repeat its
// statement. An error with nothing to take out comes back as it was.
func scrubbed(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	clean := digestRun.ReplaceAllString(text, "[digest]")
	if clean == text {
		return err
	}
	return &scrubbedError{text: clean, cause: err}
}

// ---------------------------------------------------------------------------
// reading rows
// ---------------------------------------------------------------------------

// wholeSecond is how a datetime is written here: UTC RFC 3339 at whole seconds.
// See CreateSession for why a session has to be; a code's stamps follow it so that
// there is one form in the rows rather than two.
func wholeSecond(at time.Time) string {
	return at.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// rowTime reads a stamp, answering the zero time for anything else.
//
// ABSENT AND UNPARSEABLE ARE ONE ANSWER and callers test IsZero, because a code
// with no readable expiry must be treated as expired rather than as eternal -- fail
// toward the refusal. A shaped `row.createdAt` arrives as a time.Time and a stored
// datetime as text, so both are read.
func rowTime(m map[string]any, key string) time.Time {
	switch v := m[key].(type) {
	case time.Time:
		return v.UTC()
	case string:
		at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(v))
		if err != nil {
			return time.Time{}
		}
		return at.UTC()
	default:
		return time.Time{}
	}
}

// rowStamp reads a field whose being set is what matters, a removal or an end, as text:
// "" when nothing is there, and something when anything is.
//
// ANY PRESENT VALUE COUNTS, because the alternative fails open. A removed code and an
// ended session must not read as live for arriving in a type the reader did not expect,
// and a shaped datetime can arrive as a time.Time as well as the text that was stored
// (rowTime reads both). A value that is neither is still a value someone wrote there, so
// it is printed rather than dropped: the row is treated as ended rather than as eternal.
func rowStamp(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case nil:
		return ""
	case string:
		return v
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(v)
	}
}

func rowString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func rowBool(m map[string]any, key string) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

// rowInt reads a stored integer, whichever way the read carried it: an in-process
// row holds a decoded JSON number, and the wire carries large integers as strings.
//
// A VALUE THAT DOES NOT FIT READS AS ZERO, the same as an absent one. The one field
// read here is a generation, which starts at 1, so zero matches no real code's: a
// garbled number ends a session rather than honouring it. A saturated one could match.
func rowInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return num.Int64OrZero(v)
	case float64:
		return num.Float64OrZero(v)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

func rowStrings(m map[string]any, key string) []string {
	switch v := m[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
