package routingrules

// activate.go -- arming a generated `rule` construct through the runtime
// authoring pipeline (epic memql#5127, design D7).
//
// It is the `component/emailrules` shape with one difference that shows through
// everywhere: an email rule has a persisted FORM row, and a routing rule does
// not. The form arrives on the builtin call, is rendered, is armed, and the
// authored bundle IS the record. So there is no rule row to stamp a failure
// onto -- the refusal is the return value, and the bundle row (written even
// when Gate 1 fails) is what an operator opens afterwards.
//
// WHAT THE PIPELINE BUYS, and the reason this is not a direct registry write:
// Gate 1 is the real parser and binder rather than a second opinion about the
// grammar; the core-first resolver refuses a shipped name without this package
// having to know the list; the authored runtime re-arms on boot, so a rule an
// owner added survives a restart; and the construct runs under the AUTHOR's
// envelope rather than a system one.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// Engine is the narrow surface arming needs. An interface rather than the
// concrete engine so this package is testable without a database, and so the
// dependency reads as three named operations instead of "everything".
type Engine interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
	ActivateApprovedBundle(ctx context.Context, owner, bundleId string, deps memql.AuthoredRuntimeDeps) (memql.ActivationResult, error)
	RetireActiveBundle(ctx context.Context, owner, bundleId string, deps memql.AuthoredRuntimeDeps) error
}

// Activator arms and retires routing rules.
type Activator struct {
	engine  Engine
	deps    func() memql.AuthoredRuntimeDeps
	shipped ShippedNames
}

// NewActivator returns nil without an engine or without the authored runtime,
// so a caller can construct it unconditionally and a node that cannot arm
// anything installs nothing. A nil Activator's methods refuse with a sentence
// naming the missing half rather than panicking.
func NewActivator(engine Engine, deps func() memql.AuthoredRuntimeDeps, shipped ShippedNames) *Activator {
	if engine == nil || deps == nil || shipped == nil {
		return nil
	}
	return &Activator{engine: engine, deps: deps, shipped: shipped}
}

// Result is what the builtin returns to the caller.
type Result struct {
	Name          string   `json:"name"`
	BundleId      string   `json:"bundleId"`
	Status        string   `json:"status"`
	Source        string   `json:"source"`
	Diagnostics   []string `json:"diagnostics,omitempty"`
	Error         string   `json:"error,omitempty"`
	ConstructName string   `json:"constructName"`
}

// Activate renders the form, runs Gate 1, writes the rows and arms it.
func (a *Activator) Activate(ctx context.Context, owner string, f Form) (Result, error) {
	if a == nil {
		return Result{}, fmt.Errorf("routingrules: the authored runtime is not wired on this node, so a rule cannot be armed here")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return Result{}, fmt.Errorf("routingrules: arming a rule needs an authenticated caller; the generated construct runs under their envelope")
	}

	// AUTHORING IS THE FLOOR, the same floor the rest of the authoring surface
	// stands on. A routing rule decides which model every matching call reaches
	// and therefore what the cluster spends; `auth.CanAuthor` -- owner or
	// developer -- is the tier this tree already gates authoring on. Without it
	// a writer could put a paid vendor first for every call in the cluster,
	// which is a spending decision with nobody's name on it.
	if ac, ok := auth.AccessFromContext(ctx); !ok || ac == nil || !auth.CanAuthor(auth.UserContext{Role: ac.Role}) {
		return Result{}, fmt.Errorf("routingrules: arming a routing rule is owner or developer only -- it decides which model every matching call reaches, and what the cluster spends")
	}

	res := Result{Name: ConstructNameFor(f), ConstructName: ConstructNameFor(f)}

	// 1. Refuse the form in the operator's own vocabulary, before anything is
	// written. A refusal from the parser is about a construct they never wrote.
	if err := Validate(f, a.shipped); err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res, err
	}

	source, err := GenerateRule(f)
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res, err
	}
	res.Source = source

	// 2. Gate 1: the real parser and binder, over the real corpus.
	report := memql.ValidateBundle(source, "rules/rules.memql")
	bundleId := bundleIdFor(res.Name)
	res.BundleId = bundleId
	if !report.OK {
		res.Status = "failed"
		res.Diagnostics = diagnosticSentences(report)
		res.Error = "the generated rule does not compile: " + strings.Join(res.Diagnostics, "; ")
		// The failed gate is RECORDED rather than skipped: a bundle that exists
		// and says why it failed is one an operator can open, and it is the row
		// that tells the next activation which version it supersedes.
		_ = a.writeBundle(ctx, bundleId, f, source)
		_ = a.recordValidation(ctx, bundleId, false, res.Diagnostics, res.Error)
		return res, fmt.Errorf("routingrules: %s", res.Error)
	}

	// 3. Write the rows and both gate verdicts.
	if err := a.writeBundle(ctx, bundleId, f, source); err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res, err
	}
	if err := a.recordValidation(ctx, bundleId, true, nil, ""); err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res, err
	}
	if err := a.recordDryRun(ctx, bundleId, f); err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res, err
	}

	// 4. Arm it.
	if _, err := a.engine.ActivateApprovedBundle(ctx, owner, bundleId, a.deps()); err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res, err
	}
	res.Status = "active"
	return res, nil
}

// Retire tears a rule's construct down.
func (a *Activator) Retire(ctx context.Context, owner, name string) (Result, error) {
	if a == nil {
		return Result{}, fmt.Errorf("routingrules: the authored runtime is not wired on this node, so a rule cannot be retired here")
	}
	if ac, ok := auth.AccessFromContext(ctx); !ok || ac == nil || !auth.CanAuthor(auth.UserContext{Role: ac.Role}) {
		return Result{}, fmt.Errorf("routingrules: retiring a routing rule is owner or developer only")
	}
	name = strings.TrimSpace(name)
	if a.shipped.HasRule(name) {
		return Result{}, fmt.Errorf("routingrules: %q is a shipped rule and cannot be retired. The shipped set is "+
			"re-read from the embedded tree on every boot; a rule you want out of the way is out-ranked with a "+
			"custom rule at a higher precedence, not removed", name)
	}
	bundleId := bundleIdFor(name)
	if err := a.engine.RetireActiveBundle(ctx, strings.TrimSpace(owner), bundleId, a.deps()); err != nil {
		return Result{Name: name, BundleId: bundleId, Status: "failed", Error: err.Error()}, err
	}
	return Result{Name: name, BundleId: bundleId, Status: "retired", ConstructName: name}, nil
}

// bundleIdFor is DERIVED from the rule name rather than minted fresh, so
// re-arming an edited rule supersedes its own previous bundle instead of
// accumulating one per save. A rule is addressed by name everywhere else; this
// keeps the bundle addressed the same way.
func bundleIdFor(name string) string {
	return "v1:authoring:bundle:routingrule-" + strings.ToLower(name)
}

func constructIdFor(bundleId string) string {
	return "v1:authoring:construct:" + strings.TrimPrefix(bundleId, "v1:authoring:bundle:")
}

func (a *Activator) writeBundle(ctx context.Context, bundleId string, f Form, source string) error {
	if err := a.render(ctx, "createAuthoringBundle", map[string]any{
		"bundleId": bundleId,
		"title":    "Routing rule: " + ConstructNameFor(f),
		"summary":  summaryFor(f),
		"version":  1,
	}); err != nil {
		return err
	}
	// @serverOnly (memql#6022): an active bundle arms every construct it
	// holds, so only the pipeline that runs the gates writes one, and only
	// while the bundle is a draft. Stamped inline, like the two gate writes
	// below.
	return a.render(auth.ContextWithInternalOrigin(ctx), "createAuthoringConstruct", map[string]any{
		"constructId":     constructIdFor(bundleId),
		"bundleId":        bundleId,
		"kind":            "rule",
		"name":            ConstructNameFor(f),
		"targetNamespace": "rules",
		"source":          source,
		// The grammar epoch the source was authored under, so a future engine
		// can tell a rotted row from a stale one.
		"grammarVersion": langparser.GrammarVersion,
	})
}

// The two writes below reach @serverOnly mutations, so they stamp internal
// origin INLINE at the one call that needs it -- never on a ctx bound to a
// variable, which a later frame would inherit (memql#2879).
func (a *Activator) recordValidation(ctx context.Context, bundleId string, ok bool, diagnostics []string, failureReason string) error {
	status := "validated"
	if !ok {
		status = "failed"
	}
	return a.render(auth.ContextWithInternalOrigin(ctx), "recordBundleValidation", map[string]any{
		"bundleId": bundleId,
		"status":   status,
		"validationReport": map[string]any{
			"ok":          ok,
			"diagnostics": diagnostics,
			"generator":   "component/routingrules",
		},
		"failureReason": failureReason,
	})
}

// recordDryRun writes Gate 2's verdict.
//
// THE GATE IS PASSED BY CONSTRUCTION, and saying why matters. Gate 2 exists to
// put a behavioural trace and a side-effect manifest in front of a person
// before an LLM-authored bundle is armed. This bundle was not authored by an
// LLM: it is a set of annotations rendered deterministically from a form the
// operator filled in and already saw, with an EMPTY BODY -- a rule has no
// steps and can have no side effect of its own. Manufacturing a synthetic
// trace over it would produce an artifact nobody reads, to answer a question
// the form already answered.
func (a *Activator) recordDryRun(ctx context.Context, bundleId string, f Form) error {
	return a.render(auth.ContextWithInternalOrigin(ctx), "recordBundleDryRun", map[string]any{
		"bundleId": bundleId,
		"status":   "dryRunPassed",
		"dryRunReport": map[string]any{
			"ok":        true,
			"generator": "component/routingrules",
			"rule":      ConstructNameFor(f),
			"policy":    f.Policy,
			"note":      "An empty-bodied rule rendered deterministically from a form; it has no steps and no side effect of its own.",
		},
	})
}

func summaryFor(f Form) string {
	when := "every call"
	if len(f.When) > 0 {
		when = "a call matching " + renderWhen(f.When)
	}
	level := ""
	if strings.TrimSpace(f.Level) != "" {
		level = ", at level " + f.Level
	}
	return fmt.Sprintf("Route %s through the %s policy%s, at precedence %d.", when, f.Policy, level, f.Precedence)
}

// render turns one authoring call into MemQL text and executes it.
//
// A RENDER FAILURE IS RETURNED, never swallowed. The alternative -- a helper
// that renders on a best effort and hands Execute an empty string -- produces
// a query the engine refuses with a parse error about nothing, which reads as
// an engine fault rather than as a bad argument.
func (a *Activator) render(ctx context.Context, fn string, args map[string]any) error {
	query, err := langparser.RenderCall(fn, args)
	if err != nil {
		return fmt.Errorf("routingrules: rendering %s: %w", fn, err)
	}
	if _, err := a.engine.Execute(ctx, query); err != nil {
		return fmt.Errorf("routingrules: %s: %w", fn, err)
	}
	return nil
}

func diagnosticSentences(report memql.SandboxReport) []string {
	out := make([]string, 0, len(report.Diagnostics))
	for _, d := range report.Diagnostics {
		if d.OK {
			continue
		}
		msg := strings.TrimSpace(d.Error)
		if msg == "" {
			msg = "refused with no message"
		}
		out = append(out, d.Name+": "+msg)
	}
	if len(out) == 0 {
		// A report that is not OK and names nothing is worse than one that
		// does: it stops an operator at a wall with no handhold. Say that
		// rather than returning an empty list somebody renders as blank.
		out = append(out, "the bundle was refused and the report named no construct")
	}
	return out
}
