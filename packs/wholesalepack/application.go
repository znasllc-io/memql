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
// this pack to say so (design record, section 7). So this pack refuses a
// Provider operator, refuses an illegal transition and refuses a decision
// on an unreadable application -- and takes no view at all on which of the
// merchant's own people may make one, in what order, or who is told
// afterwards. A client attaches that from its own repository with
// @trigger over v1:wholesale:applicationDecision. memql#5560 is the test
// that two clients really can.
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

// PrincipalClient is the only expressible decidedBy kind.
const PrincipalClient = "client"

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

// ClientMayDecide reports whether principalKind may author a decision.
//
// Provider operators are inexpressible: only "client" is admitted, which is
// the write-path half of what reviewspack's ClientMayModerate refuses. A
// Provider operator running a cluster can READ every merchant's
// applications -- that is what makes an operator surface possible at all --
// and cannot decide one.
func ClientMayDecide(principalKind string) error {
	if strings.TrimSpace(principalKind) != PrincipalClient {
		return fmt.Errorf("wholesale: decidedBy must be a Client principal; a Provider " +
			"operator cannot decide a merchant's application")
	}
	return nil
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

// SettingsRowID derives the one settings row id for a store.
//
// DERIVED RATHER THAN GENERATED, so the row is a singleton per store and a
// second write is a new VERSION of one logical row rather than a second row
// the read would have to choose between. reviewspack.SettingsRowID is the
// same decision for the same reason.
func SettingsRowID(storeID string) string {
	return "wholesale-settings-" + slugOf(storeID)
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
		owner = strings.TrimSpace(ac.UserId)
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
		if err := p.accountIsTheSites(ctx, trimmed(args["siteId"]), account); err != nil {
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

// accountIsTheSites refuses an accountId that is not the organization of the
// named site, read as the system.
func (p *Provider) accountIsTheSites(ctx context.Context, siteID, account string) error {
	refused := fmt.Errorf("wholesale: accountId is stamped from the organization of the site an "+
		"application arrives on, and %q is not that site's", account)
	if siteID == "" {
		return refused
	}
	rows, err := p.rowsFor(auth.ContextWithSystemActor(ctx, storeLookupActor), "siteById",
		map[string]string{"siteId": siteID})
	if err != nil {
		return err
	}
	if len(rows) == 0 || bareID(trimmed(rows[0]["accountId"])) != bareID(account) {
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
func (p *Provider) recordDecision(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := ClientMayDecide(asString(args["principalKind"])); err != nil {
		return nil, err
	}
	transition := trimmed(args["transition"])
	if err := ValidTransition(transition); err != nil {
		return nil, err
	}
	applicationID := trimmed(args["applicationId"])
	decidedBy := trimmed(args["decidedBy"])
	if applicationID == "" || decidedBy == "" {
		return nil, fmt.Errorf("wholesale: applicationId and decidedBy are required")
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
	storeID := trimmed(app["storeId"])
	if storeID == "" {
		return nil, fmt.Errorf("wholesale: application %q carries no storeId", applicationID)
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
	// decidedBy IS NOT PASSED ON. The capability requires it -- a caller
	// must say who is deciding, and refusing a Provider principal is what
	// that argument is FOR -- but the row records the ACTOR, because a
	// caller-supplied decider on an append-only log is a way to attribute
	// somebody else's approval to them permanently. mutations.memql says
	// the same thing from the other side.
	// ownerUserId IS THE APPLICATION'S MERCHANT, copied off it like storeId,
	// so the decision says whose trade desk it belongs to whoever decided.
	// accountId IS THE APPLICATION'S ORGANIZATION, copied off it, so the
	// people who read the application read what was decided about it.
	if err := p.write(ctx, "appendApplicationDecision", map[string]any{
		"applicationId": applicationID,
		"ownerUserId":   trimmed(app["ownerUserId"]),
		"accountId":     trimmed(app["accountId"]),
		"storeId":       storeID,
		"transition":    transition,
		"note":          asString(args["note"]),
	}); err != nil {
		return nil, err
	}
	return confirm("wholesale:decision:recorded", map[string]any{
		"applicationId": applicationID,
		"storeId":       storeID,
		"transition":    transition,
		"claimedBy":     decidedBy,
		"state":         StateAfter(transition),
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

// setWholesaleSettings writes one store's settings row.
//
// THE CLUSTER'S OPERATORS AND TRUSTED SERVER CODE ONLY, refused before
// anything is read. The write runs at internal origin, which the row gate
// does not owner-check, so the tier on the settings row protects nothing
// here: without this check any caller who reaches the builtin could open,
// close or re-route any store's program at its derived id.
func (p *Provider) setWholesaleSettings(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := mayConfigureStores(ctx); err != nil {
		return nil, err
	}
	storeID := trimmed(args["storeId"])
	if storeID == "" {
		return nil, fmt.Errorf("wholesale: storeId is required; settings are per store")
	}
	open, ok := args["applicationsOpen"].(bool)
	if !ok {
		return nil, fmt.Errorf("wholesale: applicationsOpen must be a bool")
	}
	adapter := trimmed(args["entitlementAdapter"])
	// AN UNKNOWN ADAPTER IS REFUSED AT SETTINGS TIME, not at approval time.
	// A merchant who typed it wrong learns now, rather than when the first
	// approved application fails to provision -- which is the moment they
	// are least able to act on it.
	if adapter != "" {
		if _, err := AdapterByName(adapter); err != nil {
			return nil, err
		}
	}
	place, err := p.settingsOwner(ctx, storeID)
	if err != nil {
		return nil, err
	}
	if err := p.write(ctx, "setWholesaleSettingsRow", map[string]any{
		"settingsId":         SettingsRowID(storeID),
		"ownerUserId":        place.owner,
		"storeId":            storeID,
		"applicationsOpen":   open,
		"entitlementAdapter": adapter,
		"introText":          asString(args["introText"]),
		// EVERY FIELD EXPLICIT: the row's organization and site are kept,
		// or, for a new row, taken from the one site bound to the store.
		"accountId": place.accountID,
		"siteId":    place.siteID,
	}); err != nil {
		return nil, err
	}
	return confirm("wholesale:settings:written", map[string]any{
		"storeId":            storeID,
		"applicationsOpen":   open,
		"entitlementAdapter": adapter,
	})
}

// mayConfigureStores admits internal origin or a cluster operator.
func mayConfigureStores(ctx context.Context) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	if ac, ok := auth.AccessFromContext(ctx); ok && auth.IsClusterOperator(ac.Role) {
		return nil
	}
	return fmt.Errorf("wholesale: a store's wholesale settings are set by the cluster's operators")
}

// storeLookupActor is the system actor that resolves whose a store's
// settings are: it reads the existing row and the sites bound to the store.
const storeLookupActor = "wholesale-store-lookup"

// settingsPlace is whose a store's settings row is after a write: its owner,
// and the organization and site it is tied to.
type settingsPlace struct {
	owner, accountID, siteID string
}

// settingsOwner is who owns a store's settings row after this write.
//
// AND WHAT IT IS TIED TO: an existing row keeps its organization and site
// (empty on a row from before this version, until the backfill ties it); a
// new row takes them from the store's site when exactly one is bound to it.
//
// THE EXISTING OWNER IS KEPT. Otherwise the operator who changed a setting
// would become the row's owner, and the merchant's own reads -- the shopper
// path's applicationsOpen among them -- would stop seeing it.
//
// A NEW ROW IS THE STORE'S MERCHANT'S: the one owner of the deployables whose
// serving or preview binding names the store (sitesBoundToStore). Two owners
// are refused rather than chosen between. With no bound site, trusted server
// code acting as a real person names that person; an operator's call is
// refused, because the operator is not the merchant and a row they owned
// would be readable by nobody on the store's side.
func (p *Provider) settingsOwner(ctx context.Context, storeID string) (settingsPlace, error) {
	sys := auth.ContextWithSystemActor(ctx, storeLookupActor)
	existing, err := p.settingsRow(sys, storeID)
	if err != nil {
		return settingsPlace{}, err
	}
	if existing != nil {
		if owner := trimmed(existing["ownerUserId"]); owner != "" {
			return settingsPlace{owner: owner, accountID: trimmed(existing["accountId"]),
				siteID: trimmed(existing["siteId"])}, nil
		}
	}
	sites, err := p.sitesForStore(sys, storeID)
	if err != nil {
		return settingsPlace{}, err
	}
	owners := map[string]bool{}
	for _, site := range sites {
		if owner := trimmed(site["ownerUserId"]); owner != "" {
			owners[owner] = true
		}
	}
	switch len(owners) {
	case 1:
		place := settingsPlace{}
		for owner := range owners {
			place.owner = owner
		}
		if len(sites) == 1 {
			place.accountID = trimmed(sites[0]["accountId"])
			place.siteID = bareID(trimmed(sites[0]["id"]))
		}
		return place, nil
	case 0:
		if auth.OriginFromContext(ctx).IsInternal() {
			if ac, ok := auth.AccessFromContext(ctx); ok && !ac.Synthetic && strings.TrimSpace(ac.UserId) != "" {
				return settingsPlace{owner: strings.TrimSpace(ac.UserId)}, nil
			}
		}
		return settingsPlace{}, fmt.Errorf("wholesale: no deployable owned by a person is bound to store %q, so "+
			"there is no merchant to own its wholesale settings; bind the store to its storefront first", storeID)
	}
	return settingsPlace{}, fmt.Errorf("wholesale: the deployables bound to store %q belong to more than one "+
		"owner, so whose wholesale settings these are cannot be told from the store alone", storeID)
}

// sitesForStore reads every site whose serving or preview binding names the
// store, in any of the forms a store id is stored in.
func (p *Provider) sitesForStore(ctx context.Context, storeID string) ([]map[string]any, error) {
	if p.reader == nil {
		return nil, fmt.Errorf("wholesale: the pack has no reader; this capability cannot run")
	}
	bare := bareID(storeID)
	return p.reader.RowsWithList(ctx, "sitesBoundToStore", map[string]string{},
		"storeIds", []string{storeID, bare, "v1:shopify:store:" + bare})
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

// applicationsOpen reads this store's gate.
//
// ABSENT IS CLOSED. A store whose merchant has never touched the setting
// has not asked for applications, and a missing row must not open a public
// write endpoint that collects somebody's business contact details.
func (p *Provider) applicationsOpen(ctx context.Context, storeID string) (bool, error) {
	settings, err := p.settingsRow(ctx, storeID)
	if err != nil {
		return false, err
	}
	if settings == nil {
		return false, nil
	}
	open, _ := settings["applicationsOpen"].(bool)
	return open, nil
}

// settingsRow reads one store's settings, or nil when there are none.
func (p *Provider) settingsRow(ctx context.Context, storeID string) (map[string]any, error) {
	rows, err := p.rowsFor(ctx, "wholesaleSettingsForStore", map[string]string{"storeId": storeID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}
