package emailrules

// activate.go -- arming a rule, immediately.
//
// # The gap this closes
//
// `MemQLEngine.ActivateApprovedBundle` shipped with increment 5 of the authoring
// epic and, until this file, had NO PRODUCTION CALLER anywhere in the tree --
// only tests and prose. The one durable path that existed (the cockpit's promote
// store) sidestepped it by writing the bundle straight to `active` and relying on
// the boot re-arm to notice, which means an authored construct took effect at
// NEXT BOOT. For a capability an operator promotes once that is merely slow. For
// a rule somebody creates in an app and expects to fire, it is the feature not
// working: they save it, nothing happens, and nothing says why.
//
// So this path drives the real gates and then calls the real activation entry
// point, and the rule is live before the call returns.
//
// # The five steps, and why each one is there
//
//	1. GENERATE   the automation source from the form (generate.go, pure).
//	2. VALIDATE   with memql.ValidateBundle -- Gate 1, the REAL parser and
//	              binder against a read-only clone of the live concept registry.
//	              This is what catches a trigger concept that does not exist and
//	              a condition the filter grammar refuses, and it catches them
//	              with the parser's own sentence, which is a better message than
//	              anything this package could invent.
//	3. WRITE      bundle + construct rows, then record the two gate verdicts.
//	4. ACTIVATE   ActivateApprovedBundle, which flips the statuses, registers the
//	              construct into the owner-scoped runtime AND hands the automation
//	              to the scheduler, promotes deps, retires the superseded prior
//	              version, and audits.
//	5. RECORD     what happened onto the rule row, so the app can render it.
//
// # Every refusal lands on the rule row
//
// A rule whose generation, validation or activation failed goes to `failed` with
// the ENGINE's own sentence on `lastError`, and the app renders that sentence
// verbatim. The alternative -- a rule that stays `draft` while an error lives in
// a log line on one replica -- is the silence this whole feature exists to
// remove.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// ActivationEngine is the concrete engine surface the activation path needs.
// Narrow on purpose: this package has no business with the rest of the engine,
// and a narrow seam is one a test can stand in for.
type ActivationEngine interface {
	Engine
	ActivateApprovedBundle(ctx context.Context, owner, bundleId string, deps memql.AuthoredRuntimeDeps) (memql.ActivationResult, error)
	RetireActiveBundle(ctx context.Context, owner, bundleId string, deps memql.AuthoredRuntimeDeps) error
}

// Activator arms and retires rules.
type Activator struct {
	engine ActivationEngine
	store  *Store
	deps   func() memql.AuthoredRuntimeDeps
}

func NewActivator(engine ActivationEngine, deps func() memql.AuthoredRuntimeDeps) *Activator {
	return &Activator{engine: engine, store: NewStore(engine), deps: deps}
}

// Result is what the builtin hands back.
type Result struct {
	RuleID        string
	Status        string
	BundleID      string
	ConstructName string
	Lane          string
	Error         string
}

// Activate generates, validates, writes and arms a rule's construct.
//
// `owner` is the AUTHENTICATED caller. It must equal the rule's owner, and the
// check is not decoration: the generated automation runs under the AUTHOR's
// envelope, so activating somebody else's rule would be running their reads
// under their identity on your say-so. `ActivateApprovedBundle` enforces the
// same rule at the bundle grain; this refuses earlier, with a sentence about
// rules rather than about bundles.
func (a *Activator) Activate(ctx context.Context, owner, ruleID string) (Result, error) {
	if a.deps == nil {
		return Result{}, fmt.Errorf("emailrules: the authored runtime is not wired on this node, so a rule cannot be armed here")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return Result{}, fmt.Errorf("emailrules: arming a rule needs an authenticated caller; the generated automation runs under their envelope")
	}

	// AUTHORING IS THE FLOOR, and it is the same floor the rest of the
	// authoring surface stands on. A rule is a person writing an automation
	// that mails other people in this cluster, and `auth.CanAuthor` -- owner or
	// developer -- is the tier the tree already gates that on. Without it a
	// writer could arm a rule that mails every admin content of their choosing,
	// which is a phishing primitive with a scheduler attached. Row authz below
	// still decides WHICH rule; this decides whether arming one is a thing the
	// caller does at all.
	if ac, ok := auth.AccessFromContext(ctx); !ok || ac == nil || !auth.CanAuthor(auth.UserContext{Role: ac.Role}) {
		return Result{}, fmt.Errorf("emailrules: arming an event-email rule is owner or developer only -- it authors an automation that sends mail on this cluster's behalf")
	}

	rule, found, err := a.store.RuleByID(ctx, ruleID)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return Result{}, fmt.Errorf("emailrules: rule %q not found", ruleID)
	}
	if rule.OwnerUserID != memql.BareShortId(owner) && rule.OwnerUserID != owner {
		return Result{}, fmt.Errorf("emailrules: %q cannot arm a rule owned by %q -- the generated automation runs under its author's envelope", owner, rule.OwnerUserID)
	}

	state, _, err := a.store.RuleStateByID(ctx, ruleID)
	if err != nil {
		return Result{}, err
	}

	res := Result{RuleID: rule.ID, Lane: LaneFor(rule.RecipientMode)}

	// 1. Generate. A form the generator refuses never reaches the pipeline, and
	// the refusal is about the FORM, in the operator's own vocabulary.
	source, err := GenerateAutomation(rule)
	if err != nil {
		return a.fail(ctx, rule.ID, res, err)
	}
	res.ConstructName = ConstructNameFor(rule.ID)

	// 2. Validate -- Gate 1, the real parser and binder.
	report := memql.ValidateBundle(source, "campaigns/automations.memql")
	if !report.OK {
		reasons := diagnosticSentences(report)
		bundleID := bundleIDFor(rule.ID, a.nextVersion(ctx, state))
		// The failed gate is RECORDED rather than skipped: a bundle that exists
		// and says why it failed is one an operator can look at, and the same
		// row is what tells the next activation which version it supersedes.
		_ = a.writeBundle(ctx, bundleID, rule, state, source)
		_ = a.recordValidation(ctx, bundleID, false, reasons, strings.Join(reasons, "; "))
		return a.fail(ctx, rule.ID, res, fmt.Errorf("emailrules: the generated automation does not compile: %s", strings.Join(reasons, "; ")))
	}

	// 3. Write the rows and both gate verdicts.
	version := a.nextVersion(ctx, state)
	bundleID := bundleIDFor(rule.ID, version)
	res.BundleID = bundleID
	if err := a.writeBundle(ctx, bundleID, rule, state, source); err != nil {
		return a.fail(ctx, rule.ID, res, err)
	}
	if err := a.recordValidation(ctx, bundleID, true, nil, ""); err != nil {
		return a.fail(ctx, rule.ID, res, err)
	}
	if err := a.recordDryRun(ctx, bundleID, rule.ID, res.Lane); err != nil {
		return a.fail(ctx, rule.ID, res, err)
	}

	// 4. Arm it. THIS is the call that had no production caller.
	if _, err := a.engine.ActivateApprovedBundle(ctx, rule.OwnerUserID, bundleID, a.deps()); err != nil {
		return a.fail(ctx, rule.ID, res, err)
	}

	// 5. Record the success on the rule row.
	res.Status = "active"
	if err := a.record(ctx, rule.ID, "active", bundleID, res.ConstructName, ""); err != nil {
		return res, err
	}
	return res, nil
}

// Retire tears a rule's construct down. The RULE row survives with its history:
// a rule somebody turned off is a different thing from one that never existed,
// and an app that deleted the row would have nothing to show for the mail it
// already sent.
func (a *Activator) Retire(ctx context.Context, owner, ruleID string) (Result, error) {
	if a.deps == nil {
		return Result{}, fmt.Errorf("emailrules: the authored runtime is not wired on this node, so a rule cannot be retired here")
	}
	rule, found, err := a.store.RuleByID(ctx, ruleID)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return Result{}, fmt.Errorf("emailrules: rule %q not found", ruleID)
	}
	state, _, err := a.store.RuleStateByID(ctx, ruleID)
	if err != nil {
		return Result{}, err
	}
	res := Result{RuleID: rule.ID, BundleID: state.BundleID, ConstructName: state.ConstructName, Lane: LaneFor(rule.RecipientMode)}
	if strings.TrimSpace(state.BundleID) == "" {
		// Nothing was ever armed. Say so as a status rather than as an error:
		// retiring a draft is a reasonable thing to ask for and a no-op is the
		// correct answer to it.
		res.Status = "draft"
		return res, a.record(ctx, rule.ID, "draft", "", "", "")
	}
	if err := a.engine.RetireActiveBundle(ctx, rule.OwnerUserID, state.BundleID, a.deps()); err != nil {
		return a.fail(ctx, rule.ID, res, err)
	}
	res.Status = "paused"
	return res, a.record(ctx, rule.ID, "paused", "", "", "")
}

func (a *Activator) writeBundle(ctx context.Context, bundleID string, rule Rule, state RuleState, source string) error {
	if err := a.store.CreateBundle(ctx, bundleID, BundleTitleFor(rule), summaryFor(rule), state.BundleID, a.nextVersion(ctx, state)); err != nil {
		return err
	}
	return a.store.CreateConstruct(ctx, constructIDFor(bundleID), bundleID, ConstructNameFor(rule.ID), "campaigns", source)
}

// The three writes below reach @serverOnly or gate mutations, so they stamp
// internal origin INLINE at the one call that needs it -- never on a ctx bound
// to a variable, which a later frame would inherit (memql#2879).
func (a *Activator) recordValidation(ctx context.Context, bundleID string, ok bool, diagnostics []string, reason string) error {
	return a.store.RecordValidation(auth.ContextWithInternalOrigin(ctx), bundleID, ok, diagnostics, reason)
}

func (a *Activator) recordDryRun(ctx context.Context, bundleID, ruleID, lane string) error {
	return a.store.RecordDryRun(auth.ContextWithInternalOrigin(ctx), bundleID, ruleID, lane)
}

func (a *Activator) record(ctx context.Context, ruleID, status, bundleID, constructName, lastError string) error {
	return a.store.RecordGeneration(auth.ContextWithInternalOrigin(ctx), ruleID, status, bundleID, constructName, lastError)
}

// fail stamps the refusal where the operator is looking and returns it. Both,
// deliberately: the row is what the app renders and the error is what the
// caller sees, and a failure visible in only one of the two is one somebody
// reports as the other not working.
func (a *Activator) fail(ctx context.Context, ruleID string, res Result, cause error) (Result, error) {
	res.Status = "failed"
	res.Error = cause.Error()
	if err := a.record(ctx, ruleID, "failed", res.BundleID, res.ConstructName, truncate(cause.Error(), 4096)); err != nil {
		return res, fmt.Errorf("%w (and the reason could not be recorded on the rule: %v)", cause, err)
	}
	return res, cause
}

func summaryFor(r Rule) string {
	who := map[string]string{
		ModeClusterRoles: "people in this cluster",
		ModeAudience:     "an audience",
		ModeRowAddress:   "an address on the triggering row",
	}[r.RecipientMode]
	return fmt.Sprintf("When a %s is %s, email %s to %s. Generated from %s; the %s lane carries it.",
		r.TriggerConcept, r.EventKind, r.TemplateID, who, r.ID, LaneFor(r.RecipientMode))
}

// bundleIDFor is DERIVED, and per version. Deriving it means a retried
// activation reuses the id rather than accumulating a second bundle for one
// attempt; including the version means an EDIT produces a new bundle the
// activation planner can supersede the old one with, which is what makes the
// old automation stop firing the instant the new one is armed.
func bundleIDFor(ruleID string, version int) string {
	sum := sha256.Sum256([]byte(ruleID + "\x00" + fmt.Sprint(version)))
	return "emr" + hex.EncodeToString(sum[:16])
}

func constructIDFor(bundleID string) string {
	sum := sha256.Sum256([]byte(bundleID + "\x00automation"))
	return "emc" + hex.EncodeToString(sum[:16])
}

// nextVersion is one past whatever is armed, read off the armed bundle rather
// than remembered on the rule. A version field on the rule row would be a
// second place for the same number to live, and the two would disagree the
// first time an activation half-failed.
//
// A bundle we cannot read is treated as version 0, so the next attempt writes
// version 1 at a fresh id -- which is the right failure: it arms something,
// rather than colliding with a row it could not see.
func (a *Activator) nextVersion(ctx context.Context, state RuleState) int {
	if strings.TrimSpace(state.BundleID) == "" {
		return 1
	}
	return a.store.BundleVersion(ctx, state.BundleID) + 1
}

func diagnosticSentences(report memql.SandboxReport) []string {
	out := make([]string, 0, len(report.Diagnostics))
	for _, d := range report.Diagnostics {
		if d.Error != "" {
			out = append(out, d.Error)
		}
	}
	if len(out) == 0 {
		out = append(out, "the bundle produced no recognizable constructs")
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
