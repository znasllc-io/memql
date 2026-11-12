package wholesalepack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// notify.go -- THE STORE'S OWN PEOPLE ARE TOLD (wholesaleNotifyStaff).
//
// One email to the address the store's settings name (staffEmail), through the
// transactional outbox -- the address is the store's own mailbox, which the
// outbox's allowlist must admit -- for one of six events: an application ready
// to review, each of the three decisions, and access that did not land, either
// way. A client's automation wires the events; the pack checks each against the
// application and its entitlement, so an automation that wires the wrong event
// sends nothing wrong.
//
// ONCE PER EVENT. The outbox's request id is made from the event, the
// application and, for a decision, the decision's id, or, for an access event,
// the entitlement's ATTEMPT -- never the row's version: a retry that fails again
// tells the store's people again, and a version the backfill re-writes for the
// same attempt, or a re-delivered event, stages nothing new.
//
// NOTHING OF A CLIENT'S OWN FIELDS IS REACHABLE: the placeholders are a closed
// set of the application's own facts, and any other {{...}} is left as written.

// The staff events.
const (
	StaffEventReadyToReview    = "readyToReview"
	StaffEventApproved         = "approved"
	StaffEventRejected         = "rejected"
	StaffEventRevoked          = "revoked"
	StaffEventAccessNotGranted = "accessNotGranted"
	StaffEventAccessNotRemoved = "accessNotRemoved"
)

// StaffEvents is every event, in the order the store's people meet them.
var StaffEvents = []string{StaffEventReadyToReview, StaffEventApproved, StaffEventRejected, StaffEventRevoked,
	StaffEventAccessNotGranted, StaffEventAccessNotRemoved}

// NoStaffAddressReason is the skip of a notice for a store that names no
// address.
const NoStaffAddressReason = "the store names no address"

const (
	notifyStaffActor = "wholesale-notify-staff"
	staffRequestedBy = "wholesale:staffNotice"
	// outboundRequestConcept is the transactional outbox's row; a notice is the
	// row at its request id.
	outboundRequestConcept = "v1:platform:outboundRequest"
)

// decisionOfEvent is the transition each decision event is a copy of.
var decisionOfEvent = map[string]string{
	StaffEventApproved: TransitionApprove,
	StaffEventRejected: TransitionReject,
	StaffEventRevoked:  TransitionRevoke,
}

// staffDefaults is each event's own subject and body.
var staffDefaults = map[string][2]string{
	StaffEventReadyToReview: {"A wholesale application is ready to review: {{companyName}}",
		"{{companyName}} ({{applicantName}}) applied for trade terms, and the application is ready to review.\n\n" +
			"Open the store's wholesale applications: {{adminUrl}}"},
	StaffEventApproved: {"Approved: {{companyName}}",
		"{{decidedBy}} approved the wholesale application of {{companyName}} ({{applicantName}}).\n\n{{note}}\n\n" +
			"Open the store's wholesale applications: {{adminUrl}}"},
	StaffEventRejected: {"Rejected: {{companyName}}",
		"{{decidedBy}} rejected the wholesale application of {{companyName}} ({{applicantName}}).\n\n{{note}}\n\n" +
			"Open the store's wholesale applications: {{adminUrl}}"},
	StaffEventRevoked: {"Removed: {{companyName}}",
		"{{decidedBy}} removed {{companyName}} ({{applicantName}}) as a wholesale buyer.\n\n{{note}}\n\n" +
			"Open the store's wholesale applications: {{adminUrl}}"},
	StaffEventAccessNotGranted: {"Trade prices did not turn on: {{companyName}}",
		"{{companyName}} ({{applicantName}}) is approved, but their trade prices did not turn on.\n\n" +
			"Open the application to see why and to retry access: {{adminUrl}}"},
	StaffEventAccessNotRemoved: {"Trade prices are still on: {{companyName}}",
		"{{companyName}} ({{applicantName}}) was removed, but their trade prices did not turn off.\n\n" +
			"Open the application to see why and to retry access: {{adminUrl}}"},
}

// StaffRequestID is one staff notice's outbox request id:
// wholesale-staff-<event>-<the application's bare id, as a slug>, then, when
// qualifier is not empty, -<qualifier, as a slug>. The qualifier is the
// decision's id for a decision event, empty for readyToReview, and attempt-<n>
// for an access event (AccessStaffRequestID). ONE RULE: whatever makes two
// notices of one event on one application different things to be told.
func StaffRequestID(event, applicationID, decisionID string) string {
	id := "wholesale-staff-" + slugOf(event) + "-" + slugOf(bareID(strings.TrimSpace(applicationID)))
	if q := bareID(strings.TrimSpace(decisionID)); q != "" {
		id += "-" + slugOf(q)
	}
	return id
}

// AccessStaffRequestID is an access event's request id: its qualifier is the
// entitlement's attempt, never the row's version.
func AccessStaffRequestID(event, applicationID string, attempt int) string {
	return StaffRequestID(event, applicationID, "attempt-"+strconv.Itoa(attempt))
}

// staffWords is a stage as the store's people read it.
var staffWords = map[string]string{
	StageWaitingForTerms: "Waiting for terms",
	StageReadyToReview:   "Ready to review",
	StateApproved:        "Approved",
	StateRejected:        "Rejected",
	StateRevoked:         "Removed",
}

// staffPlaceholders is the closed set fillPlaceholders fills.
var staffPlaceholders = []string{"companyName", "applicantName", "applicantEmail", "state", "decidedBy", "note", "adminUrl"}

// fillPlaceholders replaces each of the closed set's {{name}} in text with its
// value, in one pass, so a value that itself reads like a placeholder is left
// as it is; any other {{...}} is left as written.
func fillPlaceholders(text string, values map[string]string) string {
	pairs := make([]string, 0, 2*len(staffPlaceholders))
	for _, name := range staffPlaceholders {
		pairs = append(pairs, "{{"+name+"}}", values[name])
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

// notifyStaff answers wholesaleNotifyStaff.
func (p *Provider) notifyStaff(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	// FOR THE STORE'S OWN AUTOMATION ONLY. A client that could call this could
	// mail a store's people about any application it names.
	if err := requireInternal(ctx, "wholesaleNotifyStaff"); err != nil {
		return nil, err
	}
	applicationID := trimmed(args["applicationId"])
	if applicationID == "" {
		return nil, fmt.Errorf("wholesale: applicationId is required")
	}
	event := trimmed(args["event"])
	if _, known := staffDefaults[event]; !known {
		return nil, fmt.Errorf("wholesale: %q is not a staff event; an event is %s", event, strings.Join(StaffEvents, ", "))
	}
	sys := auth.ContextWithSystemActor(ctx, notifyStaffActor)
	rows, err := p.rowsFor(sys, "applicationById", map[string]string{"applicationId": applicationID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("wholesale: application %q does not exist", applicationID)
	}
	app := rows[0]
	id := trimmed(app["id"])
	settingsRow, err := p.settingsRowAt(sys, trimmed(app["storeId"]))
	if err != nil {
		return nil, err
	}
	target := EffectiveSettings(settingsRow).StaffEmail
	if target == "" {
		return confirm("wholesale:staffNotice", map[string]any{"applicationId": id, "event": event,
			"status": "skipped", "reason": NoStaffAddressReason, "requestId": ""})
	}
	state, err := p.stateOf(sys, id)
	if err != nil {
		return nil, err
	}
	termsRequired, accepted, err := p.acceptanceStanding(sys, app)
	if err != nil {
		return nil, err
	}
	stage := StageOf(state, termsRequired, accepted)
	values := map[string]string{
		"companyName":    asString(app["companyName"]),
		"applicantName":  asString(app["applicantName"]),
		"applicantEmail": trimmed(app["applicantEmail"]),
		"state":          staffWords[stage],
		"adminUrl":       "",
	}
	if base := p.adminBase(); base != "" {
		values["adminUrl"] = base + "/"
	}
	var requestID string
	switch event {
	case StaffEventReadyToReview:
		if stage != StageReadyToReview {
			return nil, fmt.Errorf("wholesale: application %q is not ready to review, so there is no readyToReview notice to send", id)
		}
		requestID = StaffRequestID(event, id, "")
	case StaffEventApproved, StaffEventRejected, StaffEventRevoked:
		decision, err := p.decisionNamed(sys, id, trimmed(args["decisionId"]), decisionOfEvent[event], event)
		if err != nil {
			return nil, err
		}
		values["decidedBy"] = p.people(ctx).name(trimmed(decision["decidedBy"]))
		values["note"] = asString(decision["note"])
		requestID = StaffRequestID(event, id, trimmed(decision["id"]))
	case StaffEventAccessNotGranted, StaffEventAccessNotRemoved:
		attempt, err := p.accessDidNotLand(sys, id, state, event)
		if err != nil {
			return nil, err
		}
		requestID = AccessStaffRequestID(event, id, attempt)
	}
	defaults := staffDefaults[event]
	subject, body := asString(args["subject"]), asString(args["body"])
	if strings.TrimSpace(subject) == "" {
		subject = defaults[0]
	}
	if strings.TrimSpace(body) == "" {
		body = defaults[1]
	}
	// ONE NOTICE PER REQUEST ID: one already staged is answered, and nothing is
	// staged again. The check is a FRESH read, and the stage is FENCED ON NO ROW
	// AT THE ID: of two nodes that pass the check at once (or one whose cache had
	// not heard of the other's stage), the second write loses the fence, writes
	// nothing over the first notice -- which the outbox may already have sent --
	// and answers it as already staged.
	alreadyStaged := func() ([]memorynodes.MemoryNode, error) {
		return confirm("wholesale:staffNotice", map[string]any{"applicationId": id, "event": event,
			"status": "staged", "requestId": requestID, "alreadyStaged": true})
	}
	staged, err := p.rowsFor(memql.ContextWithFreshRead(sys), "outboundRequestById", map[string]string{"requestId": requestID})
	if err != nil {
		return nil, err
	}
	if len(staged) > 0 {
		return alreadyStaged()
	}
	fenced := memql.ContextWithRowVersionFence(sys, outboundRequestConcept, requestID, time.Time{})
	if err := p.write(fenced, "stageServerOutboundRequest", map[string]any{
		"requestId":   requestID,
		"medium":      "email",
		"target":      target,
		"subject":     strings.TrimSpace(fillPlaceholders(subject, values)),
		"body":        fillPlaceholders(body, values),
		"dedupeKey":   requestID,
		"requestedBy": staffRequestedBy,
	}); err != nil {
		if errors.Is(err, memql.ErrRowVersionChanged) {
			return alreadyStaged()
		}
		// An engine error may quote the call it refused: the address does not
		// reach the caller or a log line.
		return nil, errors.New(strings.ReplaceAll(err.Error(), target, "the store's address"))
	}
	return confirm("wholesale:staffNotice", map[string]any{"applicationId": id, "event": event,
		"status": "staged", "requestId": requestID, "alreadyStaged": false})
}

// decisionNamed is the decision decisionID on the application, which must be
// the transition the event is a copy of. Read as sys.
func (p *Provider) decisionNamed(sys context.Context, applicationID, decisionID, transition, event string) (map[string]any, error) {
	if decisionID == "" {
		return nil, fmt.Errorf("wholesale: a %s notice names its decision (decisionId)", event)
	}
	log, err := p.rowsForApplication(sys, "decisionsForApplication", applicationID)
	if err != nil {
		return nil, err
	}
	for _, d := range log {
		if bareID(trimmed(d["id"])) != bareID(decisionID) {
			continue
		}
		if got := trimmed(d["transition"]); got != transition {
			return nil, fmt.Errorf("wholesale: decision %q is %s, not %s, so there is no %s notice to send", decisionID, got, transition, event)
		}
		return d, nil
	}
	return nil, fmt.Errorf("wholesale: decision %q is not on application %q", decisionID, applicationID)
}

// accessDidNotLand checks that the application's entitlement row is an
// attempt in the event's direction that did not land, and answers its attempt.
//
// accessNotGranted needs a grant that is pending or failed; accessNotRemoved
// a removal that is not revoked. A ROW WRITTEN BEFORE ROWS SAID WHICH WAY THEY
// WENT is read by its state where its state can tell: a granted or revoked row
// has nothing that did not land, and a pending or failed one on an application
// that is still approved can only be a grant -- a removal needs a revoke
// decision, and nothing leads out of revoked. On a revoked application it could
// be either (the grant's own row, before the removal reached it), so the event
// is refused with a sentence rather than guessed.
func (p *Provider) accessDidNotLand(sys context.Context, applicationID, state, event string) (int, error) {
	ent, err := p.entitlementRow(sys, applicationID)
	if err != nil {
		return 0, err
	}
	if ent == nil {
		return 0, fmt.Errorf("wholesale: application %q has no access record, so no access failed to land", applicationID)
	}
	entState := trimmed(ent["state"])
	direction := trimmed(ent["direction"])
	if direction == "" {
		switch {
		case entState == EntitlementGranted:
			direction = DirectionGrant
		case entState == EntitlementRevoked:
			direction = DirectionRevoke
		case state == StateApproved:
			direction = DirectionGrant
		default:
			return 0, fmt.Errorf("wholesale: application %q's access record was written before records said whether "+
				"they were a grant or a removal, and cannot tell which did not land; retry access, and the next "+
				"attempt says", applicationID)
		}
	}
	want, other := DirectionGrant, StaffEventAccessNotRemoved
	if event == StaffEventAccessNotRemoved {
		want, other = DirectionRevoke, StaffEventAccessNotGranted
	}
	if direction != want {
		return 0, fmt.Errorf("wholesale: the last access attempt on application %q was a %s, so its notice is %s, not %s",
			applicationID, direction, other, event)
	}
	landed := (want == DirectionGrant && entState == EntitlementGranted) || (want == DirectionRevoke && entState == EntitlementRevoked)
	if landed {
		return 0, fmt.Errorf("wholesale: the last access attempt on application %q landed (%s), so there is no %s notice to send",
			applicationID, entState, event)
	}
	return attemptOf(ent), nil
}

// adminBase is the OS's address, from the cluster's domain.
// A deliberate copy of packs/reviewspack/notify.go's adminBase.
func (p *Provider) adminBase() string {
	if p.osBase != nil {
		return strings.TrimRight(p.osBase(), "/")
	}
	if d := strings.TrimSpace(os.Getenv("MEMQL_DOMAIN")); d != "" {
		return "https://os." + d
	}
	return ""
}
