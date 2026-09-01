package campaigns

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/integrations/email"
)

// identity.go -- which mailbox a campaign leaves from (memql#4821, design
// D3-D5, D8).
//
// # The engine never infers an identity, and that is the whole rule
//
// Resolution is exactly two steps, in order:
//
//	campaign.senderIdentityId   the operator SAID which mailbox
//	otherwise                   the env-configured default sender
//
// There is no third step, and in particular there is no "the campaign has an
// accountId, so use that account's identity". The app prefills the picker
// from the account's identities, which is UX; resolution is explicit. An
// inferred identity is one nobody chose, and the consequence of getting it
// wrong is a client's list mailed from another client's mailbox -- which is
// not a bug an operator can find by looking at the campaign, because the
// campaign says nothing about it.
//
// # A missing or disabled identity is REFUSED, never silently defaulted
//
// The tempting failure mode is to fall back to the default sender when the
// named identity does not resolve. It is the wrong answer for the same reason
// inference is: the operator asked for a specific mailbox, and mailing from a
// different one is worse than not mailing at all. A campaign naming a
// disabled identity is an authoring mistake somebody has to fix; the send
// stops and says so.
//
// # The environment-versus-authoring split, applied here
//
// schedule.go states the split at length and this obeys it exactly, because
// the two failures have different fixes and therefore need different
// outcomes:
//
//	AUTHORING   the identity row is missing, or its status is `disabled`.
//	            Nothing about the cluster changes that. The campaign fails
//	            with the reason on its own row and the operator repoints or
//	            re-enables it.
//	ENVIRONMENT the read itself failed -- an engine still coming up, a
//	            database blip. The campaign is fine. Failing it would make an
//	            operator re-author a schedule to recover from a bad deploy,
//	            so the send waits and retries with the reason stamped where
//	            the portal shows it.
//
// Getting this backwards in either direction is expensive. Treating a
// missing row as environmental means a campaign that retries forever and
// never says why in terms the operator can act on; treating a read error as
// authoring means one slow query destroys a scheduled send.
//
// # Why the identity is resolved THREE times
//
// Once at authoring time (Worker.preflight), once at fire time
// (Worker.fireTimePreflight) and once on the drain path (Worker.sendBatch).
// That is not redundancy -- each covers a window the others cannot:
//
//	preflight          the operator is looking at the screen. Catching it
//	                   here is the difference between a refusal they can fix
//	                   now and one they find tomorrow.
//	fireTimePreflight  hours have passed since a schedule was committed, and
//	                   an identity can be disabled in between.
//	sendBatch          the IMMEDIATE-START path never went through the
//	                   scheduler at all, so without this check a campaign
//	                   started by hand would resolve its identity nowhere
//	                   before the first message left.
//
// The asymmetry is real: campaignStartSend runs preflight and then enqueues a
// job the drain worker picks up, and between those two moments nothing
// re-reads the identity. sendBatch is the only place that covers it.

// resolvedIdentity is the outcome of resolving one campaign's sending
// mailbox.
type resolvedIdentity struct {
	// SendAs is what the transport is handed. The zero value means "the
	// configured default", which is what an unnamed identity resolves to and
	// what every non-campaign caller in the tree passes.
	SendAs email.SendAs

	// Label is the reputation and warmup key for this send (design D8): the
	// resolved identity's normalized address, or the env-derived value when
	// the default mailbox is in play. It is a KEY, not an address to mail
	// from -- nothing puts it on a header.
	Label string

	// ReplyTo is the identity's default Reply-To, used only when the
	// campaign sets none of its own. A campaign's own value always wins,
	// because it is the more specific statement.
	ReplyTo string
}

// identityRefusal is a resolution failure classified by WHO CAN FIX IT.
type identityRefusal struct {
	Reason string
	// Terminal marks an AUTHORING problem: retrying changes nothing, so the
	// campaign fails rather than waiting. See the file doc.
	Terminal bool
}

func (r identityRefusal) refused() bool { return r.Reason != "" }

// resolveSendIdentity resolves the mailbox a campaign sends as.
//
// ownerCtx must carry the CAMPAIGN OWNER'S actor: senderIdentity is
// composite-tier, so the read answers with the owner's authority and no more.
// The send path already holds that context for every other owned read it
// makes, which is what keeps this from being a new privilege.
func (w *Worker) resolveSendIdentity(ownerCtx context.Context, campaign Campaign) (resolvedIdentity, identityRefusal) {
	// The DEFAULT mailbox. A campaign that names no identity still gets its
	// own fromName onto the From header (design D6): a SendAs carrying only
	// a display name means "the configured mailbox, under this name", which
	// is exactly what a campaign overriding fromName and nothing else asks
	// for. Before this the field was authored, stored, documented -- and
	// never reached a header.
	if strings.TrimSpace(campaign.SenderIdentityID) == "" {
		return resolvedIdentity{
			SendAs: email.SendAs{FromName: strings.TrimSpace(campaign.FromName)},
			Label:  w.cfg.SendingIdentityFor(""),
		}, identityRefusal{}
	}

	identity, found, err := w.store.SenderIdentityByID(ownerCtx, campaign.SenderIdentityID)
	if err != nil {
		return resolvedIdentity{}, identityRefusal{
			Reason: fmt.Sprintf(
				"could not read sending identity %q: %v; the campaign is fine and the send will retry",
				campaign.SenderIdentityID, err),
			Terminal: false,
		}
	}
	if !found {
		return resolvedIdentity{}, identityRefusal{
			Reason: fmt.Sprintf(
				"sending identity %q is not readable, so there is no mailbox to send as. "+
					"The send is refused rather than falling back to the default sender: the campaign names a "+
					"specific mailbox, and mailing a list from a different one is worse than not mailing it. "+
					"Point the campaign at an identity that exists, or clear the field to use the configured default",
				campaign.SenderIdentityID),
			Terminal: true,
		}
	}
	if identity.Disabled() {
		return resolvedIdentity{}, identityRefusal{
			Reason: fmt.Sprintf(
				"sending identity %q (%s) is disabled. A disabled identity is a mailbox an operator retired, "+
					"so the send is refused rather than silently falling back to the default sender -- a silent "+
					"fallback mails a client's list from the wrong mailbox and nothing says so. Re-enable it, or "+
					"point the campaign at another identity",
				identity.ID, redactAddress(identity.Address)),
			Terminal: true,
		}
	}

	// fromName precedence, most specific first: the campaign's own override,
	// then the identity's. The identity's is REQUIRED by the schema, so the
	// only way to reach the transport's own default from here is a row that
	// predates the requirement -- and the transport's default is what the
	// zero value already means.
	fromName := strings.TrimSpace(campaign.FromName)
	if fromName == "" {
		fromName = strings.TrimSpace(identity.FromName)
	}
	return resolvedIdentity{
		SendAs:  email.SendAs{Address: strings.TrimSpace(identity.Address), FromName: fromName},
		Label:   w.cfg.SendingIdentityFor(identity.Address),
		ReplyTo: strings.TrimSpace(identity.ReplyTo),
	}, identityRefusal{}
}

// replyToFor picks the Reply-To one message carries. The campaign's own value
// wins over the identity's default, because it is the more specific
// statement; an empty result leaves the header off entirely, which means
// replies go to the sending mailbox.
func replyToFor(campaign Campaign, identity resolvedIdentity) string {
	if v := strings.TrimSpace(campaign.ReplyTo); v != "" {
		return v
	}
	return strings.TrimSpace(identity.ReplyTo)
}
