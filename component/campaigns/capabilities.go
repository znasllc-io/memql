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
			Description: "Preflight and start a campaign send. Refuses rather than partially sending when the sender, one-click unsubscribe, template, audience or product_purchasable catalog is not ready.",
			Handler:     w.handleStartSend,
			ArgsSchema: map[string]string{
				"campaignId": "string (required) - the campaign to send",
			},
		},
		{
			Name:        "scheduleSend",
			Description: "Commit a campaign to a time and enqueue the send job that fires at it. Runs the same preflight as startSend, so a schedule that could never send is refused now rather than at 3am.",
			Handler:     w.handleScheduleSend,
			ArgsSchema: map[string]string{
				"campaignId":  "string (required) - the campaign to schedule",
				"scheduledAt": "string (required) - RFC 3339 instant the send should begin at",
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
			Name:        "ingestFeedback",
			Description: "Read a provider's bounce / complaint webhook off a staged v1:platform:inboundRequest row and apply it. Gated by configuration rather than by role: the source must be listed in MEMQL_CAMPAIGNS_FEEDBACK_SOURCES and the delivery must have been signature-verified.",
			Handler:     w.handleIngestFeedback,
			ArgsSchema: map[string]string{
				"inboundRequestId": "string (required) - the staged v1:platform:inboundRequest row to read",
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
	if err := sendableStatus("startSend", campaign.Status); err != nil {
		return nil, err
	}
	recipients, err := w.preflight(ctx, "startSend", campaign)
	if err != nil {
		return nil, err
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
		"recipients": recipients,
		"status":     "sending",
	})
}

// handleScheduleSend commits a campaign to a time (memql#3459).
//
// # Why this is a builtin rather than the scheduleCampaign mutation alone
//
// The mutation writes the operator's intended time onto their own row, and
// before this it wrote nothing else -- which is exactly why a scheduled
// campaign never fired. Something has to record the intent where the ENGINE
// can find it, and "find it" is the hard word: `campaign` is owned-tier, so
// no actor can scan across operators for due rows. The engine's own
// clusterOwner-tier send job is the only row a worker can find without
// already knowing whose it is, so the schedule is written there, by Go
// holding a campaign it read under the caller's actor.
//
// # The preflight runs NOW, not at the scheduled time
//
// Both, in fact -- the worker re-checks at fire time, because hours pass. But
// running it here is the point of the feature: a schedule whose template is
// still a draft, whose audience is empty or whose node has no configured
// sender is a send that was never going to happen, and the operator finds
// that out while they are looking at the screen rather than the next morning.
func (w *Worker) handleScheduleSend(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	campaignID := memql.BareShortId(strings.TrimSpace(argString(args, "campaignId")))
	if campaignID == "" {
		return nil, fmt.Errorf("campaigns.scheduleSend: campaignId is required")
	}
	raw := strings.TrimSpace(argString(args, "scheduledAt"))
	if raw == "" {
		return nil, fmt.Errorf("campaigns.scheduleSend: scheduledAt is required; to send now, use campaignStartSend")
	}
	when, err := parseScheduledAt(raw)
	if err != nil {
		return nil, fmt.Errorf("campaigns.scheduleSend: %w", err)
	}
	if strings.TrimSpace(callerUserID(ctx)) == "" {
		return nil, fmt.Errorf("campaigns.scheduleSend: no caller identity; a send is always scheduled by somebody")
	}

	// THE AUTHORIZATION, identical to startSend's: an owned-tier read under
	// the caller's own context. A campaign the caller does not own is simply
	// not found, and every value that reaches the job is copied off that row.
	campaign, found, err := w.store.CampaignByID(ctx, campaignID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.scheduleSend: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("campaigns.scheduleSend: campaign %q not found", campaignID)
	}
	if err := sendableStatus("scheduleSend", campaign.Status); err != nil {
		return nil, err
	}

	// A time already past would fire on the very next tick, which is a
	// confusing way to spell "send now" and is far more often a typo in the
	// year or the timezone. The slack absorbs the round trip and a little
	// clock skew between the operator's browser and the node.
	if now := w.nowUTC(); when.Before(now.Add(-scheduleBackdateSlack)) {
		return nil, fmt.Errorf(
			"campaigns.scheduleSend: %s is in the past (it is now %s). To send immediately, use campaignStartSend",
			when.Format(time.RFC3339), now.Format(time.RFC3339))
	}

	recipients, err := w.preflight(ctx, "scheduleSend", campaign)
	if err != nil {
		return nil, err
	}

	// Job first, campaign second, for the reason startSend gives: a crash
	// between the two leaves a `scheduled` job under a campaign that is still
	// a draft, and the promotion refuses to fire from `draft`. Inert. The
	// other order would leave a campaign promising a send with no job behind
	// it, which is the shape this issue is about.
	if err := w.store.EnqueueSend(w.systemActorContext(ctx), SendJob{
		CampaignID:          campaign.ID,
		CampaignOwnerUserID: campaign.OwnerUserID,
		AudienceID:          campaign.AudienceID,
		TemplateID:          campaign.TemplateID,
		Status:              "scheduled",
		ScheduledAt:         when,
	}); err != nil {
		return nil, fmt.Errorf("campaigns.scheduleSend: %w", err)
	}
	if err := w.store.ScheduleCampaign(ctx, campaign.ID, when); err != nil {
		return nil, fmt.Errorf("campaigns.scheduleSend: %w", err)
	}

	return resultNode("campaignSendScheduled", map[string]any{
		"campaignId":  campaign.ID,
		"audienceId":  campaign.AudienceID,
		"recipients":  recipients,
		"scheduledAt": when.Format(time.RFC3339),
		"status":      "scheduled",
	})
}

// scheduleBackdateSlack is how far into the past a scheduled time may sit
// before it is read as a mistake rather than as "now".
const scheduleBackdateSlack = 5 * time.Minute

// parseScheduledAt accepts the two spellings a UI produces: a full RFC 3339
// instant, and the offset-less form an <input type="datetime-local"> emits.
// The second is read as UTC and said so in the builtin's documentation --
// guessing a local zone from a server's TZ would make the same string mean
// different moments on different nodes.
func parseScheduledAt(raw string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("scheduledAt %q is not an RFC 3339 instant (e.g. 2026-08-14T09:00:00Z)", raw)
}

// sendableStatus refuses the campaign states from which neither starting nor
// scheduling a send is meaningful. Shared so the two entry points cannot
// drift into disagreeing about which those are.
func sendableStatus(op, status string) error {
	switch status {
	case "sending":
		return fmt.Errorf("campaigns.%s: campaign is already sending; use resumeSend after a pause", op)
	case "sent":
		return fmt.Errorf("campaigns.%s: campaign has already been sent. Re-sending means authoring a new campaign -- "+
			"the delivery ledger is per (campaign, recipient), so a second run of the same campaign would find every recipient already terminal and mail nobody", op)
	case "cancelled":
		return fmt.Errorf("campaigns.%s: campaign is cancelled", op)
	}
	return nil
}

// preflight is the refusal set shared by starting a send and scheduling one,
// and the sharing is the point: a schedule that passes a WEAKER preflight
// than a manual start is a schedule that fails in the middle of the night
// with nobody watching.
//
// Every check is a read, which is why this lives in Go rather than in a
// mutation body. It runs under the CALLER'S context throughout, so the
// template and roster it consults are the ones the caller can actually see.
//
// Returns the recipient count the send will work through.
func (w *Worker) preflight(ctx context.Context, op string, campaign Campaign) (int, error) {
	if reason := w.cfg.RequireUnsubscribe(); reason != "" {
		return 0, fmt.Errorf("campaigns.%s: %s", op, reason)
	}
	if w.resolveSender() == nil {
		return 0, fmt.Errorf("campaigns.%s: no email sender is registered on this node, so nothing could deliver the campaign", op)
	}
	if reason := w.catalogRefusal(ctx); reason != "" {
		return 0, fmt.Errorf("campaigns.%s: %s", op, reason)
	}

	// THE IDENTITY, before anything else about the content (memql#4821).
	// Refusing at the button is the whole point of a preflight, and this is
	// the refusal an operator is most likely to hit while looking at the
	// screen: they picked a mailbox, somebody retired it, and the campaign
	// still names it. Both arms are an error here -- unlike at fire time,
	// where an unreadable identity waits -- because a caller pressing the
	// button is owed an answer now rather than a queued job that may or may
	// not resolve later.
	if _, refusal := w.resolveSendIdentity(ctx, campaign); refusal.refused() {
		return 0, fmt.Errorf("campaigns.%s: %s", op, refusal.Reason)
	}

	tmpl, found, err := w.store.TemplateByID(ctx, campaign.TemplateID)
	if err != nil {
		return 0, fmt.Errorf("campaigns.%s: %w", op, err)
	}
	if !found {
		return 0, fmt.Errorf("campaigns.%s: template %q is not readable", op, campaign.TemplateID)
	}
	// `ready` is an operator asserting the copy is finished. Refusing a
	// draft is the cheapest guard there is against the single most
	// expensive mistake in this domain -- half-written copy delivered to
	// an entire audience, which is unrecallable.
	if tmpl.Status != "ready" {
		return 0, fmt.Errorf("campaigns.%s: template %q is %q, not \"ready\". Mark it ready once the copy is final", op, tmpl.ID, tmpl.Status)
	}

	// A server-side COUNT, not the length of a read (memql#3460). The
	// difference is the whole issue: measuring a bounded page and calling it
	// a total is what made 5000 a ceiling, and a count query has no window
	// to be bounded by.
	size, err := w.store.RosterSize(ctx, campaign.AudienceID)
	if err != nil {
		return 0, fmt.Errorf("campaigns.%s: %w", op, err)
	}
	if size == 0 {
		return 0, fmt.Errorf("campaigns.%s: audience %q has no recipients", op, campaign.AudienceID)
	}
	if size > w.cfg.MaxAudience {
		return 0, fmt.Errorf(
			"campaigns.%s: audience %q has %d recipients, over the %d ceiling (MEMQL_CAMPAIGNS_MAX_AUDIENCE). "+
				"The ceiling is a deliberate refusal, not a technical bound -- the send pages through the roster, so no size is unsafe. "+
				"A send this large is more often a mis-scoped audience than an intent, and it cannot be recalled. Raise the ceiling if you mean it",
			op, campaign.AudienceID, size, w.cfg.MaxAudience)
	}
	return size, nil
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
