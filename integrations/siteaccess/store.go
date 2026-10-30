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
)

// store.go -- the graph seam. Every read and write is a NAMED construct rendered
// as MemQL text and handed to the engine, the shape integrations/sitepreview and
// integrations/customdomain use. Nothing here reaches the database.
//
// # Three actors, and which is which is the whole of the authorization story
//
// THE SITE AND ITS PACKAGE ARE READ UNDER THE CALLER, with no stamp. That is what
// makes them authorization checks rather than lookups: siteById and packageById
// carry the composite tier and the organization conjunct, so a caller who may not
// see either resolves zero rows and is refused before anything is minted. A stamp
// would hand a read that IS the check the engine's escape for trusted Go, which
// is integrations/customdomain's reason exactly.
//
// A CODE'S ROWS ARE READ WITH INTERNAL ORIGIN, UNDER THE CALLER. The stamp is not
// optional here: the concept is unreadable by any client whatever the tier says
// (clientUnreadableConcepts, component/memql), so an unstamped read answers
// nothing for everybody, and nothing looks wrong but an empty panel. The actor is
// left exactly as it was, so the composite tier still decides every row, and the
// builtin in front of this read has already passed the part and the organization
// gate.
//
// THE WRITES RUN AS THE SITE'S OWNER, with internal origin, and so do the session
// reads. A row's owner is the actor that wrote it, and it has to be the site's
// rather than the operator's, because the bff finds the row again under the owner
// the edge stamped on a request. The person who asked is recorded separately, in
// `lastChangedBy` and the audit event. A session's tier has no account arm, so an
// operator who only belongs to the site's organization is not admitted to one, and
// reading "last opened" as them would answer nothing for every code they manage.
//
// The exported half below is what the bff's handler reads through, with no caller
// at all: a visitor has no identity to read as. Those functions take the owner the
// edge stamped, and the handler has already compared it with the site's own.
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

// GraphStore reads and writes access codes through the engine.
type GraphStore struct{ engine Engine }

// NewGraphStore wraps an engine.
func NewGraphStore(engine Engine) *GraphStore { return &GraphStore{engine: engine} }

// asOwner is the context every read and write made for a site's owner runs under.
//
// A BLANK OWNER ANSWERS FALSE, because auth.ContextWithUserActor leaves a context
// unchanged for one: the work would run as whoever the inbound caller happened to
// be. Each caller decides what no owner means to it, and the two answers differ --
// a read misses, a write fails.
func asOwner(ctx context.Context, owner string) (context.Context, bool) {
	owner = memql.BareShortId(owner)
	if owner == "" {
		return ctx, false
	}
	return auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, owner)), true
}

// CodeByHash finds a code by the digest of what a visitor typed, on one site. A
// miss is (nil, nil), and that covers a code of another site and a blank owner. A
// removed or expired code still answers: which of those means `invalid` and which
// `expired` is a decision about the reply, and it is the handler's.
func (s *GraphStore) CodeByHash(ctx context.Context, owner, siteID, codeHash string) (*Code, error) {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return nil, nil
	}
	call, err := langparser.RenderCall("siteAccessCodeByHash", map[string]any{"siteId": siteID, "codeHash": codeHash})
	rows, err := s.read(owned, "a code by its digest", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	code := codeFromRow(rows[0]).Code
	return &code, nil
}

// CodeByID resolves the code a session was opened with. A miss is (nil, nil).
func (s *GraphStore) CodeByID(ctx context.Context, owner, codeID string) (*Code, error) {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return nil, nil
	}
	view, found, err := s.codeByID(owned, codeID)
	if err != nil || !found {
		return nil, err
	}
	return &view.Code, nil
}

// SessionByTokenHash finds a session by the digest of the cookie presented. A
// miss is (nil, nil). An ended session still answers, for the query's reason: a
// cookie with no row is `ended` too, and a visitor must not tell the two apart.
func (s *GraphStore) SessionByTokenHash(ctx context.Context, owner, tokenHash string) (*Session, error) {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return nil, nil
	}
	call, err := langparser.RenderCall("siteAccessSessionByToken", map[string]any{"tokenHash": tokenHash})
	rows, err := s.read(owned, "a session by its digest", call, err)
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
		Ended:      rowString(r, "endedAt") != "",
	}, nil
}

// CreateSession writes a session opened with a code, as the site's owner, and
// gives it an id when the caller left it without one.
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

// EndSession marks a session as left, as the site's owner. Ending one that has
// already ended writes the time again, which nothing reads as a change.
func (s *GraphStore) EndSession(ctx context.Context, owner, sessionID string, at time.Time) error {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return errors.New("siteaccess: a session belongs to the site's owner, and this site has none")
	}
	call, err := langparser.RenderCall("endSiteAccessSession", map[string]any{
		"sessionId": sessionID,
		"endedAt":   wholeSecond(at),
	})
	return s.write(owned, "the end of the session", call, err)
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
	revokedAt := rowString(r, "revokedAt")
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
	return s.codeByID(auth.ContextWithInternalOrigin(ctx), codeID)
}

// codeByID is the one read of a code by id: which actor it runs as is its caller's.
func (s *GraphStore) codeByID(ctx context.Context, codeID string) (codeView, bool, error) {
	call, err := langparser.RenderCall("siteAccessCodeById", map[string]any{"codeId": codeID})
	rows, err := s.read(ctx, "the access code", call, err)
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

// lastOpened is when a generation of a code was last opened, as the text the
// session stored, or "" when nobody has opened it. Read AS THE SITE'S OWNER, the
// actor the sessions are written as: their tier has no account arm.
func (s *GraphStore) lastOpened(ctx context.Context, owner, codeID string, generation int) (string, error) {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return "", nil
	}
	call, err := langparser.RenderCall("siteAccessSessionLatestForCode", map[string]any{"codeId": codeID, "generation": generation})
	rows, err := s.read(owned, "when the code was last opened", call, err)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rowString(rows[0], "issuedAt"), nil
}

// ---------------------------------------------------------------------------
// the writes
// ---------------------------------------------------------------------------

// fenced is the context one write of a code runs under: the site's owner, internal
// origin, and the version of the row it was decided from.
//
// A MISSING VERSION FAILS HERE RATHER THAN BECOMING A ZERO WITNESS, which means
// something else: a zero version requires the row to be ABSENT, so it would refuse
// the write as a conflict, and a person would be told to try again for ever.
func fenced(ctx context.Context, owner, concept, rowID string, version time.Time) (context.Context, error) {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return nil, errors.New("siteaccess: this write runs as the site's owner, and this site has none")
	}
	if version.IsZero() {
		return nil, fmt.Errorf("siteaccess: the read of %s carried no version to write against", concept)
	}
	return memql.ContextWithRowVersionFence(owned, concept, rowID, version), nil
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
	guarded, err := fenced(ctx, owner, siteConcept, c.SiteID, siteVersion)
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

// rotateCode replaces a code's secret, against the version it was decided from.
func (s *GraphStore) rotateCode(ctx context.Context, owner string, r rotation, version time.Time) error {
	guarded, err := fenced(ctx, owner, codeConcept, r.CodeID, version)
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

// setCodeExpiry moves a code's expiry, against the version it was decided from.
func (s *GraphStore) setCodeExpiry(ctx context.Context, owner, codeID string, expiresAt time.Time, changedBy string, version time.Time) error {
	guarded, err := fenced(ctx, owner, codeConcept, codeID, version)
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

// removeCode ends a code, against the version it was decided from.
func (s *GraphStore) removeCode(ctx context.Context, owner, codeID string, at time.Time, changedBy string, version time.Time) error {
	guarded, err := fenced(ctx, owner, codeConcept, codeID, version)
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
func rowInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float64:
		return int(v)
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
