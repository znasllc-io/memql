package wholesalepack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
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
	// written through the pack's own mutation -- which also stamps
	// ownerUserId from the actor, the engine's own way of saying who owns a
	// row. Every concept here is @rowAuthz(owner="ownerUserId",
	// clusterOwner), so a row written any other way is owned by nobody.
	if err := p.write(ctx, "createApplicationRow", map[string]any{
		"storeId":        storeID,
		"siteId":         trimmed(args["siteId"]),
		"companyName":    company,
		"applicantName":  name,
		"applicantEmail": email,
	}); err != nil {
		return nil, err
	}
	return confirm("wholesale:application:accepted", map[string]any{
		"storeId":     storeID,
		"companyName": company,
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
	if err := p.write(ctx, "appendApplicationDecision", map[string]any{
		"applicationId": applicationID,
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
func (p *Provider) setWholesaleSettings(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
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
	if err := p.write(ctx, "setWholesaleSettingsRow", map[string]any{
		"settingsId":         SettingsRowID(storeID),
		"storeId":            storeID,
		"applicationsOpen":   open,
		"entitlementAdapter": adapter,
		"introText":          asString(args["introText"]),
	}); err != nil {
		return nil, err
	}
	return confirm("wholesale:settings:written", map[string]any{
		"storeId":            storeID,
		"applicationsOpen":   open,
		"entitlementAdapter": adapter,
	})
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
