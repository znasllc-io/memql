package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// capabilities.go -- the DSL-callable surface.
//
// # Why starting a send is a BUILTIN and not a mutation
//
// Everything else an operator does to a campaign is a plain mutation, and
// that is the right default. Starting a send is the case where it is not,
// for two reasons that a mutation body cannot express:
//
//  1. A PREFLIGHT that spans several rows. Is a sender configured on this
//     node? Is one-click unsubscribe configured? Is the template marked
//     ready? Is the audience non-empty and inside the ceiling? Each is a
//     read, and a mutation cannot read. Discovering any of them 4000
//     messages in is strictly worse than refusing at the button.
//
//  2. TWO WRITES, one of which crosses an authorization boundary. The
//     campaign moves to `sending` (the operator's own row) and a
//     clusterOwner-tier send job is created (the engine's row). One write
//     per mutation body is the language's contract, and the usual answer
//     -- an automation on the first write -- does not work here: the
//     automation actor is neither the owner nor a cluster owner, so it
//     could not write the job, and making the job writable by it would
//     mean making it writable by anyone.
//
// The authorization is the FIRST read. `campaignById` is owned-tier, so
// it returns a row only to its owner; a caller who cannot read the
// campaign gets "not found" and nothing further happens. Every value that
// ends up on the send job -- owner, audience, template -- is copied off
// THAT row rather than taken from an argument, so the job can only ever
// name a user the caller could already act as.

// IntegrationName identifies the provider.
func (w *Worker) IntegrationName() string { return "campaigns" }

// Capabilities returns the DSL-callable operations.
func (w *Worker) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "startSend",
			Description: "Preflight and start a campaign send. Refuses rather than partially sending when the sender, one-click unsubscribe, template or audience is not ready.",
			Handler:     w.handleStartSend,
			ArgsSchema: map[string]string{
				"campaignId": "string (required) - the campaign to send",
			},
		},
		{
			Name:        "pauseSend",
			Description: "Pause a running campaign send. The delivery ledger is untouched, so resuming continues where it stopped.",
			Handler:     w.handlePauseSend,
			ArgsSchema: map[string]string{
				"campaignId": "string (required) - the campaign to pause",
			},
		},
		{
			Name:        "resumeSend",
			Description: "Resume a paused campaign send.",
			Handler:     w.handleResumeSend,
			ArgsSchema: map[string]string{
				"campaignId": "string (required) - the campaign to resume",
			},
		},
		{
			Name:        "suppress",
			Description: "Add an address to the cluster-wide do-not-mail list. Admin only: the list spans every operator. The address is digested, never stored.",
			Handler:     w.handleSuppress,
			ArgsSchema: map[string]string{
				"email":  "string (required) - the address to suppress",
				"reason": "string (optional) - unsubscribed | hard_bounce | complaint | manual (default manual)",
				"note":   "string (optional) - provenance, never the address",
			},
		},
		{
			Name:        "recordFeedback",
			Description: "Record a bounce or complaint report from the mail provider. Admin only. A hard bounce or complaint suppresses the address cluster-wide; a soft bounce is recorded and does not.",
			Handler:     w.handleRecordFeedback,
			ArgsSchema: map[string]string{
				"email":      "string (required) - the address the provider reported",
				"kind":       "string (required) - hard_bounce | soft_bounce | complaint",
				"campaignId": "string (optional) - the campaign the report came from",
				"note":       "string (optional) - the provider's classification",
			},
		},
	}
}

func (w *Worker) handleStartSend(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	campaignID := memql.BareShortId(strings.TrimSpace(argString(args, "campaignId")))
	if campaignID == "" {
		return nil, fmt.Errorf("campaigns.startSend: campaignId is required")
	}
	actorID := strings.TrimSpace(callerUserID(ctx))
	if actorID == "" {
		return nil, fmt.Errorf("campaigns.startSend: no caller identity; a send is always started by somebody")
	}

	// THE AUTHORIZATION. Owned-tier read under the CALLER's own context:
	// a campaign the caller does not own is simply not found.
	campaign, found, err := w.store.CampaignByID(ctx, campaignID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.startSend: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("campaigns.startSend: campaign %q not found", campaignID)
	}
	switch campaign.Status {
	case "sending":
		return nil, fmt.Errorf("campaigns.startSend: campaign is already sending; use resumeSend after a pause")
	case "sent":
		return nil, fmt.Errorf("campaigns.startSend: campaign has already been sent. Re-sending means authoring a new campaign -- " +
			"the delivery ledger is per (campaign, recipient), so a second run of the same campaign would find every recipient already terminal and mail nobody")
	case "cancelled":
		return nil, fmt.Errorf("campaigns.startSend: campaign is cancelled")
	}

	if reason := w.cfg.RequireUnsubscribe(); reason != "" {
		return nil, fmt.Errorf("campaigns.startSend: %s", reason)
	}
	if w.resolveSender() == nil {
		return nil, fmt.Errorf("campaigns.startSend: no email sender is registered on this node, so nothing could deliver the campaign")
	}

	tmpl, found, err := w.store.TemplateByID(ctx, campaign.TemplateID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.startSend: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("campaigns.startSend: template %q is not readable", campaign.TemplateID)
	}
	// `ready` is an operator asserting the copy is finished. Refusing a
	// draft is the cheapest guard there is against the single most
	// expensive mistake in this domain -- half-written copy delivered to
	// an entire audience, which is unrecallable.
	if tmpl.Status != "ready" {
		return nil, fmt.Errorf("campaigns.startSend: template %q is %q, not \"ready\". Mark it ready once the copy is final", tmpl.ID, tmpl.Status)
	}

	roster, err := w.store.Roster(ctx, campaign.AudienceID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.startSend: %w", err)
	}
	if len(roster) == 0 {
		return nil, fmt.Errorf("campaigns.startSend: audience %q has no recipients", campaign.AudienceID)
	}
	if len(roster) >= w.cfg.MaxAudience {
		return nil, fmt.Errorf(
			"campaigns.startSend: audience %q has %d recipients, at or over the %d ceiling (MEMQL_CAMPAIGNS_MAX_AUDIENCE). "+
				"The send reads the roster whole on every batch, so it would silently mail a prefix. Split the audience, or raise the "+
				"ceiling together with the paginate bound on audienceRosterForSend",
			campaign.AudienceID, len(roster), w.cfg.MaxAudience)
	}

	// Job first, campaign second. If the process dies between the two,
	// the job exists and the campaign is not `sending` -- the worker reads
	// that as "not authorized to send" and pauses the job, which is inert.
	// The other order would leave a campaign marked `sending` with no job,
	// which looks like a stalled send and has nothing to resume.
	if err := w.store.EnqueueSend(w.systemActorContext(ctx), SendJob{
		CampaignID:          campaign.ID,
		CampaignOwnerUserID: campaign.OwnerUserID,
		AudienceID:          campaign.AudienceID,
		TemplateID:          campaign.TemplateID,
	}); err != nil {
		return nil, fmt.Errorf("campaigns.startSend: %w", err)
	}
	if err := w.store.SetCampaignStatus(ctx, "startCampaign", campaign.ID); err != nil {
		return nil, fmt.Errorf("campaigns.startSend: %w", err)
	}

	return resultNode("campaignSendStarted", map[string]any{
		"campaignId": campaign.ID,
		"audienceId": campaign.AudienceID,
		"recipients": len(roster),
		"status":     "sending",
	})
}

func (w *Worker) handlePauseSend(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return w.transition(ctx, args, "pauseCampaign", "paused")
}

func (w *Worker) handleResumeSend(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return w.transition(ctx, args, "resumeCampaign", "queued")
}

// transition flips the campaign under the caller's actor (the ownership
// check) and then the job under the engine's. The campaign write is
// FIRST here, unlike startSend: the worker treats the campaign as
// authoritative over the job, so a crash between the two self-heals on
// the next tick.
func (w *Worker) transition(ctx context.Context, args map[string]any, mutation, jobStatus string) ([]memorynodes.MemoryNode, error) {
	campaignID := memql.BareShortId(strings.TrimSpace(argString(args, "campaignId")))
	if campaignID == "" {
		return nil, fmt.Errorf("campaigns.%s: campaignId is required", mutation)
	}
	if _, found, err := w.store.CampaignByID(ctx, campaignID); err != nil {
		return nil, fmt.Errorf("campaigns.%s: %w", mutation, err)
	} else if !found {
		return nil, fmt.Errorf("campaigns.%s: campaign %q not found", mutation, campaignID)
	}
	if err := w.store.SetCampaignStatus(ctx, mutation, campaignID); err != nil {
		return nil, fmt.Errorf("campaigns.%s: %w", mutation, err)
	}
	status := jobStatus
	if err := w.store.UpdateJob(w.systemActorContext(ctx), campaignID, SendJobPatch{Status: &status}); err != nil {
		// Not fatal: the worker reconciles from the campaign row, which
		// has already been written.
		w.logger.Warn("campaigns: campaign transitioned but the send job did not", "campaign", campaignID, "error", err)
	}
	return resultNode("campaignSendTransition", map[string]any{
		"campaignId": campaignID,
		"jobStatus":  jobStatus,
	})
}

func (w *Worker) handleSuppress(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireAdmin(ctx, "campaigns.suppress"); err != nil {
		return nil, err
	}
	addr := argString(args, "email")
	reason := strings.TrimSpace(argString(args, "reason"))
	if reason == "" {
		reason = "manual"
	}
	if !validSuppressionReason(reason) {
		return nil, fmt.Errorf("campaigns.suppress: reason %q is not one of unsubscribed / hard_bounce / complaint / manual", reason)
	}
	digest := EmailDigest(addr)
	if digest == "" {
		return nil, fmt.Errorf("campaigns.suppress: %q is not a usable email address", redactAddress(addr))
	}
	if err := w.store.RecordSuppression(w.systemActorContext(ctx), digest, reason, EmailDomain(addr), "", argString(args, "note")); err != nil {
		return nil, fmt.Errorf("campaigns.suppress: %w", err)
	}
	return resultNode("campaignSuppression", map[string]any{
		"suppressed": true,
		"reason":     reason,
		"domain":     EmailDomain(addr),
	})
}

// handleRecordFeedback is the bounce / complaint ingestion point.
//
// # What a hard bounce does to an audience membership
//
// The decision, stated once here because it is the question the issue
// asks: a hard bounce SUPPRESSES the address cluster-wide and marks the
// recipient row `bounced`. It does NOT delete the membership.
//
// Deleting looks tidier and is wrong twice over. It destroys the audit
// trail -- an operator reviewing why their delivery rate fell finds a
// smaller audience and no explanation. And it makes the address
// RESURRECTABLE: the next CSV import re-adds it as a fresh `subscribed`
// row, and mailing a known-dead address is precisely what a reputation
// system punishes. Keeping the row means the audience's sendable count
// drops visibly and the reason is on the row; keeping the cluster
// suppression means a re-import cannot undo it, because the list is
// consulted at the point of send rather than at the point of import.
//
// A SOFT bounce does neither. It is a transient condition -- a full
// mailbox, a greylisting relay -- and suppressing on one is how a sender
// loses a real subscriber to a bad afternoon. Soft bounces are recorded
// and otherwise ignored; the per-recipient retry budget
// (MEMQL_CAMPAIGNS_MAX_ATTEMPTS) is what bounds them.
func (w *Worker) handleRecordFeedback(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireAdmin(ctx, "campaigns.recordFeedback"); err != nil {
		return nil, err
	}
	addr := argString(args, "email")
	kind := strings.TrimSpace(argString(args, "kind"))
	digest := EmailDigest(addr)
	if digest == "" {
		return nil, fmt.Errorf("campaigns.recordFeedback: %q is not a usable email address", redactAddress(addr))
	}

	var reason string
	switch kind {
	case "hard_bounce":
		reason = "hard_bounce"
	case "complaint":
		reason = "complaint"
	case "soft_bounce":
		// Recorded in the reply, deliberately not suppressed.
		return resultNode("campaignFeedback", map[string]any{
			"suppressed": false,
			"kind":       kind,
			"domain":     EmailDomain(addr),
			"reason":     "a soft bounce is transient; suppressing on one loses real subscribers to a temporary condition",
		})
	default:
		return nil, fmt.Errorf("campaigns.recordFeedback: kind %q is not one of hard_bounce / soft_bounce / complaint", kind)
	}

	campaignID := memql.BareShortId(strings.TrimSpace(argString(args, "campaignId")))
	if err := w.store.RecordSuppression(w.systemActorContext(ctx), digest, reason, EmailDomain(addr), campaignID, argString(args, "note")); err != nil {
		return nil, fmt.Errorf("campaigns.recordFeedback: %w", err)
	}
	return resultNode("campaignFeedback", map[string]any{
		"suppressed": true,
		"kind":       kind,
		"domain":     EmailDomain(addr),
	})
}

// --- helpers ------------------------------------------------------------

// requireAdmin gates the two cluster-wide writes.
//
// The suppression list spans every operator in the deployment, so adding
// to it is not an owner-scoped action and there is no owner tier that
// could express the gate. Admin-or-above is the tier that matches what
// the action actually is: deployment-level policy. An automated feedback
// pipeline (an ESP webhook landing on v1:platform:inboundRequest, then a
// DSL automation) reaches this by presenting an admin service-account
// credential -- the class="service_account" JWT this tree already mints --
// rather than by widening the gate.
func requireAdmin(ctx context.Context, op string) error {
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil {
		return fmt.Errorf("%s: no caller identity", op)
	}
	if auth.RoleLevel(ac.Role) > auth.RoleLevel(auth.RoleAdmin) {
		return fmt.Errorf("%s: the cluster-wide suppression list may only be written by an admin or the cluster owner; caller holds %q", op, ac.Role)
	}
	return nil
}

func callerUserID(ctx context.Context) string {
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil {
		return ""
	}
	return ac.UserId
}

func validSuppressionReason(reason string) bool {
	switch reason {
	case "unsubscribed", "hard_bounce", "complaint", "manual":
		return true
	}
	return false
}

// redactAddress keeps a rejected address out of an error string that will
// be logged. The domain is enough to diagnose a malformed import without
// putting a mailbox in a log line.
func redactAddress(addr string) string {
	if at := strings.LastIndex(addr, "@"); at > 0 && at < len(addr)-1 {
		return "***@" + addr[at+1:]
	}
	return "***"
}

func argString(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// resultNode wraps a builtin reply. Synthetic in the strict sense --
// never persisted, and a `concept` string outside the
// v{major}:{domain}:{entity} grammar so nothing mistakes it for graph
// state (the integrationStatus precedent).
func resultNode(kind string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("campaigns: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        kind,
		Concept:   "integration:campaigns:" + kind,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   body,
	}}, nil
}
