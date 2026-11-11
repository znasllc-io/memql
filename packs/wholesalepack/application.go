package wholesalepack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// application.go -- THE APPLICATION, ITS STATES AND ITS TRANSITIONS (epic
// memql#5533, issues memql#5555 and memql#5556).
//
// # The pack guarantees what must be safe, and nothing else
//
// Three invariants live here, and every one of them is a CROSS-ROW READ,
// which is why they are Go rather than a mutation body:
//
//   - APPLICATIONS CAN BE CLOSED. A submission is refused when this store's
//     wholesaleSettings.applicationsOpen is false, and absent settings are
//     closed. A mutation cannot read another concept's row, so a mutation
//     here would leave a closed door as something the storefront merely
//     declines to render.
//   - A DECISION IS SCOPED TO ITS SUBJECT'S STORE. storeId is copied off
//     the application being decided rather than supplied, exactly as
//     reviewspack copies it off the review being moderated -- and the read
//     runs under the CALLER'S own actor, so a caller who cannot read an
//     application cannot decide it either.
//   - ONLY LEGAL TRANSITIONS ARE RECORDED. approve and reject are legal
//     from submitted; revoke is legal only from approved. Deciding legality
//     means folding the decision log, which is a read of a second concept.
//
// # WHO approves is deliberately NOT here
//
// dsl/commerce already ships approvalChain for a client that wants a
// threshold and an order, and a client that does NOT must not have to edit
// this pack to say so (design record, section 7). So this pack admits a
// person holding the wholesale part in the application's organization (the
// organization gate's), refuses the cluster's own operators, refuses an
// illegal transition and refuses a decision on an unreadable application --
// and takes no view at all on which of the store's own people may make one,
// in what order, or who is told afterwards. A client attaches that from its
// own repository with @trigger over v1:wholesale:applicationDecision.
// memql#5560 is the test that two clients really can.
//
// # State is DERIVED, never stored
//
// There is no mutable state field on the application. A new decision is a
// new ROW, in the manner of v1:reviews:moderationAction, and the current
// state is the newest decision folded -- or submitted, when there is none.
// An application whose state was overwritten would lose WHY it was refused
// the moment somebody approved it, and a trade desk looking back at a
// disputed account is exactly the reader who needs that.

// The four states an application moves through. They are derived from the
// decision log rather than stored, so they are Go constants rather than a
// concept field.
const (
	StateSubmitted = "submitted"
	StateApproved  = "approved"
	StateRejected  = "rejected"
	StateRevoked   = "revoked"
)

// The three acts a decision records. The concept's enum is the same three
// words; this is the Go side of that closed set.
const (
	TransitionApprove = "approve"
	TransitionReject  = "reject"
	TransitionRevoke  = "revoke"
)

// PrincipalClient is the only principalKind a decision row carries. The
// builtin's principalKind argument is ignored: the decider is the caller.
const PrincipalClient = "client"

// The stages an application is shown in. Both are of state submitted: an
// application waits for its terms while the store requires them and they are
// not accepted, and is ready to review otherwise. Every other state is its own
// stage.
const (
	StageWaitingForTerms = "waitingForTerms"
	StageReadyToReview   = "readyToReview"
)

// How an application arrived: a shopper's form post, or a person of the store
// adding it. An application that names neither arrived on the storefront.
const (
	SourceStorefront = "storefront"
	SourceStaff      = "staff"
)

// Transitions is the closed set, in the order a reader meets them.
var Transitions = []string{TransitionApprove, TransitionReject, TransitionRevoke}

// legalFrom is the transition table.
//
// A TABLE RATHER THAN A CHAIN OF IFS, so the legal graph is readable in one
// glance and a fourth transition is a line rather than a rewrite. Note what
// it does NOT contain: nothing leads out of rejected or revoked. A rejected
// applicant applies again -- that is a NEW application with its own log --
// rather than having their old refusal quietly re-decided, and a revoked
// entitlement is re-granted the same way. Re-deciding in place is precisely
// the overwrite this pack's storage shape exists to prevent.
var legalFrom = map[string][]string{
	StateSubmitted: {TransitionApprove, TransitionReject},
	StateApproved:  {TransitionRevoke},
	StateRejected:  {},
	StateRevoked:   {},
}

// stateAfter maps an act to the state it leaves the application in.
var stateAfter = map[string]string{
	TransitionApprove: StateApproved,
	TransitionReject:  StateRejected,
	TransitionRevoke:  StateRevoked,
}

// StageOf is the stage an application in state is shown in. termsRequired is
// whether the store requires its terms accepted, accepted whether this
// application has accepted them.
func StageOf(state string, termsRequired, accepted bool) string {
	if state != StateSubmitted {
		return state
	}
	if termsRequired && !accepted {
		return StageWaitingForTerms
	}
	return StageReadyToReview
}

// deciderOf is the person a decision is recorded for: the calling user.
//
// A PERSON ON THE STORE'S SIDE DECIDES (X4). The cluster's own operators --
// the people who host the store's applications and read every one of them --
// never record a decision: the store holds the decision, and the people who run
// the platform must not be able to make it for them. A call with no signed-in
// person, or one acting as a synthetic actor, decides nothing either. The same
// rule, in the same words, as packs/reviewspack/decide.go's moderatorOf.
func deciderOf(ctx context.Context) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac.Synthetic || strings.TrimSpace(ac.UserId) == "" {
		return "", errors.New("wholesale: a decision needs a signed-in person")
	}
	if auth.IsClusterOperator(ac.Role) {
		return "", errors.New("wholesale: the cluster's operators host the store's applications and do not " +
			"decide them; a person on the store's side decides")
	}
	return strings.TrimSpace(ac.UserId), nil
}

// ValidTransition reports whether transition is in the closed set.
func ValidTransition(transition string) error {
	t := strings.TrimSpace(transition)
	for _, want := range Transitions {
		if t == want {
			return nil
		}
	}
	return fmt.Errorf("wholesale: transition %q is not one of approve, reject, revoke", transition)
}

// LegalTransition reports whether transition may be applied from state.
//
// The refusal NAMES what was legal, because the caller is a client's own
// automation and "illegal transition" with no list is a message somebody
// has to read this file to act on.
func LegalTransition(state, transition string) error {
	if err := ValidTransition(transition); err != nil {
		return err
	}
	allowed, known := legalFrom[state]
	if !known {
		return fmt.Errorf("wholesale: %q is not a state this pack derives", state)
	}
	for _, a := range allowed {
		if a == transition {
			return nil
		}
	}
	if len(allowed) == 0 {
		return fmt.Errorf("wholesale: an application that is %s is final; %q is refused. "+
			"Re-deciding in place would overwrite why the first decision was made -- a new "+
			"application is how an applicant tries again", state, transition)
	}
	return fmt.Errorf("wholesale: %q is not legal from %s; legal here is %s",
		transition, state, strings.Join(allowed, ", "))
}

// StateAfter is the state a legal transition leaves behind.
func StateAfter(transition string) string { return stateAfter[strings.TrimSpace(transition)] }

// FoldState reduces an application's decisions, NEWEST FIRST, to its state.
//
// The queries that feed this sort "row.createdAt", "desc", so the first row
// is the last decision. A row whose transition this pack does not recognise
// is SKIPPED rather than treated as final: the alternative is a payload
// nobody can parse silently pinning an application into a state no
// transition leads out of.
func FoldState(decisionsNewestFirst []map[string]any) string {
	for _, row := range decisionsNewestFirst {
		if s := StateAfter(trimmed(row["transition"])); s != "" {
			return s
		}
	}
	return StateSubmitted
}

// --------------------------------------------------------------------------
// Capabilities
// --------------------------------------------------------------------------

// submitApplication is the SHOPPER write path.
//
// A BUILTIN RATHER THAN A MUTATION because it has a precondition a mutation
// body cannot check (shopper.go says why at length), and because it derives
// the row id: the caller is a member of the public posting a plain HTML
// form, and a shopper who could choose a row id could overwrite another
// applicant's row.
func (p *Provider) submitApplication(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := trimmed(args["storeId"])
	if storeID == "" {
		return nil, fmt.Errorf("wholesale: storeId is required; it is stamped from the " +
			"binding the submission arrived through")
	}
	company := trimmed(args["companyName"])
	name := trimmed(args["applicantName"])
	email := trimmed(args["applicantEmail"])
	if company == "" || name == "" || email == "" {
		return nil, fmt.Errorf("wholesale: companyName, applicantName and applicantEmail " +
			"are the minimum an application needs to be one")
	}
	// THE GATE, AND IT IS FAIL-CLOSED. A merchant who has never touched
	// their wholesale settings has not asked the internet for their
	// customers' business details, so absent settings refuse.
	open, err := p.applicationsOpen(ctx, storeID)
	if err != nil {
		return nil, err
	}
	if !open {
		return nil, fmt.Errorf("wholesale: this store is not accepting applications")
	}
	// THE BUILTIN DECIDED; THE MUTATION WRITES. A builtin's returned nodes
	// are the expression's VALUE and are never persisted, so the row is
	// written through the pack's own mutation. ownerUserId is the CALLING
	// ACTOR -- on the shopper path the site owner whose authority the handler
	// borrowed -- passed explicitly, because every concept here is
	// @rowAuthz(owner="ownerUserId", clusterOwner) and a row owned by nobody
	// is invisible to the merchant it was submitted to.
	// THE ROW ID IS THE SUBMISSION ID THE BFF MINTED (design record
	// 2026-09-21, D4). It was derived by the engine until this seam
	// existed, which left the new row's id known to nobody -- and a client
	// extension has to relate its own row to THIS application. It travels
	// DOWN from the handler into both constructs. The receipt names it back,
	// which tells the caller nothing it did not send.
	//
	// ANY CALLER WHO REACHES THIS BUILTIN CAN NAME IT. The shopper handler
	// stamps it after the declared fields, but the builtin's object args
	// profile admits it from any signed-in caller too -- and the write runs
	// at internal origin, which the row gate does not owner-check. So the
	// id is established ABSENT before the write, and the write is fenced on
	// "row absent", or one caller could write over another's application.
	applicationID := trimmed(args["submissionId"])
	if applicationID == "" {
		return nil, fmt.Errorf("wholesale: submissionId is required; it is minted per " +
			"submission and stamped by the bff, and it is the id this application is written at")
	}
	owner := ""
	if ac, ok := auth.AccessFromContext(ctx); ok {
		// CANONICAL, like every other owner this pack writes (userRef).
		owner = userRef(ac.UserId)
	}
	if owner == "" {
		return nil, fmt.Errorf("wholesale: an application is written as the site owner, and " +
			"this call carries no actor")
	}
	// THE ACCOUNT MUST BE THE SITE'S. The shopper handler stamps it from the
	// site, but any signed-in caller who reaches this builtin can name one, and
	// an application tied to an organization is readable by its people.
	account := trimmed(args["accountId"])
	if account != "" {
		if err := p.accountIsTheSites(ctx, trimmed(args["siteId"]), storeID, owner, account); err != nil {
			return nil, err
		}
	}
	taken, err := p.applicationIDTaken(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	if taken {
		return p.existingSubmission(ctx, applicationID)
	}
	fenced := memql.ContextWithRowVersionFence(ctx, applicationConcept, applicationID, time.Time{})
	// accountId IS THE SITE'S ORGANIZATION, stamped by the shopper handler
	// from the site the post arrived on, and empty for a site with none --
	// which is a legitimate state: the application is then written untied and
	// stays its owner's, as before.
	if err := p.write(fenced, "createApplicationRow", map[string]any{
		"applicationId":  applicationID,
		"ownerUserId":    owner,
		"storeId":        storeID,
		"siteId":         trimmed(args["siteId"]),
		"companyName":    company,
		"applicantName":  name,
		"applicantEmail": email,
		"accountId":      account,
		"source":         SourceStorefront,
	}); err != nil {
		// THE FENCE'S LOSER: another write landed at this id between the
		// check above and this one. Answered exactly as a taken id is.
		if errors.Is(err, memql.ErrRowVersionChanged) {
			return p.existingSubmission(ctx, applicationID)
		}
		return nil, err
	}
	return confirm("wholesale:application:accepted", map[string]any{
		"applicationId": applicationID,
		"storeId":       storeID,
		"companyName":   company,
	})
}

// accountIsTheSites refuses an accountId unless the named site, read as the
// system, is of that organization, serves the named store, and is owned by
// the caller. The shopper handler stamps exactly these -- the site, its store,
// its organization -- and borrows its owner, so the shopper path passes; a
// direct call naming somebody else's site does not.
func (p *Provider) accountIsTheSites(ctx context.Context, siteID, storeID, caller, account string) error {
	refused := fmt.Errorf("wholesale: an application carries an accountId only when it arrives on "+
		"that organization's site, from the site's owner, for a store the site serves; %q is not", account)
	if siteID == "" {
		return refused
	}
	rows, err := p.rowsFor(auth.ContextWithSystemActor(ctx, storeLookupActor), "siteById",
		map[string]string{"siteId": siteID})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return refused
	}
	site := rows[0]
	if bareID(trimmed(site["accountId"])) != bareID(account) || !servesStore(site, bareID(storeID)) ||
		userRef(trimmed(site["ownerUserId"])) != userRef(caller) {
		return refused
	}
	return nil
}

// applicationConcept is the application's concept id, for its row fence.
const applicationConcept = "v1:wholesale:application"

// idCheckActor is the system actor that asks whether an application id is
// taken. It sees every row, and answers only yes or no.
const idCheckActor = "wholesale-id-check"

// applicationIDTaken reports whether any application exists at id, whoever
// owns it.
func (p *Provider) applicationIDTaken(ctx context.Context, applicationID string) (bool, error) {
	rows, err := p.rowsFor(auth.ContextWithSystemActor(ctx, idCheckActor), "applicationById",
		map[string]string{"applicationId": applicationID})
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

// existingSubmission answers a submission at an id that is already taken.
//
// A REPEAT IS THE SAME POST AGAIN, so the caller who can read the row is
// answered its id and nothing is written. A caller who cannot read it is
// told only that the id is taken: nothing about whose it is or what it says.
func (p *Provider) existingSubmission(ctx context.Context, applicationID string) ([]memorynodes.MemoryNode, error) {
	rows, err := p.rowsFor(ctx, "applicationById", map[string]string{"applicationId": applicationID})
	if err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("wholesale: application id %q is already taken", applicationID)
	}
	return confirm("wholesale:application:accepted", map[string]any{
		"applicationId": applicationID,
		"storeId":       trimmed(rows[0]["storeId"]),
		"existing":      true,
	})
}

// recordDecision appends one decision to an application's log.
//
// THE ORGANIZATION GATE ADMITTED THE CALLER before this runs: they hold the
// wholesale part in the application's organization. Then, before anything is
// read, the caller must be a person who is not one of the cluster's operators
// (deciderOf). The principalKind and decidedBy arguments are declared for old
// callers and ignored.
func (p *Provider) recordDecision(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	decider, err := deciderOf(ctx)
	if err != nil {
		return nil, err
	}
	transition := trimmed(args["transition"])
	if err := ValidTransition(transition); err != nil {
		return nil, err
	}
	applicationID := trimmed(args["applicationId"])
	if applicationID == "" {
		return nil, fmt.Errorf("wholesale: applicationId is required")
	}
	// THE STORE IS READ OFF THE APPLICATION, never supplied (design D9).
	// The read runs under the CALLER'S own actor, so a caller who cannot
	// read the application cannot decide it -- which also refuses a
	// decision naming an application that does not exist, instead of
	// appending a row scoped to no store that every storefront read would
	// then miss.
	app, err := p.applicationRow(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	// LEGALITY IS CHECKED AGAINST THE LOG, not against an argument. The
	// caller does not say what state it believes the application is in,
	// because a caller that could would be the one deciding.
	state, err := p.stateOf(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	if err := LegalTransition(state, transition); err != nil {
		return nil, err
	}
	// THE DECISION NAMES THE APPLICATION AS THE CALLER DID, which is the form
	// the caller then reads its log by (decisionsForApplication compares the
	// field as written).
	subject := make(map[string]any, len(app))
	for k, v := range app {
		subject[k] = v
	}
	subject["id"] = applicationID
	if err := p.appendDecision(ctx, subject, transition, asString(args["note"])); err != nil {
		return nil, err
	}
	after := StateAfter(transition)
	return confirm("wholesale:decision:recorded", map[string]any{
		"applicationId": applicationID,
		"storeId":       trimmed(app["storeId"]),
		"transition":    transition,
		"decidedBy":     decider,
		"state":         after,
		"stage":         StageOf(after, false, false),
	})
}

// appendDecision is THE ONE WRITE PATH OF A DECISION: one new row in the
// application's log, as ctx's actor, whom the row stamps as its decider.
//
// Its callers have established who ctx is and that the transition is legal.
// What the row is ABOUT is copied off the application, never supplied:
// storeId, so a decision is scoped to its subject's store (design D9);
// ownerUserId, the application's merchant, so the merchant reads every
// decision whoever made it; accountId, the application's organization, so the
// people who read the application read what was decided about it.
//
// decidedBy IS NOT PASSED ON. The row records the ACTOR, because a
// caller-supplied decider on an append-only log is a way to attribute
// somebody else's approval to them permanently. mutations.memql says the same
// thing from the other side.
//
// The row's applicationId is app's "id" exactly as app carries it.
func (p *Provider) appendDecision(ctx context.Context, app map[string]any, transition, note string) error {
	applicationID := trimmed(app["id"])
	storeID := trimmed(app["storeId"])
	if applicationID == "" {
		return fmt.Errorf("wholesale: the application being decided carries no id")
	}
	if storeID == "" {
		return fmt.Errorf("wholesale: application %q carries no storeId", applicationID)
	}
	return p.write(ctx, "appendApplicationDecision", map[string]any{
		"applicationId": applicationID,
		"ownerUserId":   userRef(trimmed(app["ownerUserId"])),
		"accountId":     trimmed(app["accountId"]),
		"storeId":       storeID,
		"transition":    transition,
		"note":          note,
	})
}

// applicationState answers the derived state of one application.
func (p *Provider) applicationState(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	applicationID := trimmed(args["applicationId"])
	if applicationID == "" {
		return nil, fmt.Errorf("wholesale: applicationId is required")
	}
	state, err := p.stateOf(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"applicationId": applicationID,
		"state":         state,
		// No store requires its terms yet, so a submitted application is ready.
		"stage":     StageOf(state, false, false),
		"legalNext": legalFrom[state],
	})
	if err != nil {
		return nil, fmt.Errorf("wholesale: marshal state: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "wholesale:state:" + applicationID,
		Concept:   "v1:wholesale:applicationState",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

// bareID is an id without its concept prefix: the part after the last colon.
func bareID(id string) string {
	if i := strings.LastIndex(id, ":"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// --------------------------------------------------------------------------
// The reads the capabilities above decide over
// --------------------------------------------------------------------------

// applicationRow reads one application under the caller's own actor.
func (p *Provider) applicationRow(ctx context.Context, applicationID string) (map[string]any, error) {
	rows, err := p.rowsFor(ctx, "applicationById", map[string]string{"applicationId": applicationID})
	if err != nil {
		return nil, fmt.Errorf("wholesale: resolving the application being decided: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("wholesale: application %q is not readable by this caller, "+
			"so it cannot be decided", applicationID)
	}
	return rows[0], nil
}

// stateOf folds one application's decision log.
func (p *Provider) stateOf(ctx context.Context, applicationID string) (string, error) {
	rows, err := p.rowsFor(ctx, "decisionsForApplication",
		map[string]string{"applicationId": applicationID})
	if err != nil {
		return "", err
	}
	return FoldState(rows), nil
}
