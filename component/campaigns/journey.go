package campaigns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
)

const journeyEnrollLag = 2 * time.Minute
const journeyDuePages = 5
const journeyMaxAttempts = 3

func journeyEnrollmentID(journeyID, recipientID string, joinedAt time.Time) string {
	return sha256Hex("campaign-journey-enrollment\x00" + bare(journeyID) + "\x00" + bare(recipientID) + "\x00" + joinedAt.UTC().Format(time.RFC3339Nano))
}
func journeyRequestID(enrollmentID, stepID string) string {
	return "journey-" + bare(enrollmentID) + "-" + stepID
}

func (w *Worker) drainJourneys(ctx, systemCtx context.Context) {
	systemCtx = auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(systemCtx))
	selected := map[string]map[string]any{}
	read := func(name string) (any, error) {
		ids := []string{}
		cursor := ""
		seen := map[string]bool{}
		for page := 0; page < journeyDuePages; page++ {
			rows, next, err := w.store.rowsPage(systemCtx, cursor, call("query", name))
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				key := name + ":" + str(row, "id")
				selected[key] = row
				ids = append(ids, key)
			}
			if next == "" {
				break
			}
			if seen[next] {
				return nil, fmt.Errorf("automation scan repeated a cursor")
			}
			seen[next] = true
			cursor = next
		}
		return ids, nil
	}
	operate := func(name string, op func(context.Context, context.Context, map[string]any) error) workflowhost.Operation {
		return func(ctx context.Context, a map[string]any) (any, error) {
			key := str(a, "id")
			row, ok := selected[key]
			if !ok || !strings.HasPrefix(key, name+":") {
				return nil, fmt.Errorf("automation outside this scan")
			}
			return nil, op(ctx, systemCtx, row)
		}
	}
	_, err := workflowhost.Run(ctx, "campaignJourneyDrainWorkflow", nil, workflowhost.Options{Logger: w.logger, Operations: map[string]workflowhost.Operation{
		"campaignJourneysToRetire": func(context.Context, map[string]any) (any, error) { return read("journeysToRetire") },
		"campaignActiveJourneys":   func(context.Context, map[string]any) (any, error) { return read("activeJourneys") },
		"campaignRetireJourney":    operate("journeysToRetire", w.retireJourney),
		"campaignRunJourney":       operate("activeJourneys", w.runJourney),
	}})
	if err != nil {
		w.logger.Warn("campaigns: automation drain failed", "error", err)
	}
}

// The journey gate is shared with editing and activation. A page is enrolled in
// order and stops on its first failed write, so the overlap watermark never
// advances beyond a person whose enrollment could not be recorded.
func (w *Worker) runJourney(ctx, systemCtx context.Context, scanned map[string]any) error {
	id := bare(str(scanned, "id"))
	if w.journeyGate == nil {
		return fmt.Errorf("automation coordination is unavailable")
	}
	hold, err := w.journeyGate(context.WithoutCancel(ctx), "journey:"+id)
	if err != nil {
		return err
	}
	err = func() error {
		defer hold.Release()
		row, err := w.newsletterRow(systemCtx, "journeyById", "journeyId", id)
		if err != nil || row == nil {
			return err
		}
		j, err := journeyFromRow(row)
		if err != nil {
			return err
		}
		if j.Status != "active" || len(j.Steps) == 0 || j.OwnerUserID == "" || j.ActivatedAt.IsZero() {
			return nil
		}
		actor := memql.ContextWithFreshRead(w.ownerActorContext(ctx, j.OwnerUserID))
		if err = w.requireJourneyOrganization(actor, j.AccountID); err != nil {
			return fmt.Errorf("journey owner=%q account=%q: %w", j.OwnerUserID, j.AccountID, err)
		}
		newest, err := w.store.rows(systemCtx, call("query", "newestJourneyEnrollment", arg{"journeyId", id}))
		if err != nil {
			return err
		}
		since := j.ActivatedAt
		if len(newest) > 0 {
			at := templateTime(newest[0]["triggeredAt"]).Add(-journeyEnrollLag)
			if at.After(since) {
				since = at
			}
		}
		query, field := "audienceJoinersSince", "joinedAt"
		if j.Trigger.Kind == "confirmed" {
			query, field = "audienceConfirmationsSince", "confirmedAt"
		}
		cursor := ""
		for {
			rows, next, err := w.store.rowsPage(systemCtx, cursor, call("query", query, arg{"audienceId", j.Trigger.AudienceID}, arg{"since", since.UTC().Format(time.RFC3339Nano)}))
			if err != nil {
				return err
			}
			for _, r := range rows {
				if !sameOrganization(j.AccountID, str(r, "accountId")) {
					continue
				}
				triggered := templateTime(r[field])
				if triggered.Before(j.ActivatedAt) || triggered.IsZero() {
					continue
				}
				enrollmentID := journeyEnrollmentID(id, str(r, "id"), templateTime(r["joinedAt"]))
				existing, err := w.newsletterRow(systemCtx, "journeyEnrollmentById", "enrollmentId", enrollmentID)
				if err != nil {
					return err
				}
				if existing != nil {
					continue
				}
				digest := str(r, "emailDigest")
				if digest == "" {
					continue
				} // The digest sweep repairs pre-feature rows; consent identity is never guessed.
				if err = w.journeyWrite(actor, hold, "enrollJourneyRecipient", map[string]any{"enrollmentId": enrollmentID, "journeyId": id, "accountId": j.AccountID, "recipientId": bare(str(r, "id")), "emailDigest": digest, "stepId": j.Steps[0].ID, "enrolledAt": w.nowUTC().Format(time.RFC3339Nano), "triggeredAt": triggered.Format(time.RFC3339Nano)}); err != nil {
					return err
				}
			}
			if next == "" {
				break
			}
			if next == cursor {
				return fmt.Errorf("automation enrollment scan repeated a cursor")
			}
			cursor = next
		}
		return nil
	}()
	if err != nil {
		return err
	}
	cursor := ""
	for page := 0; page < journeyDuePages; page++ {
		rows, next, err := w.store.rowsPage(systemCtx, cursor, call("query", "dueEnrollmentsForJourney", arg{"journeyId", id}, arg{"dueAt", w.nowUTC().Format(time.RFC3339Nano)}))
		if err != nil {
			return err
		}
		ids := []string{}
		selected := map[string]map[string]any{}
		for _, row := range rows {
			key := str(row, "id")
			ids = append(ids, key)
			selected[key] = row
		}
		err = campaignPage(ctx, ids, func(ctx context.Context, key string) error {
			return w.advanceEnrollment(ctx, systemCtx, journeyRow{ID: id}, selected[key])
		})
		if err != nil {
			return err
		}
		if next == "" || next == cursor {
			return nil
		}
		cursor = next
	}
	return nil
}

func (w *Worker) advanceEnrollment(ctx, systemCtx context.Context, journey journeyRow, scanned map[string]any) error {
	id := bare(str(scanned, "id"))
	owner := bare(str(scanned, "ownerUserId"))
	if owner == "" || w.newsletterGate == nil {
		return fmt.Errorf("automation enrollment coordination is unavailable")
	}
	// One gate connection owns the whole chain, including the single-send receipt.
	// The sending transport may reserve one more. Concurrent drains therefore cannot
	// exhaust the gate pool while waiting for their own nested receipt connection.
	receiptID := singleSendReceiptID(owner, journeyRequestID(id, str(scanned, "stepId")))
	hold, err := w.newsletterGate(context.WithoutCancel(ctx), "campaign-journey:journey:"+journey.ID, "campaign-journey:recipient:"+bare(str(scanned, "recipientId")), "campaign-journey:enrollment:"+id, singleSendLock(receiptID))
	if err != nil {
		return err
	}
	defer hold.Release()
	phase, end := context.WithTimeout(ctx, welcomePhaseCeiling)
	defer end()
	systemCtx = auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(w.systemActorContext(phase)))
	row, err := w.newsletterRow(systemCtx, "journeyEnrollmentById", "enrollmentId", id)
	if err != nil || row == nil {
		return err
	}
	if str(row, "status") != "active" && str(row, "status") != "waiting" || templateTime(row["nextAt"]).After(w.nowUTC()) {
		return nil
	}
	actual, err := w.newsletterRow(systemCtx, "journeyById", "journeyId", journey.ID)
	if err != nil || actual == nil {
		return err
	}
	j, err := journeyFromRow(actual)
	if err != nil {
		return err
	}
	if j.Status != "active" {
		return nil
	}
	if bare(str(row, "journeyId")) != j.ID || bare(str(row, "ownerUserId")) != j.OwnerUserID || !sameOrganization(str(row, "accountId"), j.AccountID) {
		return fmt.Errorf("automation enrollment scope changed")
	}
	actor := memql.ContextWithFreshRead(w.ownerActorContext(phase, j.OwnerUserID))
	if err = w.requireJourneyOrganization(actor, j.AccountID); err != nil {
		return err
	}
	index := integer(row, "stepIndex")
	if index < 0 {
		index = 0
	}
	if index > len(j.Steps) {
		index = len(j.Steps)
	}
	for i, step := range j.Steps {
		if step.ID == str(row, "stepId") {
			index = i
			break
		}
	}
	recipient, found, err := w.store.RecipientByID(actor, str(row, "recipientId"))
	if err != nil {
		return err
	}
	exitReason := ""
	switch {
	case !found:
		exitReason = "removed"
	case id != journeyEnrollmentID(j.ID, recipient.ID, recipient.JoinedAt):
		exitReason = "membership renewed"
	case !sameOrganization(j.AccountID, recipient.AccountID) || EmailDigest(recipient.Email) != str(row, "emailDigest"):
		exitReason = "address changed"
	case recipient.SubscriptionStatus == "unsubscribed" || recipient.SubscriptionStatus == "bounced" || recipient.SubscriptionStatus == "complained":
		exitReason = recipient.SubscriptionStatus
	}
	if exitReason == "" {
		_, suppressed, err := w.store.SuppressionForSend(systemCtx, j.AccountID, str(row, "emailDigest"))
		if err != nil {
			return err
		}
		if suppressed {
			exitReason = "suppressed"
		}
	}
	var step journeyStep
	if index < len(j.Steps) {
		step = j.Steps[index]
	}
	// If an edit removed the current step, the next surviving index has a different
	// receipt. Re-enter on the next tick using that step's receipt lock, never send
	// while holding the obsolete step's lock.
	if step.ID != "" && step.ID != str(scanned, "stepId") {
		if err = hold.Verify(phase); err != nil {
			return err
		}
		return detachedWrite(phase, func(write context.Context) error {
			return w.newsletterWrite(w.systemActorContext(write), "advanceJourneyEnrollment", map[string]any{"enrollmentId": id, "stepIndex": index, "stepId": step.ID, "nextAt": w.nowUTC().Format(time.RFC3339Nano), "status": "active", "attempts": 0, "versionTime": memql.VersionTimeAfter(templateTime(row["createdAt"]), w.nowUTC()).Format(time.RFC3339Nano)})
		})
	}
	write := func(patch map[string]any) error {
		if err := hold.Verify(phase); err != nil {
			return err
		}
		patch["enrollmentId"] = id
		patch["versionTime"] = memql.VersionTimeAfter(templateTime(row["createdAt"]), w.nowUTC()).Format(time.RFC3339Nano)
		if _, ok := patch["stepIndex"]; !ok {
			patch["stepIndex"] = index
		}
		return detachedWrite(phase, func(write context.Context) error {
			return w.newsletterWrite(w.systemActorContext(write), "advanceJourneyEnrollment", patch)
		})
	}
	lastError := ""
	advance := func(delay time.Duration) error {
		next := index + 1
		status := "active"
		nextID := ""
		if next < len(j.Steps) {
			nextID = j.Steps[next].ID
		} else if delay == 0 {
			status = "completed"
		}
		patch := map[string]any{"stepIndex": next, "stepId": nextID, "status": status, "nextAt": w.nowUTC().Add(delay).Format(time.RFC3339Nano), "attempts": 0, "lastError": lastError}
		if status == "completed" {
			patch["completedAt"] = w.nowUTC().Format(time.RFC3339Nano)
		}
		return write(patch)
	}
	fail := func(sendErr error) error {
		if errors.Is(sendErr, errCampaignRateLimited) || memql.IsWriteGateBusy(sendErr) || isUndecided(sendErr) || phase.Err() != nil {
			return sendErr
		}
		attempts := integer(row, "attempts") + 1
		status := "active"
		if attempts >= journeyMaxAttempts {
			status = "failed"
		}
		if err := write(map[string]any{"status": status, "attempts": attempts, "lastError": sendErr.Error(), "nextAt": w.nowUTC().Add(backoffFor(attempts)).Format(time.RFC3339Nano)}); err != nil {
			return err
		}
		if status == "failed" && j.LastError != sendErr.Error() {
			if err := hold.Verify(phase); err != nil {
				return err
			}
			return detachedWrite(phase, func(write context.Context) error {
				return w.newsletterWrite(w.systemActorContext(write), "recordJourneyDrain", map[string]any{"journeyId": j.ID, "lastError": sendErr.Error(), "versionTime": memql.VersionTimeAfter(j.Revision, w.nowUTC()).Format(time.RFC3339Nano)})
			})
		}
		return nil
	}
	_, err = workflowhost.Run(phase, "campaignJourneyStepWorkflow", map[string]any{"kind": step.Kind, "finished": index >= len(j.Steps), "exitReason": exitReason, "subscription": recipient.SubscriptionStatus, "waiting": str(row, "status") == "waiting"}, workflowhost.Options{Logger: w.logger, Operations: map[string]workflowhost.Operation{
		"campaignJourneyExit": func(_ context.Context, a map[string]any) (any, error) {
			return nil, write(map[string]any{"status": "exited", "exitReason": str(a, "reason"), "completedAt": w.nowUTC().Format(time.RFC3339Nano)})
		},
		"campaignJourneyComplete": func(_ context.Context, _ map[string]any) (any, error) {
			return nil, write(map[string]any{"status": "completed", "completedAt": w.nowUTC().Format(time.RFC3339Nano)})
		},
		"campaignJourneyAdvance": func(_ context.Context, _ map[string]any) (any, error) { return nil, advance(0) },
		"campaignJourneyWait": func(_ context.Context, _ map[string]any) (any, error) {
			return nil, advance(time.Duration(step.Days)*24*time.Hour + time.Duration(step.Hours)*time.Hour)
		},
		"campaignJourneyAwaitConfirmation": func(_ context.Context, _ map[string]any) (any, error) {
			return nil, write(map[string]any{"status": "waiting", "nextAt": w.nowUTC().Add(time.Duration(step.TimeoutDays) * 24 * time.Hour).Format(time.RFC3339Nano), "lastError": lastError, "attempts": 0})
		},
		"campaignJourneySend": func(_ context.Context, _ map[string]any) (any, error) {
			args := map[string]any{"requestId": journeyRequestID(id, step.ID), "templateId": step.TemplateID, "recipientId": recipient.ID, "senderIdentityId": step.SenderIdentityID}
			opts := singleSend{emailDigest: str(row, "emailDigest"), confirm: step.Kind == "confirm"}
			receipt, ok, err := singleSendReceiptFor(actor, args, opts)
			if err != nil || !ok {
				return nil, fmt.Errorf("automation send identity is invalid")
			}
			receipt.hold = hold
			nodes, sendErr := w.sendToRecipientHeld(actor, args, receipt, opts)
			if sendErr != nil {
				return map[string]any{"advance": false, "skipped": false}, fail(sendErr)
			}
			result := map[string]any{}
			if len(nodes) != 1 {
				return nil, fmt.Errorf("automation send returned no receipt")
			}
			if err = json.Unmarshal(nodes[0].Payload, &result); err != nil {
				return nil, err
			}
			if result["uncertain"] == true {
				lastError = str(result, "reason")
			}
			return map[string]any{"advance": result["sent"] == true || result["uncertain"] == true, "skipped": result["skipped"] == true}, nil
		},
	}})
	return err
}

func (w *Worker) retireJourney(ctx, systemCtx context.Context, scanned map[string]any) error {
	id := bare(str(scanned, "id"))
	if w.journeyGate == nil {
		return fmt.Errorf("automation coordination is unavailable")
	}
	hold, err := w.journeyGate(context.WithoutCancel(ctx), "journey:"+id)
	if err != nil {
		return err
	}
	defer hold.Release()
	row, err := w.newsletterRow(systemCtx, "journeyById", "journeyId", id)
	if err != nil || row == nil {
		return err
	}
	if str(row, "status") != "archived" || !templateTime(row["retiredAt"]).IsZero() {
		return nil
	}
	for page := 0; page < journeyDuePages; page++ {
		open, err := w.store.rows(memql.ContextWithFreshRead(systemCtx), call("query", "openEnrollmentsForJourney", arg{"journeyId", id}))
		if err != nil {
			return err
		}
		if len(open) == 0 {
			return w.journeyWrite(systemCtx, hold, "recordJourneyDrain", map[string]any{"journeyId": id, "retiredAt": w.nowUTC().Format(time.RFC3339Nano), "versionTime": memql.VersionTimeAfter(templateTime(row["createdAt"]), w.nowUTC()).Format(time.RFC3339Nano)})
		}
		for _, e := range open {
			// Advancing also owns this journey gate, so no send can race retirement.
			if err = w.journeyWrite(systemCtx, hold, "advanceJourneyEnrollment", map[string]any{"enrollmentId": bare(str(e, "id")), "stepIndex": integer(e, "stepIndex"), "status": "exited", "exitReason": "automation retired", "completedAt": w.nowUTC().Format(time.RFC3339Nano), "versionTime": memql.VersionTimeAfter(templateTime(e["createdAt"]), w.nowUTC()).Format(time.RFC3339Nano)}); err != nil {
				return err
			}
		}
	}
	return nil
}
