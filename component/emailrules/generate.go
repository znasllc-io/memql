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

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/functions"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
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
	_, err := canonicalCondition(r.Condition)
	return err
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

// ConditionError is a refused condition, worded for the person who typed it:
// the condition is the one place an end user writes an expression in the
// product (the OS Campaigns app's "Only when" field), and this message is what
// that field shows. Plain words, and the fix in one sentence.
type ConditionError struct{ Message string }

func (e *ConditionError) Error() string { return e.Message }

func conditionErr(format string, args ...any) error {
	return &ConditionError{Message: fmt.Sprintf(format, args...)}
}

// conditionExample is the example every refusal points at.
const conditionExample = `row.role == "admin"`

// canonicalCondition checks a rule's optional condition and returns it in its
// canonical form: an edition-2026 predicate over `row`, the triggering record,
// printed by the one canonical printer. It is the body of the generated
// trigger filter's lambda -- `@filter(row => <condition>)` -- so an empty
// condition is no filter at all.
//
// TWO SPELLINGS ARE ACCEPTED, and they generate the same automation. The v1
// one reads the record through `row` (`row.role == "admin"`). The legacy one,
// the only form the field accepted until the grammar changed, reads it through
// `payload` (`payload.role == "admin"`); stored rules keep that text -- it is
// the operator's data, and nothing migrates rows -- and it is converted here
// by the conversion `memqlmigrate --rewrite=expressions` applies to a trigger
// filter (payload.<f> becomes row.<f>, null becomes nil).
//
// The order of the checks is the point:
//
//  1. The INJECTION guards run on the raw text, before anything reads it. A
//     newline, a brace, an `@` or a `;` does not make a bad filter; in the
//     legacy text it closed the annotation and turned the rest of the condition
//     into source. The generated construct no longer carries the raw text --
//     it carries the parsed condition, re-printed -- but the guards stay, so a
//     condition that could only ever have been an attack is refused as one.
//  2. The condition is PARSED with the edition-2026 grammar, and a parse error
//     is refused with the parser's own words.
//  3. What it may READ is exactly what the generated filter's scope binds:
//     `row` (which it must read at least one field of), `now`, the event
//     envelope `event`, and the one argument the generated automation declares
//     (`id`, also `args.id`). `actor` is refused although the scope binds it:
//     a rule fires from the event bus with nobody signed in, so the actor is
//     always nobody and a condition over it would silently never match.
//  4. A construct call -- a query or mutation run from a condition -- is
//     refused, as the tier manifest refuses it in every trigger filter; so is
//     a call to a function the catalog does not hold, which the rule has no
//     spec or trait registry to resolve.
func canonicalCondition(condition string) (string, error) {
	c := strings.TrimSpace(condition)
	if c == "" {
		return "", nil
	}
	if strings.ContainsAny(c, "\r\n\x00{}@;") {
		return "", conditionErr("The condition has to fit on one line and can't contain braces, @ or semicolons. Write it like %s.", conditionExample)
	}
	src := c
	if legacyPayloadRoot.MatchString(maskConditionStrings(c)) {
		converted, err := convertLegacyCondition(c)
		if err != nil {
			return "", err
		}
		src = converted
	}
	node, err := langparser.ParseV1Expression(src)
	if err != nil {
		return "", conditionErr("The condition isn't valid: %v. Write it like %s.", err, conditionExample)
	}
	if err := checkConditionReads(node); err != nil {
		return "", err
	}
	return ast.FormatExpr(node), nil
}

// legacyPayloadRoot finds `payload.` used as a ROOT -- not `row.payload.x`,
// a v1 read of a payload field named payload -- on a view of the condition
// whose string literals are blanked.
var legacyPayloadRoot = regexp.MustCompile(`(^|[^A-Za-z0-9_.])payload\.`)

// maskConditionStrings blanks the contents of the condition's string
// literals, so text inside quotes is never read as structure.
func maskConditionStrings(s string) string {
	out := []byte(s)
	var quote byte
	for i := 0; i < len(out); i++ {
		switch {
		case quote != 0 && out[i] == '\\':
			out[i] = ' '
			if i+1 < len(out) {
				i++
				out[i] = ' '
			}
		case quote != 0 && out[i] == quote:
			quote = 0
		case quote != 0:
			out[i] = ' '
		case out[i] == '"' || out[i] == '\'':
			quote = out[i]
		}
	}
	return string(out)
}

// convertLegacyCondition carries a stored `payload.<field>` condition onto
// the edition-2026 form with the expressions rewrite itself, run over the
// trigger filter it used to be emitted as, and returns the lambda's body. One
// conversion, the codemod's, so a stored rule and a migrated hand-written
// automation cannot come out different.
func convertLegacyCondition(c string) (string, error) {
	const head = "@filter(row => "
	out, err := langparser.RewriteExpressions([]byte("@filter("+c+")\n"), nil)
	if err != nil {
		if m := bareWordErr.FindStringSubmatch(err.Error()); m != nil {
			return "", conditionErr("Put the text %s in quotes, like %q -- without quotes it reads as a name.", m[1], m[1])
		}
		return "", conditionErr("This condition uses the older payload. form and can't be converted automatically. Rewrite it with row., like %s.", conditionExample)
	}
	text := strings.TrimSuffix(string(out), "\n")
	if !strings.HasPrefix(text, head) || !strings.HasSuffix(text, ")") {
		return "", conditionErr("This condition uses the older payload. form and can't be converted automatically. Rewrite it with row., like %s.", conditionExample)
	}
	return strings.TrimSuffix(strings.TrimPrefix(text, head), ")"), nil
}

// bareWordErr recognises the rewrite's refusal of an unquoted word, the one
// legacy mistake worth a sentence of its own.
var bareWordErr = regexp.MustCompile(`the bare word ([^ ]+) is a path or a literal`)

// conditionRoots is what a condition may read by name: the scope the
// generated trigger filter evaluates in (see canonicalCondition). `row` is the
// lambda's parameter; `id` is the args field the generated automation
// declares, which the run's scope also answers bare.
var conditionRoots = map[string]bool{"row": true, "now": true, "event": true, "args": true, "id": true}

// checkConditionReads walks the parsed condition for what it reads and calls.
func checkConditionReads(node ast.ExpressionNode) error {
	readsRow := false
	var err error
	var walk func(n ast.ExpressionNode, bound map[string]bool)
	walk = func(n ast.ExpressionNode, bound map[string]bool) {
		if err != nil || n == nil {
			return
		}
		if tiers.KindAdmission(tiers.PositionTriggerFilter, ast.KindOf(n)) == tiers.Refused {
			if ast.KindOf(n) == ast.KindConstructCall {
				err = conditionErr("The condition can't run a query, mutation or other construct; it can only compare the record's fields, like %s.", conditionExample)
			} else {
				err = conditionErr("The condition can't use %s here. Compare the record's fields instead, like %s.", ast.FormatExpr(n), conditionExample)
			}
			return
		}
		switch e := n.(type) {
		case *ast.IdentExpr:
			if bound[e.Name] {
				return
			}
			switch {
			case e.Name == "actor":
				err = conditionErr("The condition can't use actor: nobody is signed in when a rule fires. To test who made the change, use row.createdBy.")
			case !conditionRoots[e.Name]:
				err = conditionErr("The condition can't use %s. To test a field of the record, write row.%s; if %s is a value, put it in quotes: %q.", e.Name, e.Name, e.Name, e.Name)
			}
		case *ast.MemberExpr:
			if root, ok := ast.Unparen(e.Object).(*ast.IdentExpr); ok && !bound[root.Name] {
				switch root.Name {
				case "row":
					readsRow = true
				case "args":
					if e.Field != "id" {
						err = conditionErr("The condition can read the record's fields (row.<field>) and its id, but not args.%s. Write row.%s instead.", e.Field, e.Field)
						return
					}
				}
			}
			walk(e.Object, bound)
		case *ast.CallExpr:
			if e.Receiver == nil && e.Kind == "" {
				if _, ok := functions.Lookup(e.Name); !ok {
					err = conditionErr("The condition uses %s(...), which isn't a function a condition can call. Use a built-in function instead, like lower(row.name) == \"ada\".", e.Name)
					return
				}
			}
			walk(e.Receiver, bound)
			for _, a := range e.Args {
				walk(a, bound)
			}
			for _, a := range e.Named {
				walk(a.Value, bound)
			}
		case *ast.LambdaExpr:
			inner := make(map[string]bool, len(bound)+len(e.Params))
			for k := range bound {
				inner[k] = true
			}
			for _, p := range e.Params {
				inner[p] = true
			}
			walk(e.Body, inner)
		case *ast.UnaryExpr:
			walk(e.Operand, bound)
		case *ast.BinaryExpr:
			walk(e.Left, bound)
			walk(e.Right, bound)
		case *ast.ListExpr:
			for _, el := range e.Elems {
				walk(el, bound)
			}
		case *ast.MapExpr:
			for _, en := range e.Entries {
				walk(en.Value, bound)
			}
		case *ast.ParenExpr:
			walk(e.Inner, bound)
		case *ast.TernaryExpr:
			walk(e.Condition, bound)
			walk(e.Then, bound)
			walk(e.Else, bound)
		}
	}
	walk(node, map[string]bool{})
	if err != nil {
		return err
	}
	if !readsRow {
		return conditionErr("The condition has to test a field of the record that changed, like %s.", conditionExample)
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
	// The filter is the rule's condition in its canonical v1 form -- the
	// PARSED condition, re-printed, never the text the operator typed -- as
	// the body of a lambda over the triggering row.
	cond, err := canonicalCondition(r.Condition)
	if err != nil {
		return "", err
	}
	if cond != "" {
		fmt.Fprintf(&b, "@filter(row => %s)\n", cond)
	}
	fmt.Fprintf(&b, "automation %s {\n", name)
	b.WriteString("  args {\n    id any\n  }\n")
	b.WriteString("  step send {\n")
	// The arguments are COMMA-separated. A construct call's arguments are a
	// list, and the parser refuses a second one that is not preceded by a
	// comma ("expected ')'"). The generator wrote them newline-separated, which
	// no binary linking the automation compiler accepted: this package's own
	// Gate-1 test passed only because its test binary did not link that
	// compiler, so the sandbox skipped the automation kind instead of
	// compiling it. generate_v1_test.go links it, which is what exposed it.
	b.WriteString("    builtin emailRuleFire (\n")
	fmt.Fprintf(&b, "      emailRuleId: %s,\n", langparser.QuoteString(r.ID))
	b.WriteString("      nodeId: id,\n")
	// `event: event` passes the triggering event's WHOLE ENVELOPE, and `id`
	// is the args field the trigger payload binds. In the edition-2026
	// grammar both are names: the run's scope (component/automations
	// RunScope) resolves `event` to the envelope and `id` to the bound
	// field, and a name it cannot resolve is refused -- at load by the
	// args-resolution gate, at fire time as an unknown name -- never passed
	// along as its own text. (Until the grammar flips, the legacy step
	// evaluator reads the same two words as runtime references.)
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
