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
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
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

// Firer executes one rule against one triggering event.
type Firer struct {
	store *Store
}

func NewFirer(engine Engine) *Firer { return &Firer{store: NewStore(engine)} }

// FireOutcome is what one firing did, for the result node and the log line.
type FireOutcome struct {
	RuleID     string
	Lane       string
	Recipients int
	Sent       int
	Skipped    int
	Refusals   []string
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

	switch rule.RecipientMode {
	case ModeClusterRoles:
		err = f.fireOperational(ctx, rule, nodeID, &out)
	case ModeAudience:
		err = f.fireAudience(ctx, rule, &out)
	case ModeRowAddress:
		err = f.fireRowAddress(ctx, rule, event, &out)
	default:
		err = fmt.Errorf("emailrules: rule %q has recipient mode %q, which this engine does not know", ruleID, rule.RecipientMode)
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
	if rerr := f.store.RecordFiring(auth.ContextWithInternalOrigin(ctx), ruleID, state.FiredCount+1, detail); rerr != nil && err == nil {
		err = rerr
	}
	return out, err
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
func (f *Firer) fireOperational(ctx context.Context, rule Rule, nodeID string, out *FireOutcome) error {
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
		// twice. It names the rule, the triggering row and the recipient --
		// the three things that together identify "this notice, about this
		// row, to this person".
		key := dedupeKey(rule.ID, nodeID, email)
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
func (f *Firer) fireAudience(ctx context.Context, rule Rule, out *FireOutcome) error {
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
		if err := f.store.SendToRecipient(ctx, rule.TemplateID, str(r, "id"), rule.SenderIdentity, rule.ID); err != nil {
			out.Refusals = append(out.Refusals, err.Error())
			continue
		}
		out.Sent++
	}
	return nil
}

// fireRowAddress mails one address read off the TRIGGERING EVENT.
//
// Off the event, not off the row: see this file's header. The address is
// enrolled into the rule's audience first, because an unsubscribe token is
// minted from (owner, recipient, campaign) -- an address with no recipient row
// has no way to opt out, and marketing mail nobody can unsubscribe from is the
// one thing this engine will not send.
func (f *Firer) fireRowAddress(ctx context.Context, rule Rule, event map[string]any, out *FireOutcome) error {
	address := strings.TrimSpace(payloadField(event, rule.RecipientField))
	if address == "" {
		out.Refusals = append(out.Refusals, fmt.Sprintf("the triggering row carries no %q, so there is nobody to mail", rule.RecipientField))
		return nil
	}
	if !strings.Contains(address, "@") {
		out.Refusals = append(out.Refusals, fmt.Sprintf("%q is not an address", rule.RecipientField))
		return nil
	}

	recipients, err := f.store.RecipientsForAudience(ctx, rule.AudienceID)
	if err != nil {
		return err
	}
	target := ""
	for _, r := range recipients {
		if strings.EqualFold(strings.TrimSpace(str(r, "email")), address) {
			target = str(r, "id")
			break
		}
	}
	if target == "" {
		// Not in the audience yet. Enrol, then send -- and note that the
		// enrolment is what an unsubscribe later acts on, so a second firing
		// for the same address consults the state the first one gave it.
		target = "rcp" + dedupeKey(rule.AudienceID, address, "")
		if err := f.store.AddRecipient(ctx, target, rule.AudienceID, address, payloadField(event, "displayName")); err != nil {
			return err
		}
	}
	out.Recipients++
	if err := f.store.SendToRecipient(ctx, rule.TemplateID, target, rule.SenderIdentity, rule.ID); err != nil {
		out.Refusals = append(out.Refusals, err.Error())
		return nil
	}
	out.Sent++
	return nil
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
