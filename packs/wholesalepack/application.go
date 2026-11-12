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
	company, name, email, err := applicationContactOf(args)
	if err != nil {
		return nil, err
	}
	// THE GATE, AND IT IS FAIL-CLOSED. A merchant who has never touched
	// their wholesale settings has not asked the internet for their
	// customers' business details, so absent settings refuse.
	settings, err := p.settingsRowAt(ctx, storeID)
	if err != nil {
		return nil, err
	}
	s := EffectiveSettings(settings)
	if !s.ApplicationsOpen {
		return nil, errNotAccepting
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
	// A STORE TAKES APPLICATIONS ONLY AS THE SHOPPER PATH BRINGS THEM (ruling
	// 56): through a site the store gate admits,
	// carrying that site's organization, from that site's owner -- whose
	// authority the handler borrows. This builtin carries no capability, so any
	// signed-in caller reaches it; every other call is refused exactly as a
	// closed store refuses it, before anything is written.
	account := trimmed(args["accountId"])
	if err := p.storefrontAdmits(ctx, trimmed(args["siteId"]), storeID, account, owner); err != nil {
		return nil, err
	}
	taken, err := p.applicationIDTaken(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	if taken {
		return p.existingSubmission(ctx, applicationID)
	}
	// A STORE THAT REQUIRES ITS TERMS TAKES NO APPLICATION UNTIL THEY EXIST,
	// judged at write time. The shopper is refused exactly as a closed store
	// refuses them -- the same words, so the same redirect: the sentence that
	// names the missing documents is for the store's people, never for a member
	// of the public.
	if err := p.termsRuleRefusal(ctx, storeID, s.ApplicationsOpen, s.TermsRequired); err != nil {
		if errors.Is(err, errTermsFirst) {
			return nil, errNotAccepting
		}
		return nil, err
	}
	// ONE OPEN APPLICATION PER ADDRESS ON A STORE (ruling 59), by the staff
	// add's own rule (openApplicationOf). Judged after every refusal above, so a
	// store that would refuse a first post refuses this one too, and before any
	// write. A post from an address with a submitted or approved application
	// WRITES NOTHING and is ANSWERED EXACTLY AS AN ACCEPTED ONE: the same receipt
	// -- the id the caller named, the store, the company it sent -- and, through
	// the shopper outcome, the same success page with no extension row. A
	// refusal would tell anybody posting somebody else's address who the store's
	// buyers are. The store's people see it in the one line below, which names
	// the store and the open application and never the address. Timing is not
	// equalized: this answer returns before a write a first post makes.
	open, err := p.openApplicationOf(ctx, storeID, email)
	if err != nil {
		return nil, err
	}
	if open != "" {
		p.log().Info(OpenApplicationNotWrittenLine,
			"component", "wholesale", "storeId", storeID, "openApplicationId", bareID(open))
		memql.MarkShopperSubmissionUnwritten(ctx)
		return acceptedReceipt(applicationID, storeID, company)
	}
	fenced := memql.ContextWithRowVersionFence(ctx, applicationConcept, applicationID, time.Time{})
	// accountId IS THE ADMITTED SITE'S ORGANIZATION, which is the organization
	// the store's program is tied to, and the owner is that site's owner:
	// storefrontAdmits refused every call where either is not so, an absent
	// accountId included.
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
	return acceptedReceipt(applicationID, storeID, company)
}

// acceptedReceipt answers a storefront submission that was accepted: the id
// the caller named, the store and the company it sent. A post written and a
// post from an address with an open application (not written) are answered
// with it alike, so the two cannot be told apart.
func acceptedReceipt(applicationID, storeID, company string) ([]memorynodes.MemoryNode, error) {
	return confirm("wholesale:application:accepted", map[string]any{
		"applicationId": applicationID,
		"storeId":       storeID,
		"companyName":   company,
	})
}

// applicationContactOf is an application's company, applicant and email, each
// trimmed, held to the application form's own rules (applicationFields): every
// one is required, none is longer than the form admits, and one that declares a
// format meets it (the address is an address, memql.IsEmailAddress). The shopper's post
// and a person of the store adding a buyer meet the same rules here.
func applicationContactOf(args map[string]any) (company, name, email string, err error) {
	values := map[string]string{}
	for _, f := range applicationFields() {
		v := trimmed(args[f.Name])
		if v == "" && f.Required {
			return "", "", "", fmt.Errorf("wholesale: companyName, applicantName and applicantEmail " +
				"are the minimum an application needs to be one")
		}
		if len(v) > f.EffectiveMaxLength() {
			return "", "", "", fmt.Errorf("wholesale: %s is at most %d bytes; this one is %d",
				f.Name, f.EffectiveMaxLength(), len(v))
		}
		if v != "" && !f.MatchesFormat(v) {
			return "", "", "", fmt.Errorf("wholesale: %s is not %s", f.Name, formatNoun(f.Format))
		}
		values[f.Name] = v
	}
	return values["companyName"], values["applicantName"], values["applicantEmail"], nil
}

// formatNoun names what a value of a shopper format is, for a refusal.
func formatNoun(format string) string {
	if format == memql.ShopperFormatEmail {
		return "an email address"
	}
	return "in the format " + format
}

// errNotAccepting is a shopper's refusal by a store that takes no application
// now: closed, requiring terms it has not published, not set up, or reached
// through a site that is not of its program's organization.
var errNotAccepting = errors.New("wholesale: this store is not accepting applications")

// storefrontAdmits refuses a submission, as a store that is not accepting
// applications refuses it, unless all of these hold, UNCONDITIONALLY:
//
//   - it names a site that serves the store, and the store gate admits that
//     site (storeAdmitsSite): the store is set up, and the site is of the
//     organization its program is tied to;
//   - the accountId it carries IS that site's organization -- an absent one is
//     not a pass, or a direct caller could write an application of no
//     organization into the store;
//   - the caller IS that site's owner, the authority the shopper handler
//     borrows.
//
// The site is read as the system: the judgement is of the site, not of who can
// read it.
func (p *Provider) storefrontAdmits(ctx context.Context, siteID, storeID, account, caller string) error {
	if siteID == "" {
		return errNotAccepting
	}
	rows, err := p.rowsFor(auth.ContextWithSystemActor(ctx, storeLookupActor), "siteById", map[string]string{"siteId": siteID})
	if err != nil {
		return err
	}
	if len(rows) == 0 || !servesStore(rows[0], bareID(storeID)) {
		return errNotAccepting
	}
	site := rows[0]
	if err := p.storeAdmitsSite(ctx, site, siteID, storeID); err != nil {
		if errors.Is(err, errNotSetUp) || errors.Is(err, errAnotherOrganization) {
			return errNotAccepting
		}
		return err
	}
	if bareID(account) == "" || bareID(account) != bareID(trimmed(site["accountId"])) ||
		userRef(trimmed(site["ownerUserId"])) != userRef(caller) {
		return errNotAccepting
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
	// LEGALITY IS CHECKED AGAINST THE WHOLE LOG, not against an argument and
	// not against the part of it the caller can read (X27). The caller's own
	// read of the application admitted them; the log is then folded as the
	// system, because a decision written before the account tie names no
	// merchant and the store's side cannot read it -- and an approve judged
	// against the readable part would re-decide a rejected application.
	sys := auth.ContextWithSystemActor(ctx, provisionActor)
	state, err := p.stateOf(sys, applicationID)
	if err != nil {
		return nil, err
	}
	if err := LegalTransition(state, transition); err != nil {
		return nil, err
	}
	// AN APPROVAL WAITS FOR THE ACCEPTANCE while the store requires its terms
	// (7.2), judged as the deployment like the log.
	termsRequired, accepted, err := p.acceptanceStanding(sys, app)
	if err != nil {
		return nil, err
	}
	if transition == TransitionApprove && termsRequired && !accepted {
		return nil, errors.New(NotAcceptedSentence)
	}
	// ONE APPROVED APPLICATION PER ADDRESS ON A STORE (ruling 61). Two posts in
	// one instant both pass the storefront's check, as do rows written before
	// it; two approved applications of one buyer would let a revoke of either
	// take the store-wide tag off while the other still reads granted. So an
	// approve is refused here, naming the other, by the same address rule
	// (applicationOfAddress), read as the system. A reject or a revoke is never
	// refused by it.
	if transition == TransitionApprove {
		other, err := p.applicationOfAddress(ctx, trimmed(app["storeId"]), asString(app["applicantEmail"]),
			trimmed(app["id"]), StateApproved)
		if err != nil {
			return nil, err
		}
		if other != "" {
			return nil, errors.New(AnotherApprovedSentence(other))
		}
	}
	if err := p.appendDecision(ctx, app, transition, asString(args["note"])); err != nil {
		return nil, err
	}
	return decisionReceipt(applicationID, app, transition, decider, termsRequired, accepted)
}

// decisionReceipt answers a decision just recorded on app, named as the caller
// named it: its store, the act, the decider, and the state and stage it leaves.
func decisionReceipt(applicationID string, app map[string]any, transition, decider string,
	termsRequired, accepted bool) ([]memorynodes.MemoryNode, error) {
	after := StateAfter(transition)
	return confirm("wholesale:decision:recorded", map[string]any{
		"applicationId": applicationID,
		"storeId":       trimmed(app["storeId"]),
		"transition":    transition,
		// The value the row holds: the actor's id, in canonical form.
		"decidedBy": userRef(decider),
		"state":     after,
		"stage":     StageOf(after, termsRequired, accepted),
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
// THE ROW NAMES ITS APPLICATION IN CANONICAL FORM (applicationRef). The engine
// compares the field as written and does not join a bare id with a canonical
// one, so a log written in whichever form each caller used would split; every
// read keyed by an application id asks both forms (rowsForApplication), so the
// rows written before this version are found too.
func (p *Provider) appendDecision(ctx context.Context, app map[string]any, transition, note string) error {
	applicationID := applicationRef(trimmed(app["id"]))
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
	// THE CALLER READS THE APPLICATION FIRST. One who cannot is answered what
	// an id nobody can see answers -- submitted -- and nothing is read as the
	// system for them, so a foreign id is not told from a missing one. One who
	// can is answered the WHOLE log's fold (X27), read as the system.
	state := StateSubmitted
	termsRequired, accepted := false, false
	rows, err := p.rowsFor(ctx, "applicationById", map[string]string{"applicationId": applicationID})
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		sys := auth.ContextWithSystemActor(ctx, provisionActor)
		if state, err = p.stateOf(sys, applicationID); err != nil {
			return nil, err
		}
		if termsRequired, accepted, err = p.acceptanceStanding(sys, rows[0]); err != nil {
			return nil, err
		}
	}
	payload, err := json.Marshal(map[string]any{
		"applicationId": applicationID,
		"state":         state,
		"stage":         StageOf(state, termsRequired, accepted),
		"legalNext":     legalFrom[state],
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

// acceptanceStanding answers whether app's store requires its terms and, when it
// does, whether app has accepted them, read through sys. A store that requires
// none needs no acceptance read.
func (p *Provider) acceptanceStanding(sys context.Context, app map[string]any) (termsRequired, accepted bool, err error) {
	if termsRequired, err = p.termsRequiredAt(sys, trimmed(app["storeId"])); err != nil || !termsRequired {
		return termsRequired, false, err
	}
	acceptance, err := p.acceptanceOf(sys, trimmed(app["id"]))
	if err != nil {
		return false, false, err
	}
	return true, acceptance != nil, nil
}

// stateOf folds one application's decision log.
func (p *Provider) stateOf(ctx context.Context, applicationID string) (string, error) {
	rows, err := p.rowsForApplication(ctx, "decisionsForApplication", applicationID)
	if err != nil {
		return "", err
	}
	return FoldState(rows), nil
}
