package wholesalepack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// buyers.go -- THE STORE'S PEOPLE ADD A BUYER BY HAND AND REMOVE ONE (U10;
// W7, X16).
//
// # An added buyer skips nothing
//
// Adding a buyer writes an APPLICATION on the buyer's behalf, nothing more: it
// is submitted like a shopper's, it waits for the store's terms like a
// shopper's when the store requires them, and it is approved through
// wholesaleRecordDecision like any other. Nothing here approves or grants, so
// every buyer has the acceptance record the terms rule promises (W5). The
// first email follows through the client's own automation on node.created, as
// for anyone; the application is written once, so that is its first version.
//
// # Removing a buyer is a revoke
//
// Removing records a revoke decision by the caller through the one write path
// of a decision (appendDecision). The tag comes off through the client's revoke
// automation, or wholesaleRetryAccess where that automation is not wired.
//
// # Who may
//
// Both are gated by the wholesale part (the organization gate). Adding is
// judged against the SITE's organization, and the store gate (siteForStore)
// then admits a site that serves the store only once the store is set up, and
// only for the organization its settings are tied to. Removing is judged
// against the APPLICATION's organization, and the caller must then read the
// application under their own actor. The cluster's operators add and remove
// nobody, refused as a decision is (deciderOf): the store holds who its buyers
// are.

// AlreadyAppliedSentence refuses adding an address that has a submitted or an
// approved application on the store.
const AlreadyAppliedSentence = "This address already has an application on this store."

// OpenApplicationNotWrittenLine is the staff's trace of a storefront post that
// was not written because its address has an open application on the store
// (ruling 59). The line names the store and the open application, never the
// address.
const OpenApplicationNotWrittenLine = "wholesale: an application from an address with an open application on this store was not written"

// AnotherApprovedSentence refuses an approve while another application of the
// same address on the store is approved (ruling 61), naming that application.
func AnotherApprovedSentence(approvedID string) string {
	return "Application " + bareID(approvedID) + " of this address is already approved on this store. " +
		"Remove that buyer before approving this application, or reject this one."
}

// staffApplicationPrefix begins the id of an application a person of the store
// added.
const staffApplicationPrefix = "wholesale-staff-"

// duplicateCheckActor is the system actor that reads a store's applications to
// ask whether an address already has one. It sees every row, so an application
// the caller cannot read still blocks a second one, and it answers only yes or
// no.
const duplicateCheckActor = "wholesale-duplicate-check"

// addBuyer answers wholesaleAddBuyer: an application written on a buyer's
// behalf by a person of the store.
//
// THE ORGANIZATION GATE ADMITTED THE CALLER for the site. Then, before anything
// is read, the caller must be a person who is not one of the cluster's
// operators (deciderOf), and the three fields must meet the application form's
// rules. Then the store gate (siteForStore) admits the act: the site serves
// the store, the store is set up -- its settings row is tied to an
// organization, else NotSetUpSentence -- and the site is of that organization.
// Then the store's documents must exist
// when it requires its terms -- whether or not applications are open, which is
// the public form's door and not this one's -- and the address must have no
// submitted or approved application on the store.
func (p *Provider) addBuyer(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	addedBy, err := deciderOf(ctx)
	if err != nil {
		return nil, err
	}
	company, name, email, err := applicationContactOf(args)
	if err != nil {
		return nil, err
	}
	storeID, siteID := trimmed(args["storeId"]), trimmed(args["siteId"])
	if storeID == "" {
		return nil, fmt.Errorf("wholesale: storeId is required")
	}
	site, err := p.siteForStore(ctx, siteID, storeID)
	if err != nil {
		return nil, err
	}
	owner := userRef(trimmed(site["ownerUserId"]))
	if owner == "" {
		return nil, fmt.Errorf("wholesale: site %q names no owner, so there is no merchant to own the application", siteID)
	}
	// THE SETTINGS AND THE DOCUMENTS ARE READ AS THE DEPLOYMENT, as every
	// judgement of the terms rule is: the rule is the store's, not the caller's.
	sys := auth.ContextWithSystemActor(ctx, provisionActor)
	settings, err := p.settingsRowAt(sys, storeID)
	if err != nil {
		return nil, err
	}
	if err := p.termsRuleRefusal(sys, storeID, true, EffectiveSettings(settings).TermsRequired); err != nil {
		return nil, err
	}
	open, err := p.openApplicationOf(ctx, storeID, email)
	if err != nil {
		return nil, err
	}
	// THE STAFF MEMBER IS TOLD, being entitled to know who the store's buyers
	// are; a shopper posting the same address is not (submitApplication).
	if open != "" {
		return nil, errors.New(AlreadyAppliedSentence)
	}
	applicationID := staffApplicationPrefix + id.NewShortId()
	// A NEW ID, AND STILL FENCED ON "ROW ABSENT": the write runs at internal
	// origin, which the row gate does not owner-check, so it must never land on
	// a row that exists.
	fenced := memql.ContextWithRowVersionFence(auth.ContextWithUserActor(ctx, owner), applicationConcept, applicationID, time.Time{})
	if err := p.write(fenced, "createApplicationRow", map[string]any{
		"applicationId":  applicationID,
		"ownerUserId":    owner,
		"storeId":        storeID,
		"siteId":         siteID,
		"companyName":    company,
		"applicantName":  name,
		"applicantEmail": email,
		"accountId":      trimmed(site["accountId"]),
		"source":         SourceStaff,
		"addedBy":        userRef(addedBy),
		"submittedAt":    happenedStamp(p.clock()),
	}); err != nil {
		return nil, err
	}
	return confirm("wholesale:buyer:added", map[string]any{"applicationId": applicationID})
}

// sameAddress is THE ONE RULE of when two applications are of the same address
// (rulings 59 and 61): compared trimmed and lower-cased. The staff add, the
// storefront form and the approve decision all ask it, through
// applicationOfAddress; there is no second rule. An empty address is no address
// and matches nothing.
func sameAddress(a, b string) bool {
	x := strings.ToLower(strings.TrimSpace(a))
	return x != "" && x == strings.ToLower(strings.TrimSpace(b))
}

// openApplicationOf is the id of an OPEN application of email's address on the
// store -- submitted, in whatever stage, or approved -- or "" when there is
// none. A rejected or revoked application is final and blocks nothing: a new
// application is how somebody tries again.
func (p *Provider) openApplicationOf(ctx context.Context, storeID, email string) (string, error) {
	return p.applicationOfAddress(ctx, storeID, email, "", StateSubmitted, StateApproved)
}

// applicationOfAddress is the id of an application on the store, other than
// exceptID, whose address is email's (sameAddress) and whose derived state is
// one of states; "" when there is none. The store id is matched in every form
// it is held in.
//
// Every one of the store's applications is read, with no cap, as the system:
// a duplicate the check could not see would split one person's history, and
// an application the caller cannot read still counts. It answers an id, which
// its callers use only to name the application to the store's own people.
func (p *Provider) applicationOfAddress(ctx context.Context, storeID, email, exceptID string, states ...string) (string, error) {
	sys := auth.ContextWithSystemActor(ctx, duplicateCheckActor)
	apps, _, err := p.storeApplications(sys, storeID, false)
	if err != nil {
		return "", err
	}
	for _, app := range apps {
		id := trimmed(app["id"])
		if !sameAddress(asString(app["applicantEmail"]), email) || (exceptID != "" && bareID(id) == bareID(exceptID)) {
			continue
		}
		state, err := p.stateOf(sys, id)
		if err != nil {
			return "", err
		}
		for _, s := range states {
			if state == s {
				return id, nil
			}
		}
	}
	return "", nil
}

// removeBuyer answers wholesaleRemoveBuyer: a revoke of an approved buyer,
// recorded by the caller.
//
// The order of wholesaleRecordDecision's checks: the organization gate admitted
// the caller for the application; the caller is a person who is not one of the
// cluster's operators; the application is read under the caller's own actor,
// so one they cannot read is answered as one that does not exist; its state is
// folded from the whole log as the system.
func (p *Provider) removeBuyer(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	decider, err := deciderOf(ctx)
	if err != nil {
		return nil, err
	}
	applicationID := trimmed(args["applicationId"])
	if applicationID == "" {
		return nil, fmt.Errorf("wholesale: applicationId is required")
	}
	app, err := p.applicationRow(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	state, err := p.stateOf(auth.ContextWithSystemActor(ctx, provisionActor), applicationID)
	if err != nil {
		return nil, err
	}
	if state != StateApproved {
		return nil, errors.New("wholesale: only an approved buyer can be removed")
	}
	if err := p.appendDecision(ctx, app, TransitionRevoke, asString(args["note"])); err != nil {
		return nil, err
	}
	// A revoked application's stage is its state, whatever its terms.
	return decisionReceipt(applicationID, app, TransitionRevoke, decider, false, false)
}
