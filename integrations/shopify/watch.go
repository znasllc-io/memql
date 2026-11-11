package shopify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/frontdoor"
	componentIdentity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/memql"
)

// watch.go -- every store's verdict, recorded, and its owner told.
//
// A verdict nobody looks at says nothing. The watch is a pass every five
// minutes (the watchShopifyStores automation, @mode(single), under the cron
// leader): it computes each store's verdict with the same gathering read the
// Store page uses (storeVerdict), records it on the store's health as
// `verdict`, and stages at most two emails per episode to the store's owner
// -- one when the store has needed attention, or been disconnected, for the
// alert wait, and one when it is recording again.
//
// AN EPISODE IS A `since`: the time the current state began, kept by
// computeVerdict while the state holds. The record carries which episode was
// mailed (mailedFor) and which was told it is over (recoveredFor), so each
// email is staged once whatever the number of passes, and the outbox ids are
// derived from the episode, so a pass that staged and then failed to record
// re-stages onto the same row.
//
// WHAT TO MAIL is planWatch's, a pure function of the verdict last recorded,
// the verdict computed now, the wait and the time. The pass executes the plan
// and records the outcome -- only when a recorded field changed, because each
// write is a new version of the store row.
//
// No personal data travels: an email carries the store's domain, when it
// stopped and the reasons' sentences; the owner's address is read when it is
// sent and is written nowhere else, and nothing about the owner is logged.

// storeAlertWaitVariable is the global variable that holds the alert wait,
// in minutes.
const storeAlertWaitVariable = "SHOPIFY_STORE_ALERT_MINUTES"

// defaultStoreAlertWait is the wait when the variable is absent, unreadable
// or out of range.
const defaultStoreAlertWait = 30 * time.Minute

// The alert's states, as health.verdict.alert.state records them.
const (
	AlertNone   = "none"
	AlertStaged = "staged"
	AlertSent   = "sent"
	AlertFailed = "failed"
)

// watchRequestedBy is the outbox's requestedBy on both emails.
const watchRequestedBy = "shopify:storeWatch"

// noOwnerToTell is the alert's reason when the store names nobody with an
// address.
const noOwnerToTell = "This store names no owner to tell."

// episodeIDLayout is the episode's stamp in an outbox id.
const episodeIDLayout = "20060102T150405Z"

// validAlertMinutes is the range the alert wait may be set to: one minute
// to one day.
func validAlertMinutes(n int) bool { return n >= 1 && n <= 1440 }

// storeAlertWait is the alert wait: the global variable when it holds a
// whole number of minutes in range, else the default.
func (c *Connector) storeAlertWait(ctx context.Context) time.Duration {
	wait, _ := c.alertWaitSetting(ctx)
	return wait
}

// alertWaitSetting is the alert wait and whether it is the default.
func (c *Connector) alertWaitSetting(ctx context.Context) (time.Duration, bool) {
	value, err := c.namedRowValue(ctx, conceptGlobalVariable, storeAlertWaitVariable, "value")
	if err != nil {
		return defaultStoreAlertWait, true
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || !validAlertMinutes(n) {
		return defaultStoreAlertWait, true
	}
	return time.Duration(n) * time.Minute, false
}

// recordedVerdict is health.verdict: what the next computation reads
// (state, since, resumedAt -- computeVerdict's Previous) and what the watch
// and the page need (reasons by code, the two episodes, the alert).
type recordedVerdict struct {
	State        string
	Reasons      []string
	Since        string
	ResumedAt    string
	MailedFor    string
	RecoveredFor string
	AlertState   string
	AlertReason  string
}

// verdictStamp is a time as the record writes it: RFC 3339 in UTC, to the
// second. An episode is named by its since in this form.
func verdictStamp(at time.Time) string { return at.UTC().Format(time.RFC3339) }

// episodeID is a recorded episode's stamp as an outbox id carries it.
func episodeID(stamp string) string {
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return ""
	}
	return at.UTC().Format(episodeIDLayout)
}

func alertRequestID(storeID, stamp string) string {
	return "shopify-store-alert-" + storeID + "-" + episodeID(stamp)
}

func recoveryRequestID(storeID, stamp string) string {
	return "shopify-store-recovered-" + storeID + "-" + episodeID(stamp)
}

// readRecorded reads health.verdict as the engine hands it back.
func readRecorded(previous map[string]any) recordedVerdict {
	r := recordedVerdict{
		State:        mapString(previous, "state"),
		Reasons:      mapStringSlice(previous, "reasons"),
		Since:        mapString(previous, "since"),
		ResumedAt:    mapString(previous, "resumedAt"),
		MailedFor:    mapString(previous, "mailedFor"),
		RecoveredFor: mapString(previous, "recoveredFor"),
		AlertState:   AlertNone,
	}
	if alert, ok := previous["alert"].(map[string]any); ok {
		if s := mapString(alert, "state"); s != "" {
			r.AlertState = s
		}
		r.AlertReason = mapString(alert, "reason")
	}
	return r
}

// recordOf is what a pass records for v: its state, reasons, since and
// resumedAt, with the episodes and the alert carried from the verdict last
// recorded (nil for none).
func recordOf(v Verdict, previous map[string]any) recordedVerdict {
	r := readRecorded(previous)
	r.State = v.State
	r.Reasons = []string{}
	for _, reason := range v.Reasons {
		r.Reasons = append(r.Reasons, reason.Code)
	}
	r.Since = verdictStamp(v.Since)
	r.ResumedAt = timeOrEmpty(v.ResumedAt)
	return r
}

// payload is the record as it is written to health.verdict.
func (r recordedVerdict) payload() map[string]any {
	return map[string]any{
		"state": r.State, "reasons": toAny(r.Reasons), "since": r.Since, "resumedAt": r.ResumedAt,
		"mailedFor": r.MailedFor, "recoveredFor": r.RecoveredFor,
		"alert": map[string]any{"state": r.AlertState, "reason": r.AlertReason},
	}
}

// alertStaged records that the alert of the episode was staged.
func (r *recordedVerdict) alertStaged(stamp string) {
	r.MailedFor, r.AlertState, r.AlertReason = stamp, AlertStaged, ""
}

// alertFailed records that the alert could not be sent, and why.
func (r *recordedVerdict) alertFailed(reason string) {
	r.AlertState, r.AlertReason = AlertFailed, reason
}

// recovered records that the episode was told it is over.
func (r *recordedVerdict) recovered(stamp string) { r.RecoveredFor = stamp }

// outboxOutcome reads the staged alert's delivery back: sent when the outbox
// delivered it, failed with the outbox's own reason when it gave up -- which
// is how an address off the allowlist is named. Anything else is still on its
// way and changes nothing.
func (r *recordedVerdict) outboxOutcome(status, lastError string) {
	switch status {
	case "sent":
		r.AlertState, r.AlertReason = AlertSent, ""
	case "failed":
		if strings.TrimSpace(lastError) == "" {
			lastError = "The outbox could not send the email."
		}
		r.alertFailed(strings.TrimSpace(lastError))
	}
}

// verdictRecordChanged reports whether rec differs from the verdict last
// recorded in any field it records. A verdict differs from the last one on
// every pass in what is recorded and when -- those are not part of the
// record -- so comparing the whole verdict would write the store row every
// five minutes.
func verdictRecordChanged(previous map[string]any, rec recordedVerdict) bool {
	if previous == nil {
		return true
	}
	was := readRecorded(previous)
	return was.State != rec.State || !slices.Equal(was.Reasons, rec.Reasons) || was.Since != rec.Since ||
		was.ResumedAt != rec.ResumedAt || was.MailedFor != rec.MailedFor || was.RecoveredFor != rec.RecoveredFor ||
		was.AlertState != rec.AlertState || was.AlertReason != rec.AlertReason
}

// watchPlan is what one pass does for one store.
type watchPlan struct {
	// Record is what is recorded before the outcomes below are applied.
	Record recordedVerdict
	// ReadBack: the mailed episode's alert is staged; read its outcome.
	ReadBack bool
	// Alert: stage the alert of the current episode.
	Alert bool
	// Recover: stage the recovery of this episode (the mailed one), or "".
	Recover string
}

// planWatch decides one store's pass.
//
// ALERT when the state is needs attention or not connected, has been for the
// wait (now - since >= wait), and this episode was not mailed. Paused and
// starting never alert: a person paused it, or it is on its way.
//
// RECOVER when the state is recording and an alert was mailed (mailedFor is
// set, which a pass writes only once the alert was staged) for an episode not
// yet told it is over. A store that passes through starting on its way back
// keeps mailedFor and is told when it records. An alert the outbox refused
// after it was staged still owes its recovery.
func planWatch(previous map[string]any, v Verdict, wait time.Duration, now time.Time) watchPlan {
	rec := recordOf(v, previous)
	plan := watchPlan{Record: rec, ReadBack: rec.AlertState == AlertStaged && rec.MailedFor != ""}
	switch v.State {
	case VerdictNeedsAttention, VerdictNotConnected:
		plan.Alert = now.Sub(v.Since) >= wait && rec.MailedFor != rec.Since
	case VerdictRecording:
		if rec.MailedFor != "" && rec.RecoveredFor != rec.MailedFor {
			plan.Recover = rec.MailedFor
		}
	}
	return plan
}

// alertEmail is the alert's subject and body. Each reason travels as its
// sentence alone: a detail is the engine's or Shopify's own text, which a
// mailbox is not the place for.
func alertEmail(v Verdict, osBase string) (string, string) {
	var b strings.Builder
	b.WriteString("MemQL has not been recording " + v.Domain + " since " + v.Since.UTC().Format(time.RFC1123) + ".\n\n")
	if len(v.Reasons) > 0 {
		for _, reason := range v.Reasons {
			b.WriteString(reason.Sentence + "\n")
		}
		b.WriteString("\n")
	}
	if osBase != "" {
		b.WriteString("Open the storefront's Store page in MemQL OS to see what fixes it: " + osBase + "/\n\n")
	}
	b.WriteString("This email carries no order and nothing about a customer.")
	return v.Domain + " is not being recorded", b.String()
}

// recoveryEmail is the recovery's subject and body, from the mailed
// episode's recorded since to now.
func recoveryEmail(domain, stamp string, now time.Time) (string, string) {
	from := stamp
	if at, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
		from = at.UTC().Format(time.RFC1123)
	}
	return domain + " is being recorded again",
		"MemQL is recording " + domain + " again. It was not recording from " + from + " to " + now.UTC().Format(time.RFC1123) + "."
}

// osBase is MemQL OS's address, from the cluster's domain, or "" when the
// cluster names none.
func osBase() string {
	domain := strings.TrimSpace(os.Getenv("MEMQL_DOMAIN"))
	if domain == "" {
		return ""
	}
	return "https://" + frontdoor.SiteHost(frontdoor.OsSite, domain)
}

// errNoOwner: the store names nobody, or nobody with an address.
var errNoOwner = errors.New(noOwnerToTell)

// ownerAddress is the address of the store's owner, read at send time. A
// store with no owner, an owner not found or deactivated, or one with no
// address is errNoOwner; a failed read is its own error, and nothing is
// decided on it.
func (c *Connector) ownerAddress(ctx context.Context, store Store) (string, error) {
	owner := strings.TrimSpace(store.OwnerUserID)
	if owner == "" {
		return "", errNoOwner
	}
	users := &componentIdentity.Store{Engine: c.engine, Logger: c.logger}
	user, err := users.LookupUserById(operatorContext(ctx), owner)
	if err != nil {
		return "", fmt.Errorf("shopify: read the store's owner: %w", err)
	}
	if user == nil || !user.Active || strings.TrimSpace(user.PrimaryEmail) == "" {
		return "", errNoOwner
	}
	return strings.TrimSpace(user.PrimaryEmail), nil
}

// stageEmail stages one email through the server staging, which marks the
// row serverStaged so that only server code writes it after: its outcome is
// read back here, and nothing a client writes may fake it.
func (c *Connector) stageEmail(ctx context.Context, requestID, target, subject, body string) error {
	_, err := c.engine.Execute(operatorContext(ctx), renderCall("stageServerOutboundRequest", map[string]any{
		"requestId": requestID, "medium": "email", "target": target, "subject": subject, "body": body,
		"dedupeKey": requestID, "requestedBy": watchRequestedBy,
	}))
	if err != nil {
		// The pass logs this error, and an engine error may quote the call
		// it refused: the owner's address never reaches the log.
		return fmt.Errorf("shopify: stage the email %s: %s", requestID, strings.ReplaceAll(err.Error(), target, "<the owner's address>"))
	}
	return nil
}

// outboundOutcome reads one staged email's delivery state, fresh: the
// outbound worker stamps it on whichever node claimed it.
func (c *Connector) outboundOutcome(ctx context.Context, requestID string) (string, string, error) {
	res, err := c.engine.Execute(memql.ContextWithFreshRead(operatorContext(ctx)), renderCall("outboundRequestById", map[string]any{"requestId": requestID}))
	if err != nil {
		return "", "", fmt.Errorf("shopify: read the alert's delivery: %w", err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return "", "", nil
	}
	return mapString(rows[0], "status"), mapString(rows[0], "lastError"), nil
}

// WatchStores is one pass of the watch over every store. One store whose
// verdict cannot be computed or recorded is logged by its id and the pass
// goes on; the pass fails only when it cannot list the stores, and stops,
// with the context's error, when its context is done.
func (c *Connector) WatchStores(ctx context.Context) error {
	stores, err := c.stores.Stores(memql.ContextWithFreshRead(ctx))
	if err != nil {
		return fmt.Errorf("shopify: the store watch could not list the stores: %w", err)
	}
	wait := c.storeAlertWait(ctx)
	for _, store := range stores {
		if err := ctx.Err(); err != nil {
			return err
		}
		v, err := c.storeVerdict(ctx, store)
		if err == nil {
			err = c.watchStoreVerdict(ctx, store, v, wait)
		}
		if err != nil {
			c.logger.Warn("shopify: the store watch could not watch a store", "store", store.ID, "error", err.Error())
		}
	}
	return nil
}

// watchStoreVerdict executes one store's plan and records the outcome when a
// recorded field changed. A failed staging leaves its episode unmailed, so
// the next pass tries again; what else changed is still recorded.
func (c *Connector) watchStoreVerdict(ctx context.Context, store Store, v Verdict, wait time.Duration) error {
	now := c.now().UTC()
	previous, _ := store.Health["verdict"].(map[string]any)
	plan := planWatch(previous, v, wait, now)
	rec := plan.Record
	var errs []error

	if plan.ReadBack {
		status, lastError, err := c.outboundOutcome(ctx, alertRequestID(store.ID, rec.MailedFor))
		if err != nil {
			errs = append(errs, err)
		} else {
			rec.outboxOutcome(status, lastError)
		}
	}

	if plan.Alert || plan.Recover != "" {
		target, err := c.ownerAddress(ctx, store)
		switch {
		case errors.Is(err, errNoOwner):
			if plan.Alert {
				rec.alertFailed(noOwnerToTell)
			}
			if plan.Recover != "" {
				// Nobody to tell; the episode is closed all the same, so an
				// owner named later is not told of a recovery long past.
				rec.recovered(plan.Recover)
			}
		case err != nil:
			errs = append(errs, err)
		case plan.Alert:
			subject, body := alertEmail(v, osBase())
			if err := c.stageEmail(ctx, alertRequestID(store.ID, rec.Since), target, subject, body); err != nil {
				errs = append(errs, err)
			} else {
				rec.alertStaged(rec.Since)
			}
		default:
			subject, body := recoveryEmail(v.Domain, plan.Recover, now)
			if err := c.stageEmail(ctx, recoveryRequestID(store.ID, plan.Recover), target, subject, body); err != nil {
				errs = append(errs, err)
			} else {
				rec.recovered(plan.Recover)
			}
		}
	}

	if verdictRecordChanged(previous, rec) {
		if err := c.recordStoreHealthKey(ctx, store.ID, "verdict", rec.payload(), nil); err != nil {
			errs = append(errs, fmt.Errorf("shopify: record the verdict: %w", err))
		}
	}
	return errors.Join(errs...)
}

// recordedAlert is the alert last recorded on a store, for the verdict's
// reply.
func recordedAlert(store Store) map[string]any {
	previous, _ := store.Health["verdict"].(map[string]any)
	rec := readRecorded(previous)
	return map[string]any{"state": rec.AlertState, "reason": rec.AlertReason}
}
