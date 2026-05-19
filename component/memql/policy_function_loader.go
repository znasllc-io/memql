package memql

import (
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// PolicyFunctionRegistry holds the parsed `func (Policy)` definitions
// from dsl/v1/policies/{core,bff}/... It is distinct from the
// PolicyRegistry that holds the older SI Router routing policies
// (which use the policy NAME { @primary(...) ... } block syntax under
// dsl/v1/policies/v1/). The two coexist intentionally — they have
// different purposes and different syntax.
//
// Lookup returns a parsed FunctionDef carrying the tier, args,
// annotations, and body AST. Phase 3 wires evaluation; Phase 6 ships
// tier enforcement and the cross-tier delegation check.
type PolicyFunctionRegistry struct {
	mu     sync.RWMutex
	byName map[string]*PolicyFunction
}

// PolicyFunction is the registered representation of a `func (Policy)`
// definition. Holds the parsed AST plus the metadata pulled out of
// annotations (tier, frontend_visible, audited, etc.).
type PolicyFunction struct {
	Name             string
	Tier             string // "core" or "bff"
	Origin           string // path under the policies FS
	Description      string
	FrontendVisible  bool
	Cacheable        bool
	Audited          bool
	TracesPersisted  bool
	ReturnsTrace     bool
	Enabled          bool
	Attributes       []*languageParser.Attribute
	FunctionDef      *languageParser.FunctionDef
}

func newPolicyFunctionRegistry() *PolicyFunctionRegistry {
	return &PolicyFunctionRegistry{
		byName: make(map[string]*PolicyFunction),
	}
}

// Lookup returns the parsed policy for the given name, or (nil, false)
// if no such policy is registered.
func (r *PolicyFunctionRegistry) Lookup(name string) (*PolicyFunction, bool) {
	if r == nil {
		return nil, false
	}
	key := strings.TrimSpace(name)
	if key == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byName[key]
	return p, ok
}

// All returns a snapshot copy of every registered policy function.
func (r *PolicyFunctionRegistry) All() []*PolicyFunction {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*PolicyFunction, 0, len(r.byName))
	for _, p := range r.byName {
		out = append(out, p)
	}
	return out
}

// Count returns the number of registered policy functions.
func (r *PolicyFunctionRegistry) Count() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byName)
}

// loadPolicyFunctions walks dsl/v1/policies/core/... and
// dsl/v1/policies/bff/..., parsing each `func (Policy)` definition
// and registering the result. Files that fail to parse are logged
// and skipped — one bad file should not blank-out the whole tree.
func loadPolicyFunctions(logger *slog.Logger) (*PolicyFunctionRegistry, error) {
	// Pass 3 of the DSL restructure: the legacy walk over
	// dsl/v1/policies/{core,bff}/ is retired. Policies live at
	// dsl/policies/{core,bff,routing}.memql in the new tree; a
	// unified loader will pick them up in a follow-up. This stub
	// keeps the bootstrap call site compiling.
	_ = logger
	return newPolicyFunctionRegistry(), nil
}

// crossTierViolation records a single `core policy calls bff policy`
// edge for error reporting.
type crossTierViolation struct {
	target     string
	targetTier string
}

// findCrossTierCallViolations walks the parsed body of a policy and
// returns every `policy("name", ...)` call whose target sits at a
// higher tier (i.e. a core policy calling a bff policy). Downward
// (bff -> core) calls are allowed; same-tier calls are allowed.
func findCrossTierCallViolations(policy *PolicyFunction, registry *PolicyFunctionRegistry) []crossTierViolation {
	if policy == nil || policy.FunctionDef == nil || registry == nil {
		return nil
	}
	if policy.Tier != "core" {
		// Only core->bff is forbidden; bff calling anywhere is fine.
		return nil
	}
	var out []crossTierViolation
	walkPolicyBodyCalls(policy.FunctionDef.Body, func(targetName string) {
		target, ok := registry.Lookup(targetName)
		if !ok {
			return
		}
		if target.Tier == "bff" {
			out = append(out, crossTierViolation{target: target.Name, targetTier: target.Tier})
		}
	})
	return out
}

// walkPolicyBodyCalls invokes onCall for every `policy("<name>", ...)`
// invocation reachable from the given AST node. The walker mirrors
// the shape of the language parser's expression types — extend the
// switch as new control-flow constructs land in policy bodies.
func walkPolicyBodyCalls(node any, onCall func(string)) {
	if node == nil || onCall == nil {
		return
	}
	switch n := node.(type) {
	case *languageParser.FunctionCallExpr:
		if strings.EqualFold(strings.TrimSpace(n.Name), "policy") {
			if raw, ok := n.Args["0"]; ok {
				if s, ok := raw.(string); ok {
					onCall(s)
				}
			}
		}
		for _, arg := range n.Args {
			walkPolicyBodyCalls(arg, onCall)
		}
	case *languageParser.LogicalExpr:
		walkPolicyBodyCalls(n.Left, onCall)
		walkPolicyBodyCalls(n.Right, onCall)
	case *languageParser.ComparisonExpr:
		walkPolicyBodyCalls(n.Value, onCall)
	}
}

// walkPolicyTier deleted in Pass 3 (DSL restructure). Policies now
// live at dsl/policies/{core,bff,routing}.memql; a unified loader
// will consume them in a follow-up. The cross-tier validation logic
// in findCrossTierCallViolations + walkPolicyBodyCalls is kept for
// the unified loader to reuse.

func reportPolicyFunctionError(logger *slog.Logger, origin string, err error) {
	if logger == nil || err == nil {
		return
	}
	logger.Error("policy function skipped",
		"component", ComponentName,
		"file", origin,
		"error", err.Error(),
	)
}

// parsePolicyFunctionFile parses a single `.memql` file expected to
// contain a `func (Policy)` definition. The caller passes the
// tier (derived from the directory placement) so we can validate that
// any @tier annotation matches.
func parsePolicyFunctionFile(origin, dirTier string, raw []byte) (*PolicyFunction, error) {
	source := string(raw)
	lexer := languageParser.NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil, fmt.Errorf("lexer: %w", err)
	}
	p := languageParser.NewParser(tokens)
	ast, err := p.Parse()
	if err != nil {
		return nil, fmt.Errorf("parser: %w", err)
	}
	file, ok := ast.(*languageParser.File)
	if !ok {
		return nil, fmt.Errorf("expected File AST, got %T", ast)
	}
	for _, def := range file.Definitions {
		fd, ok := def.(*languageParser.FunctionDef)
		if !ok {
			continue
		}
		if fd.Receiver == nil || fd.Receiver.Type != languageParser.ReceiverPolicy {
			continue
		}
		return policyFunctionFromDef(origin, dirTier, fd)
	}
	return nil, fmt.Errorf("policy file must contain a func (Policy) definition")
}

// policyFunctionFromDef extracts the registered PolicyFunction record
// from a parsed FunctionDef. Validates that the file location matches
// the @tier annotation when both are present.
func policyFunctionFromDef(origin, dirTier string, fd *languageParser.FunctionDef) (*PolicyFunction, error) {
	out := &PolicyFunction{
		Name:        fd.Name,
		Origin:      origin,
		Description: fd.Description,
		FunctionDef: fd,
		Attributes:  fd.Attributes,
		Enabled:     fd.Enabled,
	}
	declared := annotationTier(fd.Attributes)
	if declared != "" {
		out.Tier = declared
	} else {
		out.Tier = dirTier
	}
	// Cross-check tier vs directory when both are known. The new
	// per-construct tree consolidates every policy into
	// dsl/policies/policies.memql, so dirTier is empty and the
	// annotation is authoritative; the check still fires for callers
	// (legacy tests) that pass a directory-derived tier.
	if declared != "" && dirTier != "" && declared != dirTier {
		return nil, fmt.Errorf("@tier(%q) does not match directory placement %q for policy %q", declared, dirTier, fd.Name)
	}
	if out.Tier == "" {
		return nil, fmt.Errorf("policy %q has no tier: add @tier(\"core\") or @tier(\"bff\")", fd.Name)
	}

	for _, attr := range fd.Attributes {
		if attr == nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(attr.Name)) {
		case "frontend_visible":
			out.FrontendVisible = true
		case "cacheable":
			out.Cacheable = true
		case "audited":
			out.Audited = true
		case "traces_persisted":
			out.TracesPersisted = true
		case "returns_trace":
			out.ReturnsTrace = true
		}
	}

	if out.FrontendVisible && out.Tier != "bff" {
		return nil, fmt.Errorf("@frontend_visible is only valid on bff-tier policies; policy %q is %s", fd.Name, out.Tier)
	}

	// Filename should match policy name (mirrors the other DSL
	// constructs' filename-no-prefix / function-name-with-prefix
	// convention -- policies use no prefix so filename == function
	// name exactly).
	base := strings.TrimSuffix(path.Base(origin), ".memql")
	if !strings.EqualFold(base, fd.Name) {
		return nil, fmt.Errorf("policy filename %q must match function name %q", base, fd.Name)
	}

	// Phase C check -- reject policies whose body is spec-shaped.
	//
	// A spec-shaped body:
	//   - is a pure boolean expression (no policy() / spec() /
	//     other function calls)
	//   - references only caller.* (would compile as a context-spec)
	//   - carries no policy-only annotations (@audited /
	//     @frontend_visible / @traces_persisted / @cacheable /
	//     @returns_trace)
	//   - the policy itself doesn't take args the body uses
	//     (no ctx.input / ctx.* references in the body)
	//
	// When all four conditions hold the body is structurally a
	// context-spec wearing a policy hat -- redundant, since specs
	// are the atomic predicate primitive. Refactor to a spec.
	if reason := isPolicySpecShaped(out); reason != "" {
		return nil, fmt.Errorf("policy %q has a spec-shaped body (%s) -- define as a spec under dsl/v1/specs/v1/<namespace>/ and compose via `spec(%q)` from policy bodies", fd.Name, reason, fd.Name)
	}

	return out, nil
}

// isPolicySpecShaped returns a one-line description of why the
// policy's body would compile cleanly as a spec, or "" when the
// policy is genuinely policy-shaped (calls sub-policies / specs,
// reads caller args, carries policy-only annotations, returns
// non-bool, etc.). See the call site for the four conditions.
func isPolicySpecShaped(p *PolicyFunction) string {
	if p == nil || p.FunctionDef == nil {
		return ""
	}
	// Policy-only annotations imply the author knows it's a policy.
	if p.FrontendVisible || p.Audited || p.TracesPersisted || p.Cacheable || p.ReturnsTrace {
		return ""
	}
	// Non-bool return types are policies (decision functions returning
	// strings / objects / etc).
	if len(p.FunctionDef.Returns) != 1 || !strings.EqualFold(strings.TrimSpace(p.FunctionDef.Returns[0]), "bool") {
		return ""
	}
	// Walk the body. Track: does it have function calls? Does it
	// reference ctx.* / args? Does it have any caller.* refs?
	body := p.FunctionDef.Body
	if body == nil {
		return ""
	}
	hasFnCall := false
	hasCtxOrArgs := false
	hasCallerRef := false
	walkPolicyBodyRefs(body, &hasFnCall, &hasCtxOrArgs, &hasCallerRef)
	if hasFnCall {
		// Composes another policy / spec / DSL function — genuinely
		// policy work.
		return ""
	}
	if hasCtxOrArgs {
		// Reads caller-passed args — specs don't take args, so this
		// has to be a policy.
		return ""
	}
	if hasCallerRef {
		return "body reads only caller.* and contains no policy()/spec() calls"
	}
	return ""
}

// walkPolicyBodyRefs scans a policy body for function calls,
// ctx-arg references, and caller references. Sets the flags as it
// finds each. Mirrors the shape of the language-parser AST.
func walkPolicyBodyRefs(node any, hasFnCall, hasCtxOrArgs, hasCallerRef *bool) {
	if node == nil {
		return
	}
	switch n := node.(type) {
	case *languageParser.FunctionCallExpr:
		*hasFnCall = true
		for _, arg := range n.Args {
			walkPolicyBodyRefs(arg, hasFnCall, hasCtxOrArgs, hasCallerRef)
		}
	case *languageParser.LogicalExpr:
		walkPolicyBodyRefs(n.Left, hasFnCall, hasCtxOrArgs, hasCallerRef)
		walkPolicyBodyRefs(n.Right, hasFnCall, hasCtxOrArgs, hasCallerRef)
	case *languageParser.ComparisonExpr:
		walkPolicyBodyFieldRef(n.Field, hasCtxOrArgs, hasCallerRef)
		walkPolicyBodyRefs(n.Value, hasFnCall, hasCtxOrArgs, hasCallerRef)
	case *languageParser.ArgRefExpr:
		*hasCtxOrArgs = true
	}
}

func walkPolicyBodyFieldRef(ref languageParser.FieldReference, hasCtxOrArgs, hasCallerRef *bool) {
	if len(ref.Parts) == 0 {
		return
	}
	first := strings.ToLower(strings.TrimSpace(ref.Parts[0]))
	switch first {
	case "ctx", "args", "payload":
		*hasCtxOrArgs = true
	case "caller":
		*hasCallerRef = true
	}
}

// annotationTier returns the @tier("core"|"bff") value if present,
// otherwise "". Accepts both `@tier("core")` (single Value) and
// `@tier(tier="bff")` (named Args).
func annotationTier(attrs []*languageParser.Attribute) string {
	for _, attr := range attrs {
		if attr == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(attr.Name), "tier") {
			continue
		}
		if s, ok := attr.Value.(string); ok && strings.TrimSpace(s) != "" {
			return strings.ToLower(strings.TrimSpace(s))
		}
		if v, ok := attr.Args["tier"]; ok {
			if s, ok := v.(string); ok {
				return strings.ToLower(strings.TrimSpace(s))
			}
		}
	}
	return ""
}
