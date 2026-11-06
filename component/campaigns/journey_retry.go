package campaigns

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
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
	for _, scanned := range failed {
		ok, err := w.retryEnrollment(ctx, system, j, scanned)
		if err != nil {
			return nil, err
		}
		if ok {
			retried++
		}
	}
	return resultNode("campaignRetryEnrollments", map[string]any{"retried": retried})
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
	// attempting receipt, which replays "uncertain" forever. The operator has
	// checked and chosen to retry, so that attempt is recorded as not accepted and
	// the next one submits. A complete receipt is never touched: that email went.
	// The receipt belongs to the owner the drain sent as; only that actor reads and
	// versions it.
	ownerCtx := auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(w.ownerActorContext(phase, owner)))
	receipts, err := w.store.rows(ownerCtx, call("query", "campaignSingleSendById", arg{"receiptId", receiptID}))
	if err != nil {
		return false, err
	}
	if len(receipts) > 0 && str(receipts[0], "status") == receiptAttempting {
		receipt := &singleSendReceipt{id: receiptID, fingerprint: str(receipts[0], "fingerprint"), accountID: str(receipts[0], "accountId"), prior: templateTime(receipts[0]["createdAt"]), hold: hold}
		if err = w.saveReceiptHeld(ownerCtx, receipt, receiptFailed, nil); err != nil {
			return false, err
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
	return err == nil, err
}
