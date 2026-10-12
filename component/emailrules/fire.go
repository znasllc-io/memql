package emailrules

// fire.go -- what happens when a rule's generated automation runs.
//
// # The actor, and why everything here is careful about it
//
// This code runs under `AuthorContext` -- the rule author's user id, role
// writer. It is NOT the system actor (an authored automation deliberately does
// not get the engine-wide bypass) and it is NOT the triggering row's owner. So:
//
//   - the rule, its template and its audience are the AUTHOR'S OWN rows, and
//     read back fine;
//   - the TRIGGERING row may belong to somebody else entirely, and a read of it
//     under this envelope would return nothing while looking correct. That is
//     the trap this tree documents twice, and the answer is that we never read
//     it: the event envelope the trigger already delivered carries the payload,
//     and `event.payload.<field>` is where a row-address rule finds its
//     recipient.
//
// # The two lanes, and the one thing that must never blur
//
// OPERATIONAL mail -- "tell the owner a new admin was added" -- rides the
// transactional outbox. The egress allowlist applies, there is no unsubscribe
// footer, and the marketing suppression list is neither consulted nor written.
// MARKETING mail rides the campaign machinery: suppression checked at the point
// of send, unsubscribe attached, sender identity applied, outcome ledgered.
//
// The lane must never be a setting somebody can get wrong, because the failure
// is silent and asymmetric in both directions: an operational notice on the
// marketing lane can be silenced by an unsubscribe from a newsletter, and a
// marketing message on the operational lane goes out with no way to opt out at
// all. So the lane is DERIVED from who receives, in one function, and there is
// no code path that takes it as an argument.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// maxFanout bounds one firing.
//
// A rule is a TRIGGER, not a campaign: it fires per row, so a rule whose
// audience is a hundred thousand addresses would turn one row write into a
// hundred thousand sends with none of a campaign's pacing, preflight, ledger
// resumption or warm-up. Refusing is the honest answer -- "this is a campaign,
// send it as one" -- and refusing loudly at a bound beats discovering it as a
// reputation event.
const maxFanout = 200

// FirstFireClaim records that a created rule has fired for a row and reports
// whether THIS firing is the one that did: true for exactly one caller per
// (rule, row), on any replica. A false is a duplicate -- or a claim that could
// not be recorded, which is treated the same, because a created rule that
// cannot prove it is first must not send.
type FirstFireClaim func(ctx context.Context, ruleID, rowID string) bool

// FirstFireRelease gives back the claim a created rule took for a row, so the row's
// firing can be made again. It is for a firing that ended with nothing attempted --
// the cluster's lock was busy -- and only that: a firing that began a send keeps its
// claim, which is what keeps the row from sending twice.
type FirstFireRelease func(ctx context.Context, ruleID, rowID string)

// Firer executes one rule against one triggering event.
type Firer struct {
	store   *Store
	claim   FirstFireClaim
	release FirstFireRelease
	gate    FiringGate
}

func NewFirer(engine Engine) *Firer { return &Firer{store: NewStore(engine)} }

// WithFirstFireClaim returns the Firer with the claim a created rule needs.
func (f *Firer) WithFirstFireClaim(claim FirstFireClaim) *Firer {
	f.claim = claim
	return f
}

// WithFirstFireRelease returns the Firer with the release that goes with the claim.
// Without one, a created rule's firing that was held up by a busy lock keeps its
// claim, and the row can never fire again.
func (f *Firer) WithFirstFireRelease(release FirstFireRelease) *Firer {
	f.release = release
	return f
}

// WithFiringGate returns the Firer with the gate that records one rule's
// firings one at a time across every replica (firing_gate.go). Without one, a
// firing is recorded unlocked, and firings that overlap can lose increments.
func (f *Firer) WithFiringGate(gate FiringGate) *Firer {
	f.gate = gate
	return f
}

// FireOutcome is what one firing did, for the result node and the log line.
type FireOutcome struct {
	RuleID     string
	Lane       string
	Recipients int
	Sent       int
	Skipped    int
	Refusals   []string
	// Duplicate is a created rule's second firing for one row -- two writes
	// raced on a new row -- which sends nothing and is not counted as a firing.
	Duplicate bool
	// Busy: a send found the cluster's lock busy and, after a few short pauses,
	// still busy. Nothing was attempted for that recipient, the rule's fault it is
	// not, and a created rule's claim for the row was given back so the row can fire
	// again.
	Busy bool
}

// Fire resolves the rule, decides the lane from who receives, and sends.
//
// `nodeId` and `event` come from the generated automation's one step. `event`
// is the whole envelope; its `payload` is the triggering row as the trigger
// delivered it.
func (f *Firer) Fire(ctx context.Context, ruleID, nodeID string, event map[string]any) (FireOutcome, error) {
	out := FireOutcome{RuleID: ruleID}

	rule, found, err := f.store.RuleByID(ctx, ruleID)
	if err != nil {
		return out, err
	}
	if !found {
		// The construct outlived its rule. That is recoverable and not an
		// error worth tripping the circuit breaker over -- the rule was
		// deleted and the bundle retirement did not land, so say so and stop.
		return out, fmt.Errorf("emailrules: rule %q is gone; retire its construct", ruleID)
	}
	state, _, err := f.store.RuleStateByID(ctx, ruleID)
	if err != nil {
		return out, err
	}
	out.Lane = LaneFor(rule.RecipientMode)

	// A PAUSED RULE IS THE OPERATOR'S STOP BUTTON, and it has to work even
	// though the construct is still armed. The scheduler's own pause is one
	// mechanism; this is the other end of the same switch, checked here so a
	// rule paused between arming and firing does not get one more send in.
	if state.Status != "active" {
		out.Refusals = append(out.Refusals, "the rule is "+state.Status+", not active")
		return out, nil
	}

	// A ROW-ADDRESS RULE LOOKS AT THE MAILBOX BEFORE IT CLAIMS THE ROW (below). The
	// claim is the row's one chance, and a firing that ends in a refusal -- "could not
	// tell yet", or a read that failed and will be retried -- has not used it: claimed
	// first, it would leave the row with a claim taken and no mail, and nothing could
	// ever fire for it again.
	var plan rowAddressPlan
	if rule.RecipientMode == ModeRowAddress {
		if plan, err = f.planRowAddress(ctx, rule, event, &out); err != nil {
			return out, err
		}
	}

	// A CREATED RULE SENDS ONCE PER ROW. Its trigger already admits only the
	// write that materialised a row's first version (firstVersionGuard); this
	// closes the one gap the trigger cannot see. Two writes racing on a new id
	// both read "no prior version", so both events say firstVersion, and on
	// the marketing lanes nothing downstream collapses the two sends. The
	// claim is a primary-key insert, so exactly one firing -- on any replica,
	// with no local state -- wins. The loser is a duplicate of a creation that
	// already fired: it sends nothing and is not recorded as a firing.
	claimed := false
	if !plan.refused && strings.TrimSpace(rule.EventKind) == "created" {
		if f.claim == nil || !f.claim(ctx, rule.ID, nodeID) {
			out.Duplicate = true
			return out, nil
		}
		claimed = true
	}

	switch {
	case plan.refused:
		// Said while planning; the firing is recorded below, with the reason.
	case rule.RecipientMode == ModeClusterRoles:
		err = f.fireOperational(ctx, rule, nodeID, event, &out)
	case rule.RecipientMode == ModeAudience:
		err = f.fireAudience(ctx, rule, nodeID, event, &out)
	case rule.RecipientMode == ModeRowAddress:
		err = f.fireRowAddress(ctx, rule, nodeID, event, plan, &out)
	default:
		err = fmt.Errorf("emailrules: rule %q has recipient mode %q, which this engine does not know", ruleID, rule.RecipientMode)
	}

	// A send that found the cluster's lock busy attempted nothing. The row's claim
	// goes back with it: kept, the row's one chance would be spent on a firing that
	// sent no mail, and nothing could fire for it again.
	if out.Busy && claimed && f.release != nil {
		f.release(ctx, rule.ID, nodeID)
	}

	// The firing is recorded either way. A rule that fired and failed is a
	// different thing from a rule that never fired, and only one of them means
	// the trigger is wrong.
	detail := ""
	if err != nil {
		detail = truncate(err.Error(), 4096)
	} else if len(out.Refusals) > 0 {
		detail = truncate(strings.Join(out.Refusals, "; "), 4096)
	}
	if rerr := f.recordFiring(ctx, ruleID, detail); rerr != nil && err == nil {
		err = rerr
	}
	return out, err
}

// recordFiring advances the rule's count by exactly one and stamps its
// liveness (memql#5431).
//
// The state read at the top of Fire is NOT what gets advanced. It was read
// before the sends, possibly from this node's result cache, and any firing that
// overlapped this one -- here or on another replica -- may have advanced the
// count since. So the count is read again INSIDE the rule's firing gate, fresh
// from the database, and the new version is stamped after the version that
// read returned. firing_gate.go says why each of the three is needed.
func (f *Firer) recordFiring(ctx context.Context, ruleID, detail string) error {
	record := func(ctx context.Context) error {
		state, found, err := f.store.FiringStateByID(ctx, ruleID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("emailrules: rule %q is gone; its firing was not recorded", ruleID)
		}
		firedAt := firedAtAfter(state.VersionAt, time.Now())
		return f.store.RecordFiring(auth.ContextWithInternalOrigin(ctx), ruleID, state.FiredCount+1, firedAt, detail)
	}
	if f.gate == nil {
		return record(ctx)
	}
	return f.gate(ctx, ruleID, record)
}

// firedAtAfter is when a firing is recorded: now, or one microsecond after the
// version it was derived from, whichever is later.
//
// The version is the ORDER. Every read of a rule is served its newest version,
// and a version's createdAt is the writing process's wall clock -- so a replica
// whose clock lags another's would stamp its increment BEFORE the version it
// read, and every later read would skip it: the same lost increment the gate
// closes, reopened by clock skew rather than by overlap. Stamping strictly
// after the version read makes the order the gate imposes the order the table
// records.
//
// Microseconds, because that is what the column stores: a Go time that differs
// from the prior only below a microsecond would round onto the prior's key, and
// the store's insert drops a second version at an existing (id, createdAt)
// without a word (ON CONFLICT DO NOTHING).
func firedAtAfter(prior, now time.Time) time.Time {
	now = now.UTC().Truncate(time.Microsecond)
	if prior.IsZero() {
		return now
	}
	prior = prior.UTC().Truncate(time.Microsecond)
	if now.After(prior) {
		return now
	}
	return prior.Add(time.Microsecond)
}

// fireOperational mails this cluster's own people through the transactional
// outbox.
//
// The user read is stamped INTERNAL ORIGIN, and that deserves a sentence.
// `activeUsers` is @serverOnly precisely because it enumerates every user in
// the cluster and cannot be caller-scoped; the author's writer-role envelope
// cannot make that read. Stamping it here is legitimate -- this IS engine code
// doing server-initiated work -- and what keeps it from being an escalation is
// that the author never learns anything from it: the addresses are used to
// ADDRESS mail and are not returned, the outbox's own egress allowlist still
// applies, and arming a rule at all is gated on the authoring tier.
func (f *Firer) fireOperational(ctx context.Context, rule Rule, nodeID string, event map[string]any, out *FireOutcome) error {
	tmpl, ok, err := f.store.TemplateByID(ctx, rule.TemplateID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("emailrules: rule %q names template %q, which is not readable", rule.ID, rule.TemplateID)
	}
	users, err := f.store.UsersInRoles(auth.ContextWithInternalOrigin(ctx))
	if err != nil {
		return err
	}
	wanted := NormalizedRoles(rule.RecipientRoles)
	subject := str(tmpl, "subject")
	body := str(tmpl, "textBody")

	for _, u := range users {
		role := strings.ToLower(str(u, "role"))
		if !roleWanted(role, wanted) {
			continue
		}
		email := strings.TrimSpace(str(u, "primaryEmail"))
		if email == "" {
			email = strings.TrimSpace(str(u, "email"))
		}
		if email == "" {
			out.Skipped++
			continue
		}
		out.Recipients++
		if out.Recipients > maxFanout {
			out.Refusals = append(out.Refusals, fmt.Sprintf("stopped at %d recipients; a rule is a trigger, not a campaign", maxFanout))
			break
		}
		// The dedupe key makes a redelivered trigger collapse rather than mail
		// twice: the rule, the triggering row and the recipient identify "this
		// notice, about this row, to this person". A CHANGED rule's notice is
		// about one change, so its key also names the triggering write (the
		// event's own time, including fractional seconds) -- without it every
		// change after a row's first re-staged the
		// same outbound request, which keeps the status the worker gave it,
		// and the owner heard about one change per row, ever.
		key := dedupeKey(rule.ID, nodeID, email)
		if strings.TrimSpace(rule.EventKind) == "updated" {
			stamp, _ := event["timestamp"].(string)
			key = dedupeKey(rule.ID, nodeID, stamp, email)
		}
		if err := f.store.StageOutbound(ctx, "obr"+key, email, subject, body, key, rule.OwnerUserID); err != nil {
			out.Refusals = append(out.Refusals, err.Error())
			continue
		}
		out.Sent++
	}
	return nil
}

// roleWanted decides who receives. An EMPTY list means the cluster owner alone,
// which is the "tell me when this happens" case and the common one -- and it is
// a deliberate default rather than "everybody", because a rule that mails the
// whole cluster is a thing you should have to say.
func roleWanted(role string, wanted []string) bool {
	if len(wanted) == 0 {
		return role == "owner"
	}
	for _, w := range wanted {
		if w == role {
			return true
		}
	}
	return false
}

// fireAudience mails an existing audience through the campaign machinery.
func (f *Firer) fireAudience(ctx context.Context, rule Rule, nodeID string, event map[string]any, out *FireOutcome) error {
	recipients, err := f.store.RecipientsForAudience(ctx, rule.AudienceID)
	if err != nil {
		return err
	}
	for _, r := range recipients {
		out.Recipients++
		if out.Recipients > maxFanout {
			out.Refusals = append(out.Refusals, fmt.Sprintf("stopped at %d recipients; an audience this size is a campaign, and a campaign has the pacing, the ledger and the warm-up a rule does not", maxFanout))
			break
		}
		f.sendMarketing(ctx, rule, nodeID, str(r, "id"), event, out)
		if out.Busy {
			// The cluster is busy, so the rest of the audience would be too.
			break
		}
	}
	return nil
}

// rowAddressPlan is what a row-address firing settles before it claims anything:
// the address, and whom to mail for it.
type rowAddressPlan struct {
	// refused: the firing ends here, with the reason already in the outcome.
	refused bool
	address string
	// target is the recipient the send is addressed to; found is false when the
	// address is in no row of the audience and has to be enrolled first.
	target string
	found  bool
}

// planRowAddress reads the address off the TRIGGERING EVENT and finds its
// mailbox in the rule's audience. It writes nothing.
//
// Off the event, not off the row: see this file's header.
//
// Whether the address is a member already is found by its digest and, for the
// rows that carry none, by comparing addresses (Store.MailboxMembers), not by
// listing the audience (memql#6022, F9). The rows are found in any state, and ANY
// row that is not subscribed keeps the mailbox from being enrolled again or
// mailed through a subscribed sibling: the send goes to the row that opted out,
// where the send path refuses it and writes the ledger row the operator is owed.
// The reads are fresh ones, because they decide whether to write: a cached
// "nobody" from before another replica's enrolment would enrol the mailbox a
// second time.
func (f *Firer) planRowAddress(ctx context.Context, rule Rule, event map[string]any, out *FireOutcome) (rowAddressPlan, error) {
	plan := rowAddressPlan{refused: true}
	address := strings.TrimSpace(payloadField(event, rule.RecipientField))
	if address == "" {
		out.Refusals = append(out.Refusals, fmt.Sprintf("the triggering row carries no %q, so there is nobody to mail", rule.RecipientField))
		return plan, nil
	}
	// An address with no digest cannot be looked up, deduplicated or suppressed,
	// and the send would refuse it anyway: say so before anything is written.
	if addressDigest(address) == "" {
		out.Refusals = append(out.Refusals, fmt.Sprintf("%q is not an address", rule.RecipientField))
		return plan, nil
	}

	members, complete, err := f.store.MailboxMembers(memql.ContextWithFreshRead(ctx), rule.AudienceID, address)
	if err != nil {
		return plan, err
	}
	if !complete {
		out.Refusals = append(out.Refusals, fmt.Sprintf(
			"could not check every membership for %q in the audience. Nothing was sent; try again later",
			rule.RecipientField))
		return plan, nil
	}
	plan.refused, plan.address = false, address
	plan.target, plan.found = mailboxTarget(members)
	return plan, nil
}

// fireRowAddress mails one address read off the TRIGGERING EVENT, as planned.
//
// The address is enrolled into the rule's audience first, because an unsubscribe
// token is minted from (owner, recipient, campaign) -- an address with no
// recipient row has no way to opt out, and marketing mail nobody can unsubscribe
// from is the one thing this engine will not send. The enrolment names the rule's
// organization, because the send refuses a recipient outside the campaign's.
func (f *Firer) fireRowAddress(ctx context.Context, rule Rule, nodeID string, event map[string]any, plan rowAddressPlan, out *FireOutcome) error {
	target := plan.target
	if !plan.found {
		// Not in the audience yet. Enrol, then send -- and note that the
		// enrolment is what an unsubscribe later acts on, so a second firing
		// for the same address consults the state the first one gave it.
		target = "rcp" + dedupeKey(rule.AudienceID, plan.address, "")
		if err := f.store.AddRecipient(ctx, target, rule.AudienceID, plan.address, payloadField(event, "displayName"), memql.BareShortId(rule.AccountID)); err != nil {
			return err
		}
	}
	out.Recipients++
	f.sendMarketing(ctx, rule, nodeID, target, event, out)
	return nil
}

// mailboxTarget picks the row a send is addressed to: the first one that is not
// subscribed, so a mailbox that opted out is never mailed through a sibling that
// has not heard of it, and otherwise the first (oldest) row. found is false when
// the mailbox holds none.
func mailboxTarget(members []map[string]any) (id string, found bool) {
	for _, r := range members {
		if status := str(r, "subscriptionStatus"); status != "" && status != "subscribed" {
			return str(r, "id"), true
		}
	}
	if len(members) > 0 {
		return str(members[0], "id"), true
	}
	return "", false
}

// Stable across replicas and checkpoint replay. A created rule names one row's
// welcome; an updated rule names one occurrence, retaining fractional seconds.
// Never manufacture a clock value when the triggering event has none.
func marketingRequestID(rule Rule, nodeID, recipientID string, event map[string]any) (string, error) {
	if strings.TrimSpace(nodeID) == "" {
		return "", fmt.Errorf("emailrules: the triggering row identifier is missing")
	}
	occurrence := "created"
	if rule.EventKind != "created" {
		stamp, err := time.Parse(time.RFC3339Nano, str(event, "timestamp"))
		if err != nil || stamp.IsZero() {
			return "", fmt.Errorf("emailrules: the triggering event time is missing or invalid; refusing an unidentifiable send")
		}
		payload, err := json.Marshal(event["payload"])
		if err != nil {
			return "", fmt.Errorf("emailrules: the triggering event cannot be identified")
		}
		occurrence = dedupeKey(stamp.UTC().Format(time.RFC3339Nano), string(payload))
	}
	return "rule_" + dedupeKey(memql.BareShortId(rule.ID), memql.BareShortId(nodeID), memql.BareShortId(recipientID), occurrence), nil
}

func (f *Firer) sendMarketing(ctx context.Context, rule Rule, nodeID, recipientID string, event map[string]any, out *FireOutcome) {
	requestID, err := marketingRequestID(rule, nodeID, recipientID, event)
	if err != nil {
		out.Refusals = append(out.Refusals, err.Error())
		return
	}
	result, err := f.sendWithPatience(ctx, rule, recipientID, requestID)
	if memql.IsWriteGateBusy(err) {
		// Nothing was attempted for this recipient: the cluster's lock was busy
		// through every pause. Not a refusal of the message, and not the rule's fault.
		out.Busy = true
		out.Refusals = append(out.Refusals, "the cluster was too busy to start this send, so nothing was sent to this recipient; the firing can be made again")
		return
	}
	if err != nil {
		out.Refusals = append(out.Refusals, err.Error())
		return
	}
	switch {
	case result["uncertain"] == true:
		out.Refusals = append(out.Refusals, "a previous send has an uncertain provider outcome; it was not submitted again")
	case result["skipped"] == true || (result["sent"] == true && result["replayed"] == true):
		out.Skipped++
	case result["sent"] == true:
		out.Sent++
	default:
		out.Refusals = append(out.Refusals, "campaign send returned no recognized outcome")
	}
}

// busyPauses are the waits between one send and the next when the cluster's lock
// was busy (a variable only so a test can shorten them). The lock is usually busy
// for a moment, and a few short pauses inside the firing keep the row from needing a
// second delivery at all. Only a lock that was busy is tried again: a refusal on
// the merits is final for the firing, as it always was.
var busyPauses = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// sendWithPatience makes the send, and makes it again after each pause while the
// answer is that the cluster's lock was busy.
func (f *Firer) sendWithPatience(ctx context.Context, rule Rule, recipientID, requestID string) (map[string]any, error) {
	result, err := f.store.SendToRecipient(ctx, rule.TemplateID, recipientID, rule.SenderIdentity, rule.ID, requestID)
	for _, pause := range busyPauses {
		if !memql.IsWriteGateBusy(err) {
			break
		}
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		result, err = f.store.SendToRecipient(ctx, rule.TemplateID, recipientID, rule.SenderIdentity, rule.ID, requestID)
	}
	return result, err
}

// payloadField reads a dotted path out of the event envelope's payload.
func payloadField(event map[string]any, field string) string {
	if event == nil || strings.TrimSpace(field) == "" {
		return ""
	}
	node := any(event)
	if p, ok := event["payload"]; ok {
		node = p
	}
	for _, seg := range strings.Split(field, ".") {
		m, ok := node.(map[string]any)
		if !ok {
			return ""
		}
		node, ok = m[seg]
		if !ok {
			return ""
		}
	}
	if s, ok := node.(string); ok {
		return s
	}
	return ""
}

// dedupeKey is injective over its parts: each is hashed BEFORE it is joined, so
// no separator can appear inside a part and alias two distinct triples onto one
// key (authoring rule 20).
func dedupeKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		sum := sha256.Sum256([]byte(p))
		h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
