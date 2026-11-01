package siteaccess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// integration.go -- the six DSL-callable capabilities:
//
//	integration.siteAccess.create      mint a code             (dsl builtin siteAccessCodeCreate)
//	integration.siteAccess.regenerate  replace its secret      (dsl builtin siteAccessCodeRegenerate)
//	integration.siteAccess.setExpiry   move its expiry         (dsl builtin siteAccessCodeSetExpiry)
//	integration.siteAccess.remove      end it                  (dsl builtin siteAccessCodeRemove)
//	integration.siteAccess.gates       the gates a site opens  (dsl builtin siteAccessGates)
//	integration.siteAccess.list        the codes of a site     (dsl builtin siteAccessCodesForSite)
//
// Every client-callable read and write on access codes is one of these, passing the shared
// organization gate; the queries and mutations beneath them are @serverOnly. The list is a builtin,
// and reads its rows through the @serverOnly siteAccessCodeRowsForSite with internal origin and the
// caller's actor untouched, because that gate judges builtins and mutations and never a query. The
// rows are readable by no client any other way (clientUnreadableConcepts, component/memql), so these
// handlers and the bff are the readers.
//
// # The dispatch checks nothing about the arguments
//
// A builtin's declared arguments are documentation to its caller. The dispatch hands a handler what
// the call said, so a `datetime!` can arrive as a word or not at all, and every value is checked
// here before anything rests on it.
//
// # A code is not minted inside a run
//
// The plain code exists in the reply and nowhere else, and inside a run the reply is not
// the end of it: the journal writes what a statement returned into v1:work:step
// (result.value.code, result.result.*.payload.code) and v1:work:run
// (outcome.returned.code), durable rows anybody who may read the run can read. A logic that
// regenerates a client's code and emails it is the natural thing for a pack to write, so
// minting and regenerating refuse a run rather than leaving that to its author to know.
//
// A RUN IS THE RIGHT TEST. Every step that journals carries one, because the executor
// stamps it on the step's context (withRunContext, component/automations), a logic's
// statements included. A dry run does not stamp one, and it journals nothing and refuses
// every integration executor (CheckBuiltinPreview). A person's own call is not in one: the
// stream builds its context from the request (component/grpc, handleExecuteQuery) and
// stamps no run, and the top-level builtin branch of executeWith adds none. The act of
// re-timing or removing a code carries no code, so a run may do either.
//
// # A refusal is a sentence
//
// The OS shows what a builtin refuses with, as written (the SDK's refusalSentence takes off the
// frames the wire puts around it, and a sentence has none of its own to lose). So a refusal here
// opens with a capital, says what is wrong and says what to do, and carries neither the construct's
// name nor this package's. That goes for every error a handler returns: a failure a person cannot
// act on is still worded for them (see failed). compose's errFolderUnavailable is the precedent.

// resultConcept is the synthetic MemoryNode concept these capabilities return: an in-flight
// integration result, never persisted.
const resultConcept = "integration:siteAccess:result"

// The audit vocabulary. `siteAccessCode` is declared on v1:identity:auditEvent's targetType enum and
// on createAuditEvent's args.
const (
	auditActionCreated       = "site_access_code_created"
	auditActionRegenerated   = "site_access_code_regenerated"
	auditActionExpiryChanged = "site_access_code_expiry_changed"
	auditActionRemoved       = "site_access_code_removed"
)

const (
	// maxLabelLength and maxSubjectLength are counted in characters a person sees,
	// so a name in another alphabet is not held to a shorter limit than one in ASCII.
	maxLabelLength   = 80
	maxSubjectLength = 120

	// auditWriteTimeout bounds the one write that outlives its request.
	auditWriteTimeout = 5 * time.Second
)

// Integration exposes the access code capabilities.
type Integration struct {
	store  *GraphStore
	logger *slog.Logger
}

// NewIntegration builds the integration over an engine.
func NewIntegration(engine Engine, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{store: NewGraphStore(engine), logger: logger}
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "siteAccess" }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name: "create",
			Description: "Mint an access code that opens one gate of a deployable. " +
				"Returns the plain code once; only its SHA-256 is stored.",
			Handler: i.handleCreate,
			ArgsSchema: map[string]string{
				"siteId":    "string (required) -- the deployable the code opens a part of.",
				"label":     "string (required) -- the operator's name for the code, 1 to 80 characters.",
				"gate":      "string (required) -- the gate the code opens.",
				"subject":   "string (optional) -- scope within the gate, at most 120 characters.",
				"expiresAt": "datetime (required) -- when the code stops opening the site.",
			},
		},
		{
			Name: "regenerate",
			Description: "Replace an access code's secret, keeping its id, label, gate and subject. " +
				"Ends every session opened with the old secret. Returns the plain code once.",
			Handler: i.handleRegenerate,
			ArgsSchema: map[string]string{
				"codeId":    "string (required) -- the access code to regenerate.",
				"expiresAt": "datetime (optional) -- a new expiry; omitted keeps the current one.",
			},
		},
		{
			Name: "setExpiry",
			Description: "Change when an access code stops opening the site, without changing the code. " +
				"Extending an expired code revives it.",
			Handler: i.handleSetExpiry,
			ArgsSchema: map[string]string{
				"codeId":    "string (required) -- the access code to re-time.",
				"expiresAt": "datetime (required) -- the new expiry.",
			},
		},
		{
			Name:        "remove",
			Description: "End an access code. A soft end: the row stays as the record that the code existed.",
			Handler:     i.handleRemove,
			ArgsSchema: map[string]string{
				"codeId": "string (required) -- the access code to end.",
			},
		},
		{
			Name: "gates",
			Description: "The gates a deployable declares with @accessGate, and whether each reads the " +
				"accessSubject argument. One row per gate.",
			Handler: i.handleGates,
			ArgsSchema: map[string]string{
				"siteId": "string (required) -- the deployable to list the gates of.",
			},
		},
		{
			Name: "list",
			Description: "The access codes minted against a deployable, removed and expired ones included, one row per " +
				"code and never the code or its digest, each with lastOpenedAt taken from the newest session of its " +
				"current generation when there is one. No order is promised.",
			Handler: i.handleList,
			ArgsSchema: map[string]string{
				"siteId": "string (required) -- the deployable to list the codes of.",
			},
		},
	}
}

// ---------------------------------------------------------------------------
// minting
// ---------------------------------------------------------------------------

// minted is what a successful mint or regenerate hands back: the plain code, once,
// and what the reply and the audit event say about the row.
type minted struct {
	CodeID, Code, SiteID, Gate string
	Generation                 int
	ExpiresAt                  time.Time
}

// handleCreate mints a code.
//
// EVERY GATE RUNS BEFORE THE CODE IS MINTED, in the order a person would ask them:
// is there a deployable I can see, is it one a code can belong to, does it declare
// the gate, and are the name, the subject and the expiry inside their bounds. A
// refusal at any of them has written nothing and minted nothing.
//
// THE WHOLE OF THAT IS RUN AGAIN IF THE WRITE LOSES THE FENCE. The witness is the
// site's, so a loss means the site changed, and what changed may be the owner, the
// organization or the package this mint copied from or was judged against.
func (i *Integration) handleCreate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := refuseInARun(ctx, "Add it there."); err != nil {
		return nil, err
	}
	actor, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	siteID := memql.BareShortId(asString(args["siteId"]))
	if siteID == "" {
		return nil, refuse("Choose a deployable to add the access code to.")
	}
	var out minted
	err = onceMore(func() (err error) {
		out, err = i.mint(ctx, actor, siteID, args)
		return err
	})
	if err != nil {
		return nil, i.failed("create", siteID, err,
			"This deployable changed at the same moment, so no access code was added. Reload the list and try again.")
	}
	i.audit(ctx, auditActionCreated, out.CodeID, map[string]any{
		"act": "create", "siteId": out.SiteID, "gate": out.Gate, "generation": out.Generation, "expiresAt": wholeSecond(out.ExpiresAt),
	})
	return i.node("create:"+out.CodeID, map[string]any{
		"codeId":    out.CodeID,
		"expiresAt": wholeSecond(out.ExpiresAt),
		// THE ONE TIME THE CODE IS READABLE. It is in this reply and in whatever the
		// operator pastes it into, and nowhere else ever again.
		"code": out.Code,
	})
}

func (i *Integration) mint(ctx context.Context, actor, siteID string, args map[string]any) (minted, error) {
	site, ok, err := i.store.siteForCaller(ctx, siteID)
	if err != nil {
		return minted{}, err
	}
	if !ok {
		return minted{}, errSiteUnreadable()
	}
	// A SYSTEM-OWNED SITE IS ASKED ABOUT FIRST, though it usually has no owner either,
	// because that is the reason worth saying: it is exempt from the whole axis, as it
	// is from preview, and no owner it could be given would change the answer.
	if site.SystemOwned {
		return minted{}, refuse("This deployable is the surface the cluster is managed through, so it cannot have access codes. Add them to one of your own deployables.")
	}
	if site.OwnerUserID == "" {
		return minted{}, refuse("This deployable has no owner, so it cannot have access codes. Give it an owner, then add the code.")
	}
	// A TEST PERSONA CAN ONLY MINT WHAT IS ITS OWN. The row is stamped with the site's
	// owner by running as them, and a persona may not borrow anybody else
	// (auth.ContextWithUserActor), so the engine would refuse the write whatever came next,
	// and the sentence for that says to try again. The acts on a code that exists do not
	// borrow anybody, so this one is all a persona loses against the role it stands in for.
	if auth.IsRoleTestContext(ctx) && actor != site.OwnerUserID {
		return minted{}, refuse("A test persona can only add an access code to a deployable it owns, because the code belongs to the deployable's owner. Add it as yourself.")
	}
	if site.DeployableName == "" {
		return minted{}, refuse("This deployable was not deployed from a package, so it has no gates for a code to open. Deploy it from a source that declares a gate with @accessGate, then add the code.")
	}
	gates, err := i.gatesOf(ctx, site)
	if err != nil {
		return minted{}, err
	}
	if len(gates) == 0 {
		return minted{}, refuse("This deployable declares no gates yet, or has not been deployed since access codes were added. Declare a gate with @accessGate and deploy it, then add the code.")
	}
	gate := strings.TrimSpace(asString(args["gate"]))
	if !declaresGate(gates, gate) {
		return minted{}, refuse("Choose a gate this deployable declares: %s.", gateNames(gates))
	}
	label := strings.TrimSpace(asString(args["label"]))
	if label == "" {
		return minted{}, refuse("Enter a name for the access code.")
	}
	if utf8.RuneCountInString(label) > maxLabelLength {
		return minted{}, refuse("Shorten the name to %d characters or fewer.", maxLabelLength)
	}
	subject := strings.TrimSpace(asString(args["subject"]))
	if utf8.RuneCountInString(subject) > maxSubjectLength {
		return minted{}, refuse("Shorten the subject to %d characters or fewer.", maxSubjectLength)
	}
	expiresAt, given, err := expiryArgument(args)
	if err != nil {
		return minted{}, err
	}
	if !given {
		return minted{}, refuse("Choose when the access code should expire.")
	}
	now := time.Now().UTC()
	if err := checkExpiry(now, expiresAt); err != nil {
		return minted{}, err
	}

	code, digest, hint, err := memql.MintAccessCode()
	if err != nil {
		return minted{}, err
	}
	row := newCode{
		ID:        id.NewShortId(),
		SiteID:    site.ID,
		AccountID: site.AccountID,
		Label:     label,
		Gate:      gate,
		Subject:   subject,
		Digest:    digest,
		Hint:      hint,
		ChangedBy: actor,
		IssuedAt:  now,
		ExpiresAt: expiresAt,
	}
	if err := i.store.createCode(ctx, site.OwnerUserID, row, site.Version); err != nil {
		return minted{}, err
	}
	return minted{CodeID: row.ID, Code: code, SiteID: site.ID, Gate: gate, Generation: 1, ExpiresAt: expiresAt}, nil
}

// ---------------------------------------------------------------------------
// acting on a code
// ---------------------------------------------------------------------------

// target is a code and its site, read the way every act on a code reads them.
type target struct {
	code codeView
	site siteView
}

// readTarget reads the code, and then its site, under the caller.
//
// THE SITE IS READ AGAIN EVEN THOUGH THE CODE WAS READABLE (D15). The tier's account
// arm and the shared gate both judge the organization COPIED onto the code when it
// was minted, and a move racing a mint can leave a copy that is out of date. The
// person the copy still admits belongs to the organization the site left, and this
// read is what stops them acting on a site they can no longer see.
func (i *Integration) readTarget(ctx context.Context, codeID string) (target, error) {
	code, ok, err := i.store.codeForCaller(ctx, codeID)
	if err != nil {
		return target{}, err
	}
	if !ok {
		return target{}, refuse("No access code you can read has that id, so nothing changed. Reload the list and choose one you can see.")
	}
	site, ok, err := i.store.siteForCaller(ctx, code.SiteID)
	if err != nil {
		return target{}, err
	}
	if !ok {
		// THREE THINGS CAN BE TRUE, and which of them is not something to tell a caller who
		// cannot read the site. The people who manage a deployable that moved are refused by
		// the shared gate, which judges the copy, so sending this person to them would be
		// wrong: only a cluster operator gets past it. A deleted site reads as nothing to
		// anybody, a cluster operator included, and its code has nothing left to open.
		return target{}, refuse("You cannot read the deployable this access code belongs to, so nothing changed. If it moved to another organization, this code opens nothing and a cluster operator must remove it. If it was deleted, this code opens nothing and can be left alone. Otherwise, ask whoever manages the deployable.")
	}
	return target{code: code, site: site}, nil
}

// refuseIfEnded is the three reasons a code cannot be given a future.
//
// REMOVAL IS FINAL, and that is what lets nothing ever have to clear `revokedAt`.
//
// A STALE CODE OPENS NOTHING, because the bff refuses a code whose copied organization
// differs from its site's. Giving it a new secret or a new date would hand back
// something that looks like a working code and is not. Removing it is the one act that
// helps, and the shared gate leaves only a cluster operator able to reach it, since it
// judges the copy.
//
// NOR DOES THE CODE OF A SITE WITH NO OWNER, for the same reason and with the same one act
// left: the edge has no owner to stamp and the bff no owner to compare it with, so no
// visitor gets as far as the code. A regenerate here would put that code in the reply.
// It comes last because it is the one that can change: a stale code stays stale once the
// site has an owner, and nobody should be sent to find one before they are told that.
func refuseIfEnded(t target) error {
	if t.code.Revoked {
		return refuse("This access code was removed, and removal is final. To give someone access again, add a new code.")
	}
	if t.code.AccountID != t.site.AccountID {
		return refuse("This access code was minted while its deployable belonged to another organization, so it opens nothing. Remove it and add a new code.")
	}
	if t.site.OwnerUserID == "" {
		return refuse("This deployable has no owner, so this access code opens nothing. Give it an owner and try again, or remove the code.")
	}
	return nil
}

// handleRegenerate replaces a code's secret and nothing else: the id, the label, the
// gate and the subject survive, so rows a product stamped with this id stay attached
// and the operator reconfigures nothing. The generation moves on, which ends every
// session of the old secret at its next request.
//
// TWO REGENERATES FROM ONE READ DO NOT BOTH SUCCEED. The fence stops them landing at one
// generation, and a plain retry would then land the loser at the next, replacing a code
// the winner has just been told is the one that works and is at that moment copying to
// somebody. So a retry that finds the generation has moved reports it instead: the old
// secret is already dead, which is what regenerating is for, and whether a third secret
// is wanted is a person's decision to make with the list in front of them. A loss to
// anything else (a re-timing, say) changes nothing this act decides from, and it retries.
func (i *Integration) handleRegenerate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := refuseInARun(ctx, "Regenerate it there."); err != nil {
		return nil, err
	}
	actor, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	codeID := memql.BareShortId(asString(args["codeId"]))
	if codeID == "" {
		return nil, refuse("Choose an access code to regenerate.")
	}
	var out minted
	generationSeen := 0
	err = onceMore(func() (err error) {
		out, err = i.regenerate(ctx, actor, codeID, args, &generationSeen)
		return err
	})
	if err != nil {
		return nil, i.failed("regenerate", codeID, err, conflictOnCode)
	}
	i.audit(ctx, auditActionRegenerated, out.CodeID, map[string]any{
		"act": "regenerate", "siteId": out.SiteID, "gate": out.Gate, "generation": out.Generation, "expiresAt": wholeSecond(out.ExpiresAt),
	})
	return i.node("regenerate:"+out.CodeID, map[string]any{
		"codeId":    out.CodeID,
		"expiresAt": wholeSecond(out.ExpiresAt),
		// THE ONE TIME THIS CODE IS READABLE, as at the mint.
		"code": out.Code,
	})
}

// regenerate is one attempt. generationSeen carries what the first attempt read across
// to a second: zero on the first, and the reason a second can tell a rival regenerate
// from any other write that beat it.
func (i *Integration) regenerate(ctx context.Context, actor, codeID string, args map[string]any, generationSeen *int) (minted, error) {
	t, err := i.readTarget(ctx, codeID)
	if err != nil {
		return minted{}, err
	}
	if err := refuseIfEnded(t); err != nil {
		return minted{}, err
	}
	if *generationSeen != 0 && t.code.Generation != *generationSeen {
		return minted{}, &refusal{
			sentence: "Someone else regenerated this access code at the same moment, and the code they were given is the one that works. Reload the list, and regenerate it again if you still need to.",
			cause:    memql.ErrRowVersionChanged,
		}
	}
	*generationSeen = t.code.Generation
	now := time.Now().UTC()
	expiresAt, given, err := expiryArgument(args)
	if err != nil {
		return minted{}, err
	}
	if given {
		if err := checkExpiry(now, expiresAt); err != nil {
			return minted{}, err
		}
	} else {
		// A CODE PAST ITS EXPIRY NEEDS A NEW ONE, or this would be a fresh secret that
		// has never opened anything and never will. A kept expiry is otherwise not held
		// to the bounds: the operator is not choosing it, and refusing a code that has
		// thirty seconds left would refuse the revocation it is being regenerated for.
		// An unreadable expiry reads as zero and so as past, which fails toward asking.
		if !now.Before(t.code.ExpiresAt) {
			return minted{}, refuse("This access code has expired, so regenerating it needs a new expiry. Choose when the new code should expire.")
		}
		expiresAt = t.code.ExpiresAt
	}

	code, digest, hint, err := memql.MintAccessCode()
	if err != nil {
		return minted{}, err
	}
	generation := t.code.Generation + 1
	if err := i.store.rotateCode(ctx, rotation{
		CodeID:     t.code.ID,
		Digest:     digest,
		Hint:       hint,
		Generation: generation,
		IssuedAt:   now,
		ExpiresAt:  expiresAt,
		ChangedBy:  actor,
	}, t.code.Version); err != nil {
		return minted{}, err
	}
	return minted{CodeID: t.code.ID, Code: code, SiteID: t.site.ID, Gate: t.code.Gate, Generation: generation, ExpiresAt: expiresAt}, nil
}

// handleSetExpiry moves when a code stops opening the site, without changing the code.
// It takes effect on the next request in either direction, because validity is read
// from this row every time, and extending an expired code revives it: nobody has to be
// handed a new one.
func (i *Integration) handleSetExpiry(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	codeID := memql.BareShortId(asString(args["codeId"]))
	if codeID == "" {
		return nil, refuse("Choose an access code to change.")
	}
	var (
		t         target
		expiresAt time.Time
	)
	err = onceMore(func() (err error) {
		t, expiresAt, err = i.setExpiry(ctx, actor, codeID, args)
		return err
	})
	if err != nil {
		return nil, i.failed("setExpiry", codeID, err, conflictOnCode)
	}
	i.audit(ctx, auditActionExpiryChanged, t.code.ID, map[string]any{
		"act": "setExpiry", "siteId": t.site.ID, "gate": t.code.Gate, "generation": t.code.Generation, "expiresAt": wholeSecond(expiresAt),
	})
	return i.node("setExpiry:"+t.code.ID, map[string]any{
		"codeId":    t.code.ID,
		"expiresAt": wholeSecond(expiresAt),
	})
}

func (i *Integration) setExpiry(ctx context.Context, actor, codeID string, args map[string]any) (target, time.Time, error) {
	t, err := i.readTarget(ctx, codeID)
	if err != nil {
		return target{}, time.Time{}, err
	}
	if err := refuseIfEnded(t); err != nil {
		return target{}, time.Time{}, err
	}
	expiresAt, given, err := expiryArgument(args)
	if err != nil {
		return target{}, time.Time{}, err
	}
	if !given {
		return target{}, time.Time{}, refuse("Choose when the access code should expire.")
	}
	if err := checkExpiry(time.Now().UTC(), expiresAt); err != nil {
		return target{}, time.Time{}, err
	}
	if err := i.store.setCodeExpiry(ctx, t.code.ID, expiresAt, actor, t.code.Version); err != nil {
		return target{}, time.Time{}, err
	}
	return t, expiresAt, nil
}

// handleRemove ends a code. A soft end: the row stays as the record that the code
// existed, and every session opened with it stops being honoured at the next request.
//
// REMOVING A REMOVED CODE IS HARMLESS AND WRITES NOTHING. What was asked for is already
// true, and a write would move the row's record of who ended it and when, and put a
// second event in the log for an act that did not happen. It is also what a retry finds
// when the write that beat this one was somebody else's removal.
//
// A STALE CODE CAN BE REMOVED, unlike the other two acts: it opens nothing, but it blocks
// a further move of its site until it is gone, and this is how it goes.
func (i *Integration) handleRemove(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	codeID := memql.BareShortId(asString(args["codeId"]))
	if codeID == "" {
		return nil, refuse("Choose an access code to remove.")
	}
	var (
		t     target
		wrote bool
	)
	err = onceMore(func() (err error) {
		t, wrote, err = i.remove(ctx, actor, codeID)
		return err
	})
	if err != nil {
		return nil, i.failed("remove", codeID, err, conflictOnCode)
	}
	if wrote {
		i.audit(ctx, auditActionRemoved, t.code.ID, map[string]any{
			"act": "remove", "siteId": t.site.ID, "gate": t.code.Gate, "generation": t.code.Generation,
		})
	}
	return i.node("remove:"+t.code.ID, map[string]any{"codeId": t.code.ID})
}

func (i *Integration) remove(ctx context.Context, actor, codeID string) (target, bool, error) {
	t, err := i.readTarget(ctx, codeID)
	if err != nil {
		return target{}, false, err
	}
	if t.code.Revoked {
		return t, false, nil
	}
	if err := i.store.removeCode(ctx, t.code.ID, time.Now().UTC(), actor, t.code.Version); err != nil {
		return target{}, false, err
	}
	return t, true, nil
}

// ---------------------------------------------------------------------------
// the two reads
// ---------------------------------------------------------------------------

// handleGates lists the gates a deployable declares, for the Add form. It decides
// nothing: minting checks the gate against the same answer, because a check only one
// caller runs is not a check.
//
// EVERY WAY OF NOT KNOWING IS NO ROWS, and that is the panel's cue to explain
// @accessGate. A site the caller cannot read is a refusal rather than one of them: an
// empty answer would tell a stranger the deployable declares nothing.
func (i *Integration) handleGates(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	siteID := memql.BareShortId(asString(args["siteId"]))
	if siteID == "" {
		return nil, refuse("Choose a deployable to list the gates of.")
	}
	site, ok, err := i.store.siteForCaller(ctx, siteID)
	if err != nil {
		return nil, i.failed("gates", siteID, err, "")
	}
	if !ok {
		return nil, errSiteUnreadable()
	}
	gates, err := i.gatesOf(ctx, site)
	if err != nil {
		return nil, i.failed("gates", siteID, err, "")
	}
	nodes := make([]memorynodes.MemoryNode, 0, len(gates))
	for _, gate := range gates {
		node, err := i.node("gate:"+gate.Gate, map[string]any{
			"gate":        gate.Gate,
			"usesSubject": gate.UsesSubject,
			"constructs":  gate.Constructs,
		})
		if err != nil {
			return nil, i.failed("gates", siteID, err, "")
		}
		nodes = append(nodes, node...)
	}
	return nodes, nil
}

// gatesOf answers what a site's package can open. The deployable's name and the
// domains the package ships are both needed: gate names are short and will collide
// between products, so a gate another domain declares under the same name is not
// this deployable's to offer (D5).
func (i *Integration) gatesOf(ctx context.Context, site siteView) ([]memql.AccessGateSummary, error) {
	if site.DeployableName == "" {
		return nil, nil
	}
	domains, err := i.store.packageDomains(ctx, site.PackageID)
	if err != nil {
		return nil, err
	}
	return memql.AccessGatesFor(site.DeployableName, domains), nil
}

// handleList answers every code of a site, ended ones included, without the code or
// its digest, each with when it was last opened.
//
// THE SITE IS READ FIRST, under the caller: a person who cannot read it is refused rather
// than answered empty, and the rows and the sessions below are only read for one who can.
//
// A READ THAT FAILS IS AN ERROR, ON ANY ROW. A code with no `lastOpenedAt` says "Not
// opened yet" on the panel, so a session read that failed must not look like one that
// found nothing.
func (i *Integration) handleList(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	siteID := memql.BareShortId(asString(args["siteId"]))
	if siteID == "" {
		return nil, refuse("Choose a deployable to list the access codes of.")
	}
	site, ok, err := i.store.siteForCaller(ctx, siteID)
	if err != nil {
		return nil, i.failed("list", siteID, err, "")
	}
	if !ok {
		return nil, errSiteUnreadable()
	}
	codes, err := i.store.codesForSite(ctx, site.ID)
	if err != nil {
		return nil, i.failed("list", siteID, err, "")
	}
	nodes := make([]memorynodes.MemoryNode, 0, len(codes))
	for _, code := range codes {
		opened, err := i.store.lastOpened(ctx, code.ID, code.Generation)
		if err != nil {
			return nil, i.failed("list", siteID, err, "")
		}
		node, err := i.node("list:"+code.ID, listRow(code, opened))
		if err != nil {
			return nil, i.failed("list", siteID, err, "")
		}
		nodes = append(nodes, node...)
	}
	return nodes, nil
}

// listRow is one row of the list. An absent field is absent rather than empty, which is
// how `lastOpenedAt` says nobody has opened the code. The ids are bare, because a builtin's
// reply is what a client reads directly.
func listRow(code codeView, lastOpenedAt string) map[string]any {
	row := map[string]any{
		"id":         code.ID,
		"siteId":     code.SiteID,
		"label":      code.Label,
		"gate":       code.Gate,
		"codeHint":   code.Hint,
		"generation": code.Generation,
		"issuedAt":   code.IssuedAt,
		"expiresAt":  code.ExpiresOn,
		"createdAt":  code.Version.Format(time.RFC3339Nano),
	}
	for key, value := range map[string]string{
		"accountId":    code.AccountID,
		"subject":      code.Subject,
		"revokedAt":    code.RevokedAt,
		"lastOpenedAt": lastOpenedAt,
	} {
		if value != "" {
			row[key] = value
		}
	}
	return row
}

// ---------------------------------------------------------------------------
// shared by the acts
// ---------------------------------------------------------------------------

// conflictOnCode is what an act on a code says after losing the fence twice. Nothing it
// tried to write is in the row, and whatever beat it is. A person who reads the list again
// will see what did happen, and may no longer want what they asked for.
const conflictOnCode = "Someone else changed this access code at the same moment, so this change did not land. Reload the list and try again."

// onceMore runs an act, and runs it once more if it lost the fence.
//
// THE ACT RUNS FROM ITS FIRST READ, not from the write, because a loss means the read it
// decided from is out of date: the fence's own doc says the caller must read and decide
// again, never blindly repeat effects. So the retry can refuse what the first attempt
// allowed (a regenerate racing a removal) and a fresh secret is minted for it. A loss
// after that is reported, since a row this busy is worth a person looking at it.
func onceMore(act func() error) error {
	err := act()
	if errors.Is(err, memql.ErrRowVersionChanged) {
		return act()
	}
	return err
}

// refusal is a sentence a person reads, with the failure behind it if there was one.
// The cause stays reachable so a caller can recognise it with errors.Is, and stays out
// of the sentence, which is the part that reaches the OS.
type refusal struct {
	sentence string
	cause    error
}

func (r *refusal) Error() string { return r.sentence }
func (r *refusal) Unwrap() error { return r.cause }

func refuse(format string, args ...any) error {
	return &refusal{sentence: fmt.Sprintf(format, args...)}
}

// errSiteUnreadable is one answer for a site this caller cannot read and a site that
// does not exist, for siteForCaller's reason.
func errSiteUnreadable() error {
	return refuse("No deployable you can read has that id. Reload the list of deployables and choose one you can see.")
}

// failed is what a handler returns for whatever stopped an act.
//
// A refusal is already worded. A lost fence is told in `conflict`. Anything else is a
// failure the person cannot act on beyond trying again, so it gets a sentence that says
// so and claims nothing about what landed. The log gets a fixed reason and the id (see
// failureReason), and the cause stays behind the sentence for errors.Is.
func (i *Integration) failed(act, about string, err error, conflict string) error {
	var worded *refusal
	if errors.As(err, &worded) {
		return err
	}
	if conflict != "" && errors.Is(err, memql.ErrRowVersionChanged) {
		return &refusal{sentence: conflict, cause: err}
	}
	i.logger.Warn("siteaccess: an act on an access code failed", "act", act, "about", about, "reason", failureReason(err))
	return &refusal{
		sentence: "That did not complete. Reload the list to see where it stands, and try again.",
		cause:    err,
	}
}

// failureReason says why an act failed in words a log line can keep.
//
// NEVER THE ENGINE'S OWN TEXT. An engine error may repeat the statement it was given, and
// taking the digests out of that (scrubbed) still leaves an operator's label, a client's
// subject and a code's hint in a line read by whoever reads logs. The person who needs the
// statement has the act and the id, and can read the row.
func failureReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "the request was cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "the request timed out"
	default:
		return "the engine did not complete the statement"
	}
}

// audit writes the event for an act that landed.
//
// A FAILED AUDIT WRITE DOES NOT FAIL THE ACT. The row is already written by the time this
// runs, and for a mint or a regenerate the reply holds the only copy of the new code, so
// reporting a failure here would throw away a working credential and invite a retry that
// replaces it. It is logged instead, adminops' and integrations/groups' call on the same
// question.
//
// IT DOES NOT WAIT ON THE REQUEST. A stream dropped in the moment after the write has
// cancelled this context, and the act happened regardless, so the record of it is written
// on a context of its own with a bound of its own: compose records a terminal failure the
// same way, and for the same reason. It keeps the caller's actor, which is what it records.
//
// THE CONTEXT IS THE HANDLER'S. The witness each write carried was a local of that write
// (store.go), so this one is not fenced: the code's own write has just moved the row the
// witness names, and carrying it here would refuse this event every time.
func (i *Integration) audit(ctx context.Context, action, codeID string, detail map[string]any) {
	entry := auditEntry{Action: action, CodeID: codeID, Detail: detail}
	if access, ok := auth.AccessFromContext(ctx); ok && access != nil {
		entry.ActorID = access.UserId
		entry.ActorEmail = access.PrimaryEmail
		entry.ActorRole = string(access.Role)
	}
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	if err := i.store.writeAudit(detached, entry); err != nil {
		i.logger.Warn("siteaccess: the audit event for an access code did not land", "action", action, "codeId", codeID, "reason", failureReason(err))
	}
}

// expiryArgument reads the `expiresAt` a call carried, in the form it will be written:
// UTC at whole seconds. What is checked against the bounds is then exactly what is stored.
// `given` is false for a call that carried none.
//
// A VALUE THAT IS NOT A DATE IS NOT THE SAME AS NO VALUE. Reading it as absent would let a
// regenerate quietly keep the old expiry when somebody asked for a new one.
func expiryArgument(args map[string]any) (at time.Time, given bool, err error) {
	raw, present := args["expiresAt"]
	if !present || raw == nil {
		return time.Time{}, false, nil
	}
	text, isString := raw.(string)
	if isString && strings.TrimSpace(text) == "" {
		return time.Time{}, false, nil
	}
	if !isString {
		return time.Time{}, false, errExpiryUnreadable()
	}
	parsed, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(text))
	if parseErr != nil {
		return time.Time{}, false, errExpiryUnreadable()
	}
	return parsed.UTC().Truncate(time.Second), true, nil
}

func errExpiryUnreadable() error {
	return refuse("That expiry is not a date and time this can read. Choose when the access code should expire.")
}

// checkExpiry presents the bounds' refusal as the sentence the OS will show. Their words
// are memql.ValidateAccessCodeExpiry's, in Go's lowercase there because it is a function
// any caller may word for itself.
func checkExpiry(now, expiresAt time.Time) error {
	if err := memql.ValidateAccessCodeExpiry(now, expiresAt); err != nil {
		return refuse("%s", asSentence(err.Error()))
	}
	return nil
}

// asSentence opens a lowercase phrase with a capital and closes it with a stop.
func asSentence(phrase string) string {
	first, size := utf8.DecodeRuneInString(phrase)
	if size == 0 {
		return phrase
	}
	sentence := string(unicode.ToUpper(first)) + phrase[size:]
	if !strings.HasSuffix(sentence, ".") {
		sentence += "."
	}
	return sentence
}

func declaresGate(gates []memql.AccessGateSummary, gate string) bool {
	for _, declared := range gates {
		if declared.Gate == gate {
			return true
		}
	}
	return false
}

func gateNames(gates []memql.AccessGateSummary) string {
	names := make([]string, 0, len(gates))
	for _, gate := range gates {
		names = append(names, gate.Gate)
	}
	return strings.Join(names, ", ")
}

// refuseInARun is what a mint or a regenerate says inside a run: see the file note. It is
// asked first, before anything is read or minted, and `instead` says what to do about it.
func refuseInARun(ctx context.Context, instead string) error {
	if _, inRun := common.RunFromContext(ctx); inRun {
		return refuse("An access code is minted only by a person in Deployables, because a run's journal would keep it. %s", instead)
	}
	return nil
}

// requireCaller is the person behind an act, bare, for `lastChangedBy`.
//
// AN ACT ON A CODE BELONGS TO A PERSON. The write does not run as them, so this is the
// only record of who asked, and a connector, an anonymous actor, the cluster acting on
// its own and the operator credential have nobody to name. It is asked first, though no
// person would think to: the reads after it would refuse some of these callers, in words
// about a site or a row tier that say nothing about what is wrong, and admit the rest and
// write their name on the row.
//
// THE COST IS ACCEPTED: tooling that holds the operator key can read a list, and it can
// no longer end a code. A person signs in and does that.
func requireCaller(ctx context.Context) (string, error) {
	actor := memql.BareShortId(callerUserID(ctx))
	if actor == "" {
		return "", refuse("An access code is changed by a person, and this call resolves to no one. Sign in as yourself and try again.")
	}
	return actor, nil
}

// callerUserID is the authenticated person calling, or "" when there is none.
//
// THE ID IS ASKED AS WELL AS THE FLAG, and the flag alone is not enough. A stream admitted
// with the operator key resolves to an owner named `cluster:operator` with no Synthetic
// flag, since no user row is behind it, so it holds every part, passes the organization
// gate and is admitted by every tier: nothing else here would stop it writing that name on
// a row. auth.ActsForNoPerson asks both, the gate component/repositoryauthority also uses.
func callerUserID(ctx context.Context) string {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.IsAnonymousActor() || ac.IsConnector() || auth.ActsForNoPerson(ctx, ac.UserId) {
		return ""
	}
	return strings.TrimSpace(ac.UserId)
}

func (i *Integration) node(suffix string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("siteaccess: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "siteAccess:" + suffix,
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   raw,
	}}, nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
