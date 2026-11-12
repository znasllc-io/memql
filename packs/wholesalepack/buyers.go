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
// Both are gated by the wholesale part (the organization gate): adding is
// judged against the SITE's organization, which must serve the store
// (siteForStore); removing against the APPLICATION's, which the caller must
// then read under their own actor. The cluster's operators add and remove
// nobody, refused as a decision is (deciderOf): the store holds who its buyers
// are.

// AlreadyAppliedSentence refuses adding an address that has a submitted or an
// approved application on the store.
const AlreadyAppliedSentence = "This address already has an application on this store."

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
// rules. Then the site must serve the store, the store's documents must exist
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
	taken, err := p.addressHasOpenApplication(ctx, storeID, email)
	if err != nil {
		return nil, err
	}
	if taken {
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
	}); err != nil {
		return nil, err
	}
	return confirm("wholesale:buyer:added", map[string]any{"applicationId": applicationID})
}

// addressHasOpenApplication reports whether email, trimmed and lower-cased, has
// a submitted or an approved application on the store, in any form the store id
// is held in. A rejected or revoked application is final and blocks nothing: a
// new application is how somebody tries again.
//
// Every one of the store's applications is read, with no cap, as the system:
// a duplicate the check could not see would split one person's history.
func (p *Provider) addressHasOpenApplication(ctx context.Context, storeID, email string) (bool, error) {
	sys := auth.ContextWithSystemActor(ctx, duplicateCheckActor)
	apps, _, err := p.storeApplications(sys, storeID, false)
	if err != nil {
		return false, err
	}
	want := strings.ToLower(strings.TrimSpace(email))
	for _, app := range apps {
		if strings.ToLower(trimmed(app["applicantEmail"])) != want {
			continue
		}
		state, err := p.stateOf(sys, trimmed(app["id"]))
		if err != nil {
			return false, err
		}
		if state == StateSubmitted || state == StateApproved {
			return true, nil
		}
	}
	return false, nil
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
