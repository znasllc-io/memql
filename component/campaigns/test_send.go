package campaigns

import (
	"context"
	"errors"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// test_send.go -- one real message to one named address (memql#4822, design
// D11).
//
// # What it is FOR, stated before what it does
//
// The merge-tag set is a closed list resolved per recipient, and an unknown
// tag stays LITERAL in the body rather than resolving to nothing. That is the
// right behaviour -- a lookup that silently returned empty would put blanks
// in a body nobody proofread -- but it means a typo'd {{fields.compnay}} is
// invisible until it reaches the whole audience. This is what makes it
// visible first: it returns the tags it could not resolve, in a message the
// operator can also just read.
//
// # It writes NOTHING, and each omission is deliberate
//
//	no delivery row   the ledger is per (campaign, recipient) and is what
//	                  decides who gets mailed. A test row would make the
//	                  campaign look partly sent and would REMOVE whoever the
//	                  synthetic recipient collided with from the real send.
//	no counters       sentCount is what an operator reads to know how a send
//	                  went; a test that moved it would make the figure a lie
//	                  in the direction of "more went out than did".
//	no engagement     the token would name a delivery that does not exist.
//	no consent        nobody granted or withdrew anything.
//
// It DOES consume the ordinary send-rate token bucket, because it is a real
// message to a real mailbox and the provider counts it. A test send that
// bypassed the limiter would let a burst of them trip the throttle a campaign
// is then blamed for.
//
// # It deliberately does NOT go through sendableStatus
//
// A campaign that has already been SENT must still be test-sendable: the
// commonest moment to want one is while duplicating last month's campaign, or
// while diagnosing a complaint about a message that already went out.
// sendableStatus exists to stop a SEND starting from a state that would
// mis-drive the ledger, and this touches the ledger not at all -- so applying
// it would refuse the case the feature is most wanted in, for a reason that
// does not hold here.
//
// The gate is instead the one every campaign-scoped builtin uses: the
// composite-tier read of the campaign. A caller who cannot read it cannot
// test-send it.
//
// # `to` is REQUIRED and never defaults to the caller
//
// A builtin that mails somewhere you did not name is one you have to remember
// the default of. The address is validated for shape before anything is
// rendered, so a typo is a refusal rather than a bounce against the sending
// identity's reputation.

// testSendSubjectPrefix marks the message in the operator's own inbox. In the
// SUBJECT rather than a header, because the whole point is that a person
// looking at a list of messages can tell which one is the test.
const testSendSubjectPrefix = "[Test] "

// testSendDisplayName is the synthetic recipient's name. A fixed, obviously
// synthetic value: rendering the CALLER's own name would make the test look
// more like a real send than it is, and the tag being visibly a placeholder
// is what tells an operator the greeting is personalized at all.
const testSendDisplayName = "Test Recipient"

func (w *Worker) handleTestSend(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	campaignID := memql.BareShortId(strings.TrimSpace(argString(args, "campaignId")))
	if campaignID == "" {
		return nil, errors.New("campaigns.testSend: campaignId is required")
	}
	to := NormalizeEmail(argString(args, "to"))
	if to == "" || !plausibleAddress(to) {
		return nil, fmt.Errorf(
			"campaigns.testSend: %q is not a usable email address. `to` is required and never defaults "+
				"to the caller's own address -- a builtin that mails somewhere you did not name is one "+
				"you have to remember the default of", redactAddress(argString(args, "to")))
	}

	// THE AUTHORIZATION: a composite-tier read under the caller's own actor.
	campaign, found, err := w.store.CampaignByID(ctx, campaignID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.testSend: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("campaigns.testSend: campaign %q not found", campaignID)
	}
	if reason := w.cfg.RequireUnsubscribe(); reason != "" {
		// The same precondition a real send has. A test message is a real
		// message to a real mailbox, and one without a working opt-out is the
		// thing this deployment refuses to emit.
		return nil, fmt.Errorf("campaigns.testSend: %s", reason)
	}
	if w.resolveSender() == nil {
		return nil, errors.New("campaigns.testSend: no email sender is registered on this node")
	}

	tmpl, found, err := w.store.TemplateByID(ctx, campaign.TemplateID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.testSend: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("campaigns.testSend: template %q is not readable", campaign.TemplateID)
	}
	// NO template-status check, unlike the send preflight. Refusing a draft
	// is what stops half-written copy reaching an audience; a test send to
	// one address is how the copy gets finished, so requiring `ready` here
	// would forbid the one use the feature exists for.

	identity, refusal := w.resolveSendIdentity(ctx, campaign)
	if refusal.refused() {
		// Both arms are an error here, terminal or not: the caller is waiting
		// on an answer, and "it will retry" is not one -- nothing retries a
		// test send.
		return nil, fmt.Errorf("campaigns.testSend: %s", refusal.Reason)
	}

	recipient := w.syntheticRecipient(ctx, campaign, to)
	accountName, err := w.store.AccountName(ctx, campaign.AccountID)
	if err != nil {
		w.logger.Debug("campaigns.testSend: could not resolve the account name", "campaign", campaign.ID, "error", err)
	}

	msg, err := renderMessage(campaign, tmpl, recipient, unsubscribeURL(w.cfg.UnsubscribeBaseURL, inertUnsubscribeToken), RenderOptions{
		ReplyTo:       replyToFor(campaign, identity),
		AccountName:   accountName,
		SubjectPrefix: testSendSubjectPrefix,
		// Tracking is left ZERO. There is no delivery row to attribute a hit
		// to, so a pixel here would record against an id nobody holds -- and
		// the operator's own open would land in the campaign's numbers.
	})
	if err != nil {
		return nil, fmt.Errorf("campaigns.testSend: %w", err)
	}

	// OUR rate limit, same bucket as a campaign's. A test is a real message
	// and the provider counts it.
	if !w.limiter.Allow() {
		return nil, errors.New("campaigns.testSend: the send-rate limit is exhausted; try again shortly. " +
			"A test send consumes the same token bucket a campaign does, because the provider counts it the same way")
	}
	sendCtx, cancel := context.WithTimeout(ctx, w.cfg.SendTimeout)
	err = w.deliver(sendCtx, msg, identity.SendAs)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("campaigns.testSend: %w", err)
	}

	unresolved := UnresolvedMergeTags(msg.Subject, msg.TextBody, msg.HTMLBody)
	return resultNode("campaignTestSend", map[string]any{
		"campaignId":     campaign.ID,
		"to":             to,
		"sentAs":         identity.SendAs.Address,
		"subject":        msg.Subject,
		"unresolvedTags": unresolved,
		"fieldsFrom":     recipient.ID,
	})
}

// syntheticRecipient builds the stand-in the template renders against.
//
// `fields` are borrowed from the audience's FIRST REAL RECIPIENT when one
// exists, and that is the detail that makes the feature work. A synthetic
// recipient with an empty fields map renders every {{fields.*}} tag as
// unresolved -- so the report would flag the operator's CORRECT tags as
// typos, which is worse than not reporting at all. Borrowing real shape means
// the tags that come back are the ones no column supplies.
//
// The ID is the borrowed recipient's, or "" when the audience is empty. It is
// returned to the caller as `fieldsFrom` so the operator can tell whether the
// values they are looking at are real.
func (w *Worker) syntheticRecipient(ctx context.Context, campaign Campaign, to string) Recipient {
	recipient := Recipient{Email: to, DisplayName: testSendDisplayName, SubscriptionStatus: "subscribed"}
	page, _, err := w.store.RosterPage(ctx, campaign.AudienceID, "")
	if err != nil || len(page) == 0 {
		if err != nil {
			w.logger.Debug("campaigns.testSend: could not read the audience for field shape",
				"campaign", campaign.ID, "error", err)
		}
		return recipient
	}
	first := page[0]
	recipient.ID = first.ID
	recipient.Fields = first.Fields
	return recipient
}

// inertUnsubscribeToken is what the test message's footer and List-Unsubscribe
// header carry.
//
// OBVIOUSLY INERT, and signed by nothing. A test send must still render the
// unsubscribe surface -- it is most of what an operator is checking -- but a
// REAL token would name a real recipient and a real campaign, and clicking it
// out of curiosity would opt somebody out of a campaign that has not been
// sent yet. The endpoint refuses this string like any other unverifiable
// token and shows the "link not valid" page, which is the honest outcome for
// a link in a message that was never really addressed to anyone.
const inertUnsubscribeToken = "test-send-token-not-valid"
