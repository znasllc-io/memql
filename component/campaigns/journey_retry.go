package campaigns

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// journey_retry.go -- a run that failed can be started again by the person who
// runs the automation. Retry only puts the enrollment back where the drain
// finds it: every exit rule still applies there, so an unsubscribed or
// suppressed person leaves instead of being mailed.

// handleRetryEnrollments starts failed runs of one automation again: back to
// active, due now, with no attempt spent and no error. An empty enrollmentIds
// means every failed run. Anything that is not failed is left alone.
func (w *Worker) handleRetryEnrollments(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	id := bare(argString(args, "journeyId"))
	if !templateIDPattern.MatchString(id) || w.journeyGate == nil || w.newsletterGate == nil {
		return nil, fmt.Errorf("select an automation while connected to the cluster")
	}
	chosen, err := retryEnrollmentIDs(args["enrollmentIds"])
	if err != nil {
		return nil, err
	}
	row, err := w.newsletterRow(ctx, "journeyById", "journeyId", id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fmt.Errorf("automation is not readable")
	}
	j, err := journeyFromRow(row)
	if err != nil {
		return nil, err
	}
	if err = w.requireJourneyOrganization(ctx, j.AccountID); err != nil {
		return nil, err
	}
	if j.Status == "archived" {
		return nil, visitorMessage("An archived automation can't be changed.")
	}
	system := auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(w.systemActorContext(ctx)))
	failed := []map[string]any{}
	cursor := ""
	for page := 0; ; page++ {
		rows, next, err := w.store.rowsPage(system, cursor, call("query", "failedEnrollmentsForJourney", arg{"journeyId", id}))
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if len(chosen) == 0 || chosen[bare(str(r, "id"))] {
				failed = append(failed, r)
			}
		}
		if next == "" {
			break
		}
		if next == cursor {
			return nil, fmt.Errorf("failed enrollment scan repeated a cursor")
		}
		cursor = next
	}
	retried := 0
	reasons := map[string]bool{}
	for _, scanned := range failed {
		ok, err := w.retryEnrollment(ctx, system, j, scanned)
		if err != nil {
			return nil, err
		}
		if ok {
			retried++
			reasons[str(scanned, "lastError")] = true
		}
	}
	if retried > 0 {
		if err = w.clearRetriedJourneyError(ctx, system, j.ID, reasons); err != nil {
			return nil, err
		}
	}
	return resultNode("campaignRetryEnrollments", map[string]any{"retried": retried})
}

// clearRetriedJourneyError clears the automation's error once a retry has
// restarted the runs it describes and none is failed any more. The drain clears
// it when an email goes out, but a restarted run can also leave without one (the
// person unsubscribed), and then nothing would. A failed run's error and the
// automation's are the same text; an error that is not a restarted run's own
// (why sends wait, say) is the drain's to keep, and a run that fails again notes
// its reason again. It reads the automation and its failed runs under the
// journey gate the drain notes the error under.
func (w *Worker) clearRetriedJourneyError(ctx, system context.Context, id string, reasons map[string]bool) error {
	hold, err := w.journeyGate(context.WithoutCancel(ctx), journeyLock(id))
	if err != nil {
		return err
	}
	defer hold.Release()
	row, err := w.newsletterRow(system, "journeyById", "journeyId", id)
	if err != nil || row == nil {
		return err
	}
	if said := str(row, "lastError"); said == "" || !reasons[said] {
		return nil
	}
	left, _, err := w.store.rowsPage(system, "", call("query", "failedEnrollmentsForJourney", arg{"journeyId", id}))
	if err != nil || len(left) > 0 {
		return err
	}
	return w.journeyWrite(system, hold, "recordJourneyDrain", map[string]any{"journeyId": id, "lastError": "", "versionTime": memql.VersionTimeAfter(templateTime(row["createdAt"]), w.nowUTC()).Format(time.RFC3339Nano)})
}

// retryEnrollmentIDs is the enrollments an operator named, bare. Nothing named
// means all of them.
func retryEnrollmentIDs(raw any) (map[string]bool, error) {
	ids := map[string]bool{}
	switch list := raw.(type) {
	case nil:
	case []string:
		for _, v := range list {
			ids[bare(v)] = true
		}
	case []any:
		for _, v := range list {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("enrollmentIds must be a list of text")
			}
			ids[bare(s)] = true
		}
	default:
		return nil, fmt.Errorf("enrollmentIds must be a list of text")
	}
	delete(ids, "")
	return ids, nil
}

// retryEnrollment resets one failed enrollment under the locks the drain takes
// for it, reading it again there: it may have been retried or advanced since the
// scan.
func (w *Worker) retryEnrollment(ctx, system context.Context, j journeyRow, scanned map[string]any) (bool, error) {
	id := bare(str(scanned, "id"))
	owner := bare(str(scanned, "ownerUserId"))
	receiptID := singleSendReceiptID(owner, journeyRequestID(id, str(scanned, "stepId")))
	hold, err := w.newsletterGate(context.WithoutCancel(ctx), journeyLock(j.ID), "campaign-journey:recipient:"+bare(str(scanned, "recipientId")), "campaign-journey:enrollment:"+id, singleSendLock(receiptID))
	if err != nil {
		return false, err
	}
	defer hold.Release()
	phase, end := context.WithTimeout(ctx, welcomePhaseCeiling)
	defer end()
	row, err := w.newsletterRow(system, "journeyEnrollmentById", "enrollmentId", id)
	if err != nil || row == nil {
		return false, err
	}
	if str(row, "status") != "failed" || bare(str(row, "journeyId")) != j.ID || !sameOrganization(str(row, "accountId"), j.AccountID) ||
		bare(str(row, "ownerUserId")) != owner || str(row, "stepId") != str(scanned, "stepId") {
		return false, nil
	}
	// An enrollment that failed because its send could not be confirmed keeps an
	// attempting receipt, which replays "uncertain" forever. Ask the provider what
	// became of that attempt first: if it accepted the email, the receipt is
	// completed and the step replays as sent, with no second submission. Otherwise
	// -- not accepted, still unknown, or no record kept -- the operator asked for a
	// retry, so the attempt is recorded as not accepted and the next one submits.
	// A complete receipt is never touched: that email went.
	// The receipt belongs to the owner the drain sent as; only that actor reads and
	// versions it.
	ownerCtx := auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(w.ownerActorContext(phase, owner)))
	receipts, err := w.store.rows(ownerCtx, call("query", "campaignSingleSendById", arg{"receiptId", receiptID}))
	if err != nil {
		return false, err
	}
	if len(receipts) > 0 && str(receipts[0], "status") == receiptAttempting {
		receipt := &singleSendReceipt{id: receiptID, fingerprint: str(receipts[0], "fingerprint"), accountID: str(receipts[0], "accountId"), prior: templateTime(receipts[0]["createdAt"]), hold: hold}
		status, result := receiptFailed, map[string]any(nil)
		accepted := w.priorOutcome(ownerCtx, receipt) == email.OutcomeAccepted
		if accepted {
			status = receiptComplete
			result = map[string]any{"sent": true, "skipped": false, "recipientId": bare(str(row, "recipientId")), "emailRuleId": ""}
		}
		if err = w.saveReceiptHeld(ownerCtx, receipt, status, result); err != nil {
			return false, err
		}
		if accepted {
			// The ledger says what the provider says, as a reconciled send's does.
			w.settleAcceptedDelivery(ownerCtx, receipt, journeyRequestID(id, str(row, "stepId")), bare(str(row, "recipientId")))
		}
	}
	if err = hold.Verify(phase); err != nil {
		return false, err
	}
	now := w.nowUTC()
	err = detachedWrite(phase, func(write context.Context) error {
		return w.newsletterWrite(w.systemActorContext(write), "advanceJourneyEnrollment", map[string]any{
			"enrollmentId": id, "stepId": str(row, "stepId"), "stepIndex": integer(row, "stepIndex"),
			"nextAt": now.Format(time.RFC3339Nano), "status": "active", "exitReason": "", "completedAt": "", "lastError": "", "attempts": 0,
			"versionTime": memql.VersionTimeAfter(templateTime(row["createdAt"]), now).Format(time.RFC3339Nano),
		})
	})
	if err != nil {
		return false, err
	}
	return true, nil
}
