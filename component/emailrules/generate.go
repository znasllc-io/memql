package emailrules

// generate.go -- turning an event-email rule's FORM into a real authored
// automation construct (memql#4829, program decision P4).
//
// # Why a generated construct at all
//
// An automation's `@trigger` names ONE concept, at load time. A person picking
// an arbitrary trigger concept in a form therefore cannot be served by anything
// pre-shipped: there is no wildcard trigger, and there should not be one -- an
// automation that fires on every write in the cluster is a thing you build once
// and regret continuously. So the rule's executable form has to be authored, and
// the runtime authoring pipeline (bundle -> validate -> activate ->
// AuthoredRuntimeRegistry + AuthoredScheduler) is the tree's existing machinery
// for authoring something at runtime. Using it brings the whole governance
// apparatus along for free: per-rule pause, the per-automation circuit breaker,
// the cluster kill switch `authoredAutomationsEnabled`, boot re-arm, and an
// author-scoped actor rather than a system one.
//
// # Why the generated body is one line
//
// The obvious generator writes lane-specific DSL: a recipient loop for an
// audience, a role lookup for the operational lane, a condition step, a
// different send call per lane. That generator is a program that writes a
// program, and every bug in it is a bug in a string -- discovered per rule, at
// fire time, in text nobody reviewed.
//
// So the generated construct carries EXACTLY what only it can carry -- the
// `@trigger` that binds a concept at load time, and the `@filter` that drops
// non-matching events before any Go runs -- and its single step hands control
// to `emailRuleFire`, one builtin, in Go, read and tested once for every rule.
// The output is short enough that a person can read it and see what it does,
// which is the property the "no LLM" decision was actually protecting.
//
// # Why the event is forwarded rather than the row re-read
//
// An authored automation runs under the AUTHOR's envelope (AuthorContext, role
// writer), which is not the system actor and not the triggering row's owner. A
// rule triggered on `v1:identity:user` therefore cannot necessarily re-read the
// user row it fired on -- and a caller-scoped read from inside an automation
// returns nothing while looking entirely correct, which is the trap this tree
// documents twice (component/campaigns/schedule.go's header, and the fleet
// pack's billing.memql). Forwarding the event envelope sidesteps it: the
// payload the trigger already delivered is the payload the rule reads its
// recipient address out of, and no second read happens at all.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// Rule is the form half of v1:campaigns:emailRule -- everything the generator
// reads. The generated/refs half (bundleId, constructName, lastError, the
// counters) is deliberately absent: those are the ENGINE's account of what it
// made of the form, and a generator that read them could produce a construct
// whose content depended on its own past output.
type Rule struct {
	ID             string
	OwnerUserID    string
	Name           string
	Description    string
	TriggerConcept string
	EventKind      string
	Condition      string
	TemplateID     string
	RecipientMode  string
	RecipientRoles []string
	AudienceID     string
	RecipientField string
	AccountID      string
	SenderIdentity string
}

// The three recipient modes, and the two lanes they pick (program P5). The lane
// is a CONSEQUENCE of who receives, never a separate setting -- a person
// choosing "the people in this cluster" has chosen the operational lane whether
// or not they have ever heard the word.
const (
	ModeClusterRoles = "cluster_roles" // operational lane: the transactional outbox
	ModeAudience     = "audience"      // marketing lane: the campaign machinery
	ModeRowAddress   = "row_address"   // marketing lane, address off the triggering row
)

// LaneFor reports which delivery lane a recipient mode implies. Operational
// mail must never consult or write the marketing suppression list: an
// unsubscribe from a newsletter that silenced a security notice would be a
// correctly-implemented disaster.
func LaneFor(recipientMode string) string {
	if recipientMode == ModeClusterRoles {
		return "operational"
	}
	return "marketing"
}

var (
	// A canonical concept id, which is what @trigger's concept argument takes.
	conceptIdRe = regexp.MustCompile(`^v[0-9]+:[a-zA-Z][a-zA-Z0-9]*:[a-zA-Z][a-zA-Z0-9]*$`)
	// What a generated construct name may contain, after sanitising.
	nameSafeRe = regexp.MustCompile(`[^A-Za-z0-9]`)
	// A conservative grammar for the optional condition. See validateCondition.
	conditionSafeRe = regexp.MustCompile(`^[A-Za-z0-9_. "'\-=!<>&|()\[\],]+$`)
)

// ConstructNameFor derives the generated automation's name from the rule id.
//
// DETERMINISTIC, because that is what makes a regeneration REPLACE rather than
// accumulate: the activation planner supersedes a prior bundle by matching what
// it registers, and two names for one rule would leave the old automation armed
// beside the new one -- a rule that had been edited would then send twice, once
// with each version, which is the failure mode a person reports as "it sent the
// old copy as well".
//
// Construct names are unique tree-wide, so the prefix is not decoration.
func ConstructNameFor(ruleID string) string {
	short := ruleID
	if i := strings.LastIndex(short, ":"); i >= 0 {
		short = short[i+1:]
	}
	short = nameSafeRe.ReplaceAllString(short, "")
	if short == "" {
		short = "unnamed"
	}
	return "emailRule" + strings.ToUpper(short[:1]) + short[1:]
}

// BundleTitleFor is what an operator sees in the authoring surfaces. It names
// the rule rather than the construct, because the rule is the thing they made.
func BundleTitleFor(r Rule) string {
	name := strings.TrimSpace(r.Name)
	if name == "" {
		name = r.ID
	}
	return "Email rule: " + name
}

// Validate checks the form for everything that can be decided without the live
// concept registry. What it deliberately does NOT check is whether the trigger
// concept exists and whether the recipient field resolves on it -- those need
// the registry, they belong to the activation path, and a generator that
// reached for a registry would stop being a pure function of the form.
func (r Rule) Validate() error {
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("emailrules: the rule has no id")
	}
	if strings.TrimSpace(r.OwnerUserID) == "" {
		return fmt.Errorf("emailrules: rule %q has no owner; the generated automation runs under the author's envelope, so there is nobody to run it as", r.ID)
	}
	if !conceptIdRe.MatchString(strings.TrimSpace(r.TriggerConcept)) {
		return fmt.Errorf("emailrules: %q is not a concept id (expected the canonical v1:namespace:name form, e.g. v1:identity:user)", r.TriggerConcept)
	}
	if _, err := triggerEventFor(r.EventKind); err != nil {
		return err
	}
	if strings.TrimSpace(r.TemplateID) == "" {
		return fmt.Errorf("emailrules: rule %q names no template; there is nothing to send", r.ID)
	}
	switch r.RecipientMode {
	case ModeClusterRoles:
		// An empty role list is legal and means the cluster owner alone --
		// the "tell me when this happens" case, which is the common one.
	case ModeAudience:
		if strings.TrimSpace(r.AudienceID) == "" {
			return fmt.Errorf("emailrules: rule %q sends to an audience but names none", r.ID)
		}
	case ModeRowAddress:
		if strings.TrimSpace(r.RecipientField) == "" {
			return fmt.Errorf("emailrules: rule %q reads its recipient off the triggering row but names no field", r.ID)
		}
		// The audience is required here too, and it is not bookkeeping. An
		// unsubscribe token is minted from (owner, recipient, campaign), so an
		// address with no recipient row has no way to opt out -- and marketing
		// mail somebody cannot unsubscribe from breaches the RFC 8058 stance
		// the rest of this engine is built on. The address is enrolled into the
		// named audience, which gives it a membership, a subscription state,
		// and a second send that consults what the first one recorded.
		if strings.TrimSpace(r.AudienceID) == "" {
			return fmt.Errorf("emailrules: rule %q mails an address read off the triggering row but names no audience to enrol it in; without a recipient row the message would carry no working unsubscribe", r.ID)
		}
	default:
		return fmt.Errorf("emailrules: %q is not a recipient mode (expected %s, %s or %s)",
			r.RecipientMode, ModeClusterRoles, ModeAudience, ModeRowAddress)
	}
	return validateCondition(r.Condition)
}

// triggerEventFor maps the form's event kind onto the automation trigger's.
//
// Two values only, and the omission is deliberate: a DELETE has no row left to
// read a recipient or a merge value out of, so a rule triggered by one could
// describe nothing about what was deleted. Offering it would produce mail that
// says a thing happened and cannot say to what.
func triggerEventFor(eventKind string) (string, error) {
	switch strings.TrimSpace(eventKind) {
	case "created":
		return "node.created", nil
	case "updated":
		return "node.updated", nil
	default:
		return "", fmt.Errorf("emailrules: %q is not an event kind (expected \"created\" or \"updated\")", eventKind)
	}
}

// validateCondition pre-checks the optional filter expression.
//
// THIS IS NOT THE REAL CHECK, and saying so matters. The condition is emitted
// UNQUOTED inside `@filter(...)`, and the authoritative verdict on it is Gate 1
// of the authoring pipeline, which compiles the generated construct with the
// real parser -- so a condition this function lets through and the parser
// refuses fails the rule with the parser's own sentence, which is the better
// message anyway.
//
// What this function is for is the class the parser cannot help with: a
// condition carrying a `)` or a newline does not produce a bad filter, it
// produces a DIFFERENT CONSTRUCT -- the annotation closes early and whatever
// follows is read as source. That is the injection shape, and it has to be
// refused before the text is assembled rather than after.
func validateCondition(condition string) error {
	c := strings.TrimSpace(condition)
	if c == "" {
		return nil
	}
	if strings.ContainsAny(c, "\r\n\x00{}@;") {
		return fmt.Errorf("emailrules: the condition may not contain a newline, a brace, an @ or a semicolon -- it is emitted inside the generated automation's @filter, where any of those would close the annotation and turn the rest of the condition into source")
	}
	if !conditionSafeRe.MatchString(c) {
		return fmt.Errorf("emailrules: the condition contains a character the filter grammar does not use; write it as a comparison over payload fields, e.g. payload.role == \"admin\"")
	}
	if strings.Count(c, "(") != strings.Count(c, ")") {
		return fmt.Errorf("emailrules: the condition's parentheses are unbalanced")
	}
	if strings.Count(c, `"`)%2 != 0 {
		return fmt.Errorf("emailrules: the condition has an unterminated string")
	}
	if !strings.Contains(c, "payload.") {
		return fmt.Errorf("emailrules: the condition must test a field of the triggering row, which is spelled payload.<field> -- e.g. payload.role == \"admin\"")
	}
	return nil
}

// GenerateAutomation renders the rule's executable form.
//
// Pure: a function of the Rule and nothing else, so its whole output is
// diffable in a test and a change to it is visible as a change to the expected
// string rather than as a behaviour somebody has to reproduce.
func GenerateAutomation(r Rule) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	event, err := triggerEventFor(r.EventKind)
	if err != nil {
		return "", err
	}
	name := ConstructNameFor(r.ID)

	var b strings.Builder
	b.WriteString("// GENERATED from " + r.ID + " by component/emailrules. Do not edit:\n")
	b.WriteString("// regenerating the rule replaces this construct wholesale, and an edit here\n")
	b.WriteString("// would be silently discarded the next time somebody saves the rule.\n")
	b.WriteString("//\n")
	if n := strings.TrimSpace(r.Name); n != "" {
		b.WriteString("// " + commentSafe(n) + "\n")
	}
	if d := strings.TrimSpace(r.Description); d != "" {
		b.WriteString("// " + commentSafe(d) + "\n")
	}
	b.WriteString("// Lane: " + LaneFor(r.RecipientMode) + " (recipients: " + r.RecipientMode + ").\n")

	fmt.Fprintf(&b, "@trigger(event=%s, concept=%s, partition=\"*\")\n",
		langparser.QuoteString(event), langparser.QuoteString(strings.TrimSpace(r.TriggerConcept)))
	if c := strings.TrimSpace(r.Condition); c != "" {
		fmt.Fprintf(&b, "@filter(%s)\n", c)
	}
	fmt.Fprintf(&b, "automation %s {\n", name)
	b.WriteString("  args {\n    id any\n  }\n")
	b.WriteString("  step send {\n")
	b.WriteString("    builtin emailRuleFire (\n")
	fmt.Fprintf(&b, "      emailRuleId: %s\n", langparser.QuoteString(r.ID))
	b.WriteString("      nodeId: id\n")
	b.WriteString("      event: event\n")
	b.WriteString("    )\n")
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String(), nil
}

// commentSafe flattens operator-authored text onto one comment line. A newline
// in a name would end the comment and leave the rest as source; the rule's own
// name is not a place that has to carry one.
func commentSafe(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.Join(strings.Fields(s), " ")
}

// NormalizedRoles returns the operational lane's recipient roles, lowercased,
// de-duplicated and ordered, so two spellings of one rule produce one answer.
func NormalizedRoles(roles []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}
