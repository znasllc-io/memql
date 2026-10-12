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
// remove. An EDIT that is refused is the exception to `failed`: the version
// that was live keeps running, so the row keeps naming it and reading `active`,
// and `lastError` says why the edit did not take and that the previous version
// is still running -- decided from a fresh read of the row and the bundle it
// names, and written against the version of the row read, so a breaker's
// pause, another activation or a failed turn-off that landed meanwhile is not
// overwritten (refuse). The row records what happened -- a caller that hangs
// up after the activation went live does not stop it being recorded, and an
// attempt that ended because the caller hung up leaves the row as it was.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

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

// EngineAdapter fits the live *memql.MemQLEngine to ActivationEngine.
//
// The two differ in exactly one thing: the engine returns a *ExecuteResult
// where this package's read helpers take `any` -- and MaterializeRows accepts
// either. So the adapter is one method wide, and it lives here rather than in
// app/ for the same reason CampaignsEngineAdapter lives beside its consumer:
// the shape being adapted is this package's, and a wrapper written at the call
// site is one that has to be rewritten at the next call site.
type EngineAdapter struct {
	Engine *memql.MemQLEngine
}

func (a EngineAdapter) Execute(ctx context.Context, query string) (any, error) {
	return a.Engine.Execute(ctx, query)
}

func (a EngineAdapter) ActivateApprovedBundle(ctx context.Context, owner, bundleId string, deps memql.AuthoredRuntimeDeps) (memql.ActivationResult, error) {
	return a.Engine.ActivateApprovedBundle(ctx, owner, bundleId, deps)
}

func (a EngineAdapter) RetireActiveBundle(ctx context.Context, owner, bundleId string, deps memql.AuthoredRuntimeDeps) error {
	return a.Engine.RetireActiveBundle(ctx, owner, bundleId, deps)
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

	// 0. Verify the rule's world is readable FROM THE ENVELOPE THE RULE WILL
	// RUN UNDER. The construct runs as its author, role writer, no internal
	// origin -- so a rule naming a template, audience or identity the author
	// cannot read is a rule that fires forever and mails nobody, correctly,
	// silently, on every matching row. Checking at arm time turns that into a
	// refusal the operator reads once, at the moment they can still fix it.
	//
	// The check runs under the ARMING caller's ctx, which is the author's --
	// the ownership gate above has already established that they are the same
	// person, which is what makes this check mean what it says.
	if err := a.verifyReadable(ctx, rule); err != nil {
		return a.refuse(ctx, rule.ID, res, state, err)
	}

	// 1. Generate. A form the generator refuses never reaches the pipeline, and
	// the refusal is about the FORM, in the operator's own vocabulary.
	source, err := GenerateAutomation(rule)
	if err != nil {
		return a.refuse(ctx, rule.ID, res, state, err)
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
		return a.refuse(ctx, rule.ID, res, state, fmt.Errorf("emailrules: the generated automation does not compile: %s", strings.Join(reasons, "; ")))
	}

	// 3. Write the rows and both gate verdicts.
	version := a.nextVersion(ctx, state)
	bundleID := bundleIDFor(rule.ID, version)
	res.BundleID = bundleID
	if err := a.writeBundle(ctx, bundleID, rule, state, source); err != nil {
		return a.refuse(ctx, rule.ID, res, state, err)
	}
	if err := a.recordValidation(ctx, bundleID, true, nil, ""); err != nil {
		return a.refuse(ctx, rule.ID, res, state, err)
	}
	if err := a.recordDryRun(ctx, bundleID, rule.ID, res.Lane); err != nil {
		return a.refuse(ctx, rule.ID, res, state, err)
	}

	// 4. Arm it. THIS is the call that had no production caller.
	//
	// The gate is handed the AUTHENTICATED caller, never rule.OwnerUserID.
	// The two name one person -- the check above established that -- but not
	// in one spelling: ruleFromRow bare-ifies the owner ("owner1"), while the
	// gate compares against the bundle's ownerUserId, stamped from this same
	// caller in writeBundle and stored canonical ("v1:identity:user:owner1").
	// Handing it the bare form refused every activation, for every person,
	// once the generated construct first compiled.
	if _, err := a.engine.ActivateApprovedBundle(ctx, owner, bundleID, a.deps()); err != nil {
		return a.refuse(ctx, rule.ID, res, state, err)
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
	// The caller, for the reason Activate hands the gate the caller: the gate
	// compares against the bundle's canonical ownerUserId, and rule.OwnerUserID
	// is the bare spelling. It is also this call's only ownership check -- the
	// gate refuses anyone but the bundle's owner.
	if err := a.engine.RetireActiveBundle(ctx, strings.TrimSpace(owner), state.BundleID, a.deps()); err != nil {
		return a.fail(ctx, rule.ID, res, err)
	}
	res.Status = "paused"
	return res, a.record(ctx, rule.ID, "paused", "", "", "")
}

// verifyReadable re-reads everything the rule will need, under the caller's own
// actor. It is deliberately not a null check on the ids: an id that is present
// and unreadable is exactly the case that looks correct on the form and fails
// at fire time.
func (a *Activator) verifyReadable(ctx context.Context, rule Rule) error {
	if _, ok, err := a.store.TemplateByID(ctx, rule.TemplateID); err != nil {
		return fmt.Errorf("emailrules: could not read template %q: %w", rule.TemplateID, err)
	} else if !ok {
		return fmt.Errorf("emailrules: template %q is not readable by this rule's author, so the rule would fire and mail nobody", rule.TemplateID)
	}
	if LaneFor(rule.RecipientMode) == "marketing" {
		if ok, err := a.store.AudienceByID(ctx, rule.AudienceID); err != nil {
			return fmt.Errorf("emailrules: could not read audience %q: %w", rule.AudienceID, err)
		} else if !ok {
			return fmt.Errorf("emailrules: audience %q is not readable by this rule's author, so the rule would fire and mail nobody", rule.AudienceID)
		}
	}
	if id := strings.TrimSpace(rule.SenderIdentity); id != "" {
		row, ok, err := a.store.SenderIdentityByID(ctx, id)
		if err != nil {
			return fmt.Errorf("emailrules: could not read sending identity %q: %w", id, err)
		}
		if !ok {
			return fmt.Errorf("emailrules: sending identity %q is not readable by this rule's author", id)
		}
		// A disabled identity is refused HERE for the same reason a campaign
		// naming one is refused at preflight: a silent fallback to the default
		// mailbox mails a client's list from the wrong address, and nothing
		// says so.
		if st := str(row, "status"); st == "disabled" {
			return fmt.Errorf("emailrules: sending identity %q is disabled; enable it or pick another rather than letting the rule fall back to the default mailbox", id)
		}
	}
	return nil
}

// writeBundle writes the bundle, as a draft, then its one construct. The
// construct write is @serverOnly (memql#6022) -- an active bundle arms every
// construct it holds, so only the pipeline that runs the gates writes one, and
// only while the bundle is a draft -- so it stamps internal origin, inline.
func (a *Activator) writeBundle(ctx context.Context, bundleID string, rule Rule, state RuleState, source string) error {
	if err := a.store.CreateBundle(ctx, bundleID, BundleTitleFor(rule), summaryFor(rule), state.BundleID, a.nextVersion(ctx, state)); err != nil {
		return err
	}
	return a.store.CreateConstruct(auth.ContextWithInternalOrigin(ctx), constructIDFor(bundleID), bundleID, ConstructNameFor(rule.ID), "campaigns", source)
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

// record writes what happened onto the rule row, on a context the caller
// cannot cancel (memql#6022): an activation that went live, or a retire that
// went through, has happened whether or not the client is still listening,
// and a row left naming the version it replaced makes turning the rule off
// retire the wrong bundle and turning it on a conflict with the live one. An
// attempt the caller abandoned is not recorded at all (abandoned).
func (a *Activator) record(ctx context.Context, ruleID, status, bundleID, constructName, lastError string) error {
	return a.recordAt(ctx, ruleID, time.Time{}, status, bundleID, constructName, lastError)
}

// recordAt is record, conditional on the rule row still being the version
// read at `at` -- a compare-and-set: a record that lost the race to another
// write fails with memql.ErrRowVersionChanged instead of overwriting what
// landed in between. A zero `at` (a read that carried no version) records
// unconditionally.
func (a *Activator) recordAt(ctx context.Context, ruleID string, at time.Time, status, bundleID, constructName, lastError string) error {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	return a.store.RecordGeneration(auth.ContextWithInternalOrigin(fencedOnRule(recordCtx, ruleID, at)), ruleID, status, bundleID, constructName, lastError)
}

// fencedOnRule makes a write conditional on the version of the rule row that
// was read. A zero version leaves the write as it was: a zero witness would
// require the row to be absent, which refuses every write to a rule that
// exists.
func fencedOnRule(ctx context.Context, ruleID string, at time.Time) context.Context {
	if at.IsZero() {
		return ctx
	}
	return memql.ContextWithRowVersionFence(ctx, RuleConcept, ruleID, at)
}

// recordTimeout bounds a rule-row record that no caller can cancel.
const recordTimeout = 30 * time.Second

// recordAttempts bounds how often a record conditional on the row it read is
// decided again after another write landed on the row in between.
const recordAttempts = 3

// abandoned reports whether an attempt ended because its caller gave up: the
// error it ended with IS the caller's cancellation or deadline. Nothing took
// effect -- an activation that fails undoes what it did before its commit
// point, and a retire that fails has not retired -- so the row is left as it
// was: a hang-up is not something the operator can fix on the rule, and
// "context canceled" is not a reason to show them. A refusal on the merits
// that completes after the caller left is not abandoned: it is the operator's
// to read on the row.
func abandoned(cause error) bool {
	return errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)
}

// PreviousVersionStillRunning ends the reason a refused edit records: the
// version that was live before the edit is still armed and still sending.
const PreviousVersionStillRunning = "The previous version is still running."

// refuse records a refused activation where the operator is looking -- or
// leaves the row alone, when what it says is still true or is someone else's
// account.
//
// The refusal changed nothing about what is running: ActivateApprovedBundle
// leaves a live version armed when it refuses a new one, and every earlier
// refusal never reached it. What the row should say depends on what is true
// NOW, not on the state read when this attempt began -- a breaker may have
// paused the live version meanwhile, another activation may have gone live, a
// turn-off may have failed -- so the row and the bundle it names are re-read
// fresh, and the record is written against the version of the row that was
// read (recordAt), decided again if another write lands in between:
//
//   - the row names a different bundle than when this attempt began: another
//     activation, a retire or an edit wrote it since, and it is left alone;
//   - it reads active and names a bundle that reads active: the version that
//     was live keeps running, so the row keeps naming it and reading active,
//     and says why the edit did not take and that the previous version is
//     still running;
//   - the bundle it names reads active but the row does not -- a turn-off that
//     failed reads failed -- or the row reads paused naming the bundle a
//     breaker stopped: left alone, so the row keeps naming the bundle a
//     turn-off must reach, and a stop keeps its account;
//   - otherwise nothing it names is running: the rule reads failed with the
//     refusal, naming this attempt's bundle -- or, when this attempt wrote
//     none, the one it named.
func (a *Activator) refuse(ctx context.Context, ruleID string, res Result, state RuleState, cause error) (Result, error) {
	res.Error = cause.Error()
	if abandoned(cause) {
		return asRead(res, state), cause
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	for attempt := 0; attempt < recordAttempts; attempt++ {
		now, found, err := a.store.RuleStateByID(memql.ContextWithFreshRead(readCtx), ruleID)
		if err != nil {
			return res, fmt.Errorf("%w (and the rule could not be read again to record it: %v)", cause, err)
		}
		if !found || !sameBundle(now.BundleID, state.BundleID) {
			return asRead(res, now), cause
		}
		named := strings.TrimSpace(now.BundleID)
		live := false
		if named != "" {
			status, _, err := a.store.BundleStatus(memql.ContextWithFreshRead(readCtx), named)
			if err != nil {
				return res, fmt.Errorf("%w (and the version the rule names could not be read to record it: %v)", cause, err)
			}
			live = status == "active"
		}
		var status, bundleID, constructName, lastError string
		switch {
		case live && now.Status == "active":
			status, bundleID, constructName = "active", named, now.ConstructName
			lastError = asSentence(truncate(cause.Error(), 4000)) + " " + PreviousVersionStillRunning
		case live, now.Status == "paused" && named != "":
			return asRead(res, now), cause
		default:
			status, bundleID, constructName = "failed", res.BundleID, res.ConstructName
			if bundleID == "" {
				bundleID = named
			}
			lastError = truncate(cause.Error(), 4096)
		}
		if constructName == "" {
			constructName = now.ConstructName
		}
		err = a.recordAt(ctx, ruleID, now.VersionAt, status, bundleID, constructName, lastError)
		if errors.Is(err, memql.ErrRowVersionChanged) {
			continue
		}
		if err != nil {
			return res, fmt.Errorf("%w (and the reason could not be recorded on the rule: %v)", cause, err)
		}
		res.Status, res.BundleID, res.ConstructName = status, bundleID, constructName
		return res, cause
	}
	return res, fmt.Errorf("%w (and the rule kept changing, so the reason was not recorded on it)", cause)
}

// asRead is a refusal's result as the rule row reads: what is true, rather
// than what this attempt would have made it.
func asRead(res Result, row RuleState) Result {
	res.Status, res.BundleID = row.Status, row.BundleID
	if strings.TrimSpace(row.ConstructName) != "" {
		res.ConstructName = row.ConstructName
	}
	return res
}

// sameBundle compares two bundle ids however each is spelled.
func sameBundle(a, b string) bool {
	return memql.BareShortId(strings.TrimSpace(a)) == memql.BareShortId(strings.TrimSpace(b))
}

// asSentence ends text with a full stop unless it already ends a sentence.
func asSentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || strings.HasSuffix(text, ".") || strings.HasSuffix(text, "!") || strings.HasSuffix(text, "?") {
		return text
	}
	return text + "."
}

// fail stamps the refusal where the operator is looking and returns it. Both,
// deliberately: the row is what the app renders and the error is what the
// caller sees, and a failure visible in only one of the two is one somebody
// reports as the other not working.
func (a *Activator) fail(ctx context.Context, ruleID string, res Result, cause error) (Result, error) {
	res.Status = "failed"
	res.Error = cause.Error()
	if abandoned(cause) {
		return res, cause
	}
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
	return ruleBundlePrefix + hex.EncodeToString(sum[:16])
}

// ruleBundlePrefix starts every rule bundle id (bundleIDFor), which is how a
// bundle is recognised as a rule's without a read.
const ruleBundlePrefix = "emr"

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
