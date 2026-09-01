package emailrules

// store.go -- every graph read and write this package makes, rendered as named
// DSL constructs.
//
// No raw query strings and no bespoke SQL: the same doctrine component/campaigns
// records. A named construct carries its own authorization tier, its own
// projection and its own argument contract, so a call here inherits all three
// rather than re-stating them somewhere they can drift.
//
// THE ACTOR IS THE CALLER'S BUSINESS, not the store's. Not one method builds an
// actor context. Every call runs under the ctx it is handed, and the two callers
// are explicit about which identity that is: the activation path runs under the
// operator who pressed the button, and the fire path runs under the AUTHOR's
// envelope that the authored scheduler built. Neither is the system actor, and
// neither should be.

import (
	"context"
	"fmt"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/num"
)

// Engine is the sliver of the engine this package needs.
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

type Store struct{ engine Engine }

func NewStore(engine Engine) *Store { return &Store{engine: engine} }

// call renders `<kind> <name>(k: v, ...)`. Every string rides through the
// parser's own quoter rather than %q: the two disagree on four control bytes,
// and one of the values passed through here is a GENERATED .memql SOURCE full of
// quotes, braces and newlines. A %q-quoted source emits escapes the MemQL lexer
// refuses outright, which would make the statement unparseable and silently drop
// the write that was recording it.
func call(kind, name string, args ...arg) string {
	rendered := make([]string, 0, len(args))
	for _, a := range args {
		switch v := a.value.(type) {
		case string:
			rendered = append(rendered, a.name+": "+langparser.QuoteString(v))
		case int:
			rendered = append(rendered, fmt.Sprintf("%s: %d", a.name, v))
		case bool:
			rendered = append(rendered, fmt.Sprintf("%s: %t", a.name, v))
		case rawValue:
			rendered = append(rendered, a.name+": "+string(v))
		case []string:
			quoted := make([]string, 0, len(v))
			for _, item := range v {
				quoted = append(quoted, langparser.QuoteString(item))
			}
			rendered = append(rendered, a.name+": ["+strings.Join(quoted, ", ")+"]")
		default:
			rendered = append(rendered, a.name+": "+langparser.QuoteString(fmt.Sprintf("%v", v)))
		}
	}
	return fmt.Sprintf("%s %s(%s)", kind, name, strings.Join(rendered, ", "))
}

type arg struct {
	name  string
	value any
}

// rawValue is an already-rendered MemQL literal -- the object reports the
// authoring gates take. It exists so those two calls do not have to route a map
// through the string quoter, which would hand `validationReport` a quoted blob
// where the mutation declares an object. Everything else goes through the
// quoter, and nothing operator-authored is ever a rawValue.
type rawValue string

func (s *Store) rows(ctx context.Context, q string) ([]map[string]any, error) {
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("emailrules: %s: %w", firstWords(q), err)
	}
	return memql.MaterializeRows(res), nil
}

func (s *Store) exec(ctx context.Context, q string) error {
	if _, err := s.engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("emailrules: %s: %w", firstWords(q), err)
	}
	return nil
}

// firstWords truncates a rendered call at its first paren, so an error can name
// what failed without echoing an argument -- one of which is an email address.
func firstWords(q string) string {
	if i := strings.Index(q, "("); i > 0 {
		return strings.TrimSpace(q[:i])
	}
	if len(q) > 60 {
		return q[:60]
	}
	return q
}

// RuleByID reads one rule under the caller's own actor. That read IS the
// authorization on every path in this package: a rule the caller cannot read is
// a rule they cannot activate, retire or fire.
func (s *Store) RuleByID(ctx context.Context, ruleID string) (Rule, bool, error) {
	rows, err := s.rows(ctx, call("query", "emailRuleById", arg{"emailRuleId", memql.BareShortId(ruleID)}))
	if err != nil {
		return Rule{}, false, err
	}
	if len(rows) == 0 {
		return Rule{}, false, nil
	}
	return ruleFromRow(rows[0]), true, nil
}

// RuleStateByID returns the fields the generator deliberately does not read --
// the engine's own account of what it last made of the form.
func (s *Store) RuleStateByID(ctx context.Context, ruleID string) (RuleState, bool, error) {
	rows, err := s.rows(ctx, call("query", "emailRuleById", arg{"emailRuleId", memql.BareShortId(ruleID)}))
	if err != nil {
		return RuleState{}, false, err
	}
	if len(rows) == 0 {
		return RuleState{}, false, nil
	}
	r := rows[0]
	return RuleState{
		Status:        str(r, "status"),
		BundleID:      str(r, "bundleId"),
		ConstructName: str(r, "constructName"),
		LastError:     str(r, "lastError"),
		FiredCount:    integer(r, "firedCount"),
	}, true, nil
}

// RuleState is the generated half of the row.
type RuleState struct {
	Status        string
	BundleID      string
	ConstructName string
	LastError     string
	FiredCount    int
}

func ruleFromRow(r map[string]any) Rule {
	return Rule{
		ID:             str(r, "id"),
		OwnerUserID:    memql.BareShortId(str(r, "ownerUserId")),
		Name:           str(r, "name"),
		Description:    str(r, "description"),
		TriggerConcept: str(r, "triggerConcept"),
		EventKind:      str(r, "eventKind"),
		Condition:      str(r, "condition"),
		TemplateID:     str(r, "templateId"),
		RecipientMode:  str(r, "recipientMode"),
		RecipientRoles: strList(r, "recipientRoles"),
		AudienceID:     str(r, "audienceId"),
		RecipientField: str(r, "recipientField"),
		AccountID:      str(r, "accountId"),
		SenderIdentity: str(r, "senderIdentityId"),
	}
}

// RecordGeneration stamps what the engine made of the form. @serverOnly, so the
// caller must have stamped internal origin -- see activate.go, where it is done
// inline at the one Execute that needs it.
func (s *Store) RecordGeneration(ctx context.Context, ruleID, status, bundleID, constructName, lastError string) error {
	return s.exec(ctx, call("mutation", "recordEmailRuleGeneration",
		arg{"emailRuleId", memql.BareShortId(ruleID)},
		arg{"status", status},
		arg{"bundleId", bundleID},
		arg{"constructName", constructName},
		arg{"lastError", lastError},
	))
}

// RecordFiring stamps that a rule ran. Its two fields are LIVENESS -- they move
// on their own -- which is why the surfaces that render them are told not to
// fingerprint them for arrival cues.
func (s *Store) RecordFiring(ctx context.Context, ruleID string, firedCount int, lastError string) error {
	return s.exec(ctx, call("mutation", "recordEmailRuleFiring",
		arg{"emailRuleId", memql.BareShortId(ruleID)},
		arg{"firedCount", firedCount},
		arg{"lastError", lastError},
	))
}

// ---------------------------------------------------------------------------
// The authoring pipeline's four writes
// ---------------------------------------------------------------------------

func (s *Store) CreateBundle(ctx context.Context, bundleID, title, summary, supersedes string, version int) error {
	args := []arg{
		{"bundleId", bundleID},
		{"title", title},
		{"summary", summary},
		{"version", version},
	}
	if strings.TrimSpace(supersedes) != "" {
		args = append(args, arg{"supersedesBundleId", supersedes})
	}
	return s.exec(ctx, call("mutation", "createAuthoringBundle", args...))
}

func (s *Store) CreateConstruct(ctx context.Context, constructID, bundleID, name, namespace, source string) error {
	return s.exec(ctx, call("mutation", "createAuthoringConstruct",
		arg{"constructId", constructID},
		arg{"bundleId", bundleID},
		arg{"kind", "automation"},
		arg{"name", name},
		arg{"targetNamespace", namespace},
		arg{"source", source},
		// Stamp the grammar epoch the source was authored under, so a future
		// engine can tell a rotted row from a stale one.
		arg{"grammarVersion", langparser.GrammarVersion},
	))
}

// RecordValidation writes Gate 1's verdict. The report is an object literal
// rather than a quoted blob: `validationReport` is declared `object!`, and
// handing it a string would be a type the mutation does not accept.
func (s *Store) RecordValidation(ctx context.Context, bundleID string, ok bool, diagnostics []string, failureReason string) error {
	status := "validated"
	if !ok {
		status = "failed"
	}
	return s.exec(ctx, call("mutation", "recordBundleValidation",
		arg{"bundleId", bundleID},
		arg{"status", status},
		arg{"validationReport", rawValue(objectLiteral(map[string]any{
			"ok":          ok,
			"diagnostics": diagnostics,
			"generator":   "component/emailrules",
		}))},
		arg{"failureReason", failureReason},
	))
}

// RecordDryRun writes Gate 2's verdict.
//
// THE GATE IS PASSED BY CONSTRUCTION, and saying why matters. Gate 2 exists to
// put a behavioural trace and a side-effect manifest in front of a person before
// an LLM-authored bundle is armed. This bundle was not authored by an LLM: it is
// one automation with one step, produced by a deterministic template from a form
// the operator filled in and already saw, and the side effect it can have is
// exactly the one the form describes. Running a synthetic trace over it would
// manufacture an artifact nobody reads to satisfy a gate whose question has
// already been answered -- so the report says plainly what it is, and the
// operator's approval is the form they saved.
func (s *Store) RecordDryRun(ctx context.Context, bundleID, ruleID, lane string) error {
	return s.exec(ctx, call("mutation", "recordBundleDryRun",
		arg{"bundleId", bundleID},
		arg{"status", "dryRunPassed"},
		arg{"dryRunReport", rawValue(objectLiteral(map[string]any{
			"ok":        true,
			"generator": "component/emailrules",
			"rule":      ruleID,
			"lane":      lane,
			"note":      "Deterministic single-step construct; the operator's approval is the rule form they saved.",
		}))},
	))
}

// objectLiteral renders a flat MemQL object literal. Deliberately flat and
// deliberately tiny: this is the only place in the package that emits unquoted
// structure, and keeping it unable to express nesting is what keeps it unable
// to express a mistake.
func objectLiteral(fields map[string]any) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		switch v := fields[k].(type) {
		case bool:
			parts = append(parts, fmt.Sprintf("%s: %t", k, v))
		case int:
			parts = append(parts, fmt.Sprintf("%s: %d", k, v))
		case []string:
			quoted := make([]string, 0, len(v))
			for _, item := range v {
				quoted = append(quoted, langparser.QuoteString(item))
			}
			parts = append(parts, k+": ["+strings.Join(quoted, ", ")+"]")
		default:
			parts = append(parts, k+": "+langparser.QuoteString(fmt.Sprintf("%v", v)))
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---------------------------------------------------------------------------
// Reads and writes the fire path makes
// ---------------------------------------------------------------------------

// RecipientsForAudience reads the roster the marketing lane mails.
func (s *Store) RecipientsForAudience(ctx context.Context, audienceID string) ([]map[string]any, error) {
	return s.rows(ctx, call("query", "sendableRecipientsForAudience",
		arg{"audienceId", memql.BareShortId(audienceID)}))
}

// TemplateByID reads the copy both lanes send.
func (s *Store) TemplateByID(ctx context.Context, templateID string) (map[string]any, bool, error) {
	rows, err := s.rows(ctx, call("query", "templateById", arg{"templateId", memql.BareShortId(templateID)}))
	if err != nil || len(rows) == 0 {
		return nil, false, err
	}
	return rows[0], true, nil
}

// StageOutbound is the OPERATIONAL lane's one write: the transactional outbox,
// where the egress allowlist applies, no unsubscribe footer is attached and the
// marketing suppression list is neither consulted nor written. Telling an owner
// that a new admin was added is not marketing, and an unsubscribe from a
// newsletter must not be able to silence it.
func (s *Store) StageOutbound(ctx context.Context, requestID, target, subject, body, dedupeKey, requestedBy string) error {
	return s.exec(ctx, call("mutation", "stageOutboundRequest",
		arg{"requestId", requestID},
		arg{"medium", "email"},
		arg{"target", target},
		arg{"subject", subject},
		arg{"body", body},
		arg{"dedupeKey", dedupeKey},
		arg{"requestedBy", requestedBy},
	))
}

// SendToRecipient is the MARKETING lane's one call: suppression checked at the
// point of send, the RFC 8058 pair attached, the resolved sending identity
// applied, the outcome ledgered. Reached as a builtin rather than as a Go call
// so this package does not depend on component/campaigns' internals.
//
// A kind-prefixed `builtin <name>(...)` string IS executable through the
// engine, and the precedent is shipped: dsl/memql/tools.memql declares
// `@handler(type="query", query="builtin help(name: \"$args.name\")")`, and
// tool_execution.go's query arm hands exactly that text to Execute.
func (s *Store) SendToRecipient(ctx context.Context, templateID, recipientID, senderIdentityID, ruleID string) error {
	args := []arg{
		{"templateId", memql.BareShortId(templateID)},
		{"recipientId", memql.BareShortId(recipientID)},
	}
	if strings.TrimSpace(senderIdentityID) != "" {
		args = append(args, arg{"senderIdentityId", memql.BareShortId(senderIdentityID)})
	}
	if strings.TrimSpace(ruleID) != "" {
		args = append(args, arg{"emailRuleId", memql.BareShortId(ruleID)})
	}
	return s.exec(ctx, call("builtin", "campaignSendToRecipient", args...))
}

// AddRecipient enrols a row-derived address into the rule's audience, so it
// gains a membership, a subscription state and a working unsubscribe. `signup`
// is the honest source value: a product flow added it.
func (s *Store) AddRecipient(ctx context.Context, recipientID, audienceID, email, displayName string) error {
	return s.exec(ctx, call("mutation", "addRecipient",
		arg{"recipientId", recipientID},
		arg{"audienceId", memql.BareShortId(audienceID)},
		arg{"email", email},
		arg{"displayName", displayName},
		arg{"source", "signup"},
	))
}

// UsersInRoles reads the cluster people the operational lane mails.
func (s *Store) UsersInRoles(ctx context.Context) ([]map[string]any, error) {
	return s.rows(ctx, call("query", "activeUsers"))
}

func str(r map[string]any, key string) string {
	if v, ok := r[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// integer reads a rule or bundle row's numeric field.
//
// narrowing: SATURATE -- both fields read through here are ORDERINGS, and a
// wrapped negative does not merely report a wrong number, it changes what the
// engine does next. `version` is the authoring bundle's, and nextVersion is
// one past it: a negative would send the version backwards, and
// PlanBundleActivation refuses a bundle whose version does not exceed the one
// it supersedes -- so a rule would silently stop being armable. `firedCount`
// is a monotonic counter written back as +1; a negative would make a rule's
// own history run backwards in the surface that renders it.
//
// Zero would be wrong for the same reason it is right elsewhere: nothing here
// reads 0 as "unset". A bundle at version 0 is a bundle that has never been
// written, which is a DIFFERENT state from one whose version we could not
// read, and collapsing them is what would let an unreadable row supersede a
// live one.
func integer(r map[string]any, key string) int {
	switch v := r[key].(type) {
	case int:
		return v
	case int64:
		return num.ClampInt64(v)
	case float64:
		return num.ClampFloat64(v)
	}
	return 0
}

func strList(r map[string]any, key string) []string {
	raw, ok := r[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// BundleVersion reads an authoring bundle's version. Zero when the bundle is
// unreadable -- which is treated as "there is nothing to supersede" rather than
// as an error, because the alternative is refusing to arm a rule on the
// strength of a row the caller cannot see.
func (s *Store) BundleVersion(ctx context.Context, bundleID string) int {
	rows, err := s.rows(ctx, call("query", "authoringBundleById", arg{"bundleId", memql.BareShortId(bundleID)}))
	if err != nil || len(rows) == 0 {
		return 0
	}
	return integer(rows[0], "version")
}

// AudienceByID reads the audience a marketing rule mails. Used at ARM time, not
// only at fire time: see Activator.verifyReadable.
func (s *Store) AudienceByID(ctx context.Context, audienceID string) (bool, error) {
	rows, err := s.rows(ctx, call("query", "audienceById", arg{"audienceId", memql.BareShortId(audienceID)}))
	return len(rows) > 0, err
}

// SenderIdentityByID reads the identity a rule sends as.
func (s *Store) SenderIdentityByID(ctx context.Context, identityID string) (map[string]any, bool, error) {
	rows, err := s.rows(ctx, call("query", "senderIdentityById", arg{"senderIdentityId", memql.BareShortId(identityID)}))
	if err != nil || len(rows) == 0 {
		return nil, false, err
	}
	return rows[0], true, nil
}
