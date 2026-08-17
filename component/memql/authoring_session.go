package memql

// authoring_session.go -- session-scoped authoring for the MCP `define` tool
// (MCP epic memql#1529, Phase 3 #1533; design mcp-server.md §3 Tier 2, §5).
//
// The durable authoring pipeline (authoring_activation_engine.go and friends)
// persists a v1:authoring:bundle to the graph and activates it under an owner's
// envelope -- a heavyweight, reviewable, owner-gated path. MCP Tier 2 wants the
// LIGHT counterpart: a developer submits a `.memql` bundle, it is validated and
// made callable BY NAME for the duration of their session, and nothing touches
// the shared/durable schema until a separate owner-gated promotion (§5).
//
// This file provides exactly that, reusing the existing machinery rather than
// reinventing it:
//
//   - SplitBundleSource slices a `.memql` bundle into (kind, name, source)
//     constructs using the same per-kind slicers the bootstrap loaders use.
//   - AuthorSessionBundle validates the bundle through the Gate-1 sandbox
//     (SandboxCompileBundle's compile core), compiles each function-family
//     construct into the engine's executable *Function, and registers it into a
//     caller-supplied, owner-scoped AuthoredRuntimeRegistry. Non-durable: the
//     registry lives for the session and is dropped when the session ends.
//   - ExecuteAuthored runs a query against a CORE-FIRST overlay of the engine's
//     function registry plus the session's authored functions, so a
//     session-authored construct is callable by name while NEVER shadowing a
//     core construct (the one-way precedence the authoring resolver guarantees).
//
// Owner-gated PROMOTION into the durable schema is the separate path
// (ActivateApprovedBundle); it is wired from the MCP server, not here.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// DefinedConstruct names one construct a session define registered.
type DefinedConstruct struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// SessionDefineResult reports the outcome of a session `define`: whether the
// bundle validated, the per-construct diagnostics (so a failing bundle explains
// itself), and the constructs that became callable on success.
type SessionDefineResult struct {
	OK          bool                `json:"ok"`
	Defined     []DefinedConstruct  `json:"defined,omitempty"`
	Diagnostics []SandboxDiagnostic `json:"diagnostics,omitempty"`
}

// SplitBundleSource slices a `.memql` bundle string into the (kind, name,
// source) constructs the sandbox + authored runtime operate on. It reuses the
// per-kind slicers the bootstrap loaders use, covering EVERY authorable kind so
// a deployment-style bundle (concept + query/mutation/logic + spec/trait/shape +
// automation + action + capability) is fully classified. Duplicate (kind, name)
// slices collapse to the first -- the sandbox separately hard-fails genuine
// in-bundle duplicates.
//
// FAIL-LOUD (epic #2354 E1 / #2351): any remaining top-level `<keyword> ... {`
// region the splitter cannot classify (prompt / provider / tool / builtin /
// policy / seed, or garbage) is emitted as an "unrecognized construct" that the
// sandbox hard-fails -- NEVER silently dropped.
func SplitBundleSource(source string) []SandboxConstruct {
	var out []SandboxConstruct
	seen := make(map[string]bool)
	// The shared file-top `use ...{ ... }` import preamble the function / action /
	// capability slicers prepend onto every slice they emit. Recomputed here so
	// bundleAnchorFor can strip it back off and locate each construct's BODY
	// verbatim in the bundle for authored-position mapping (#2375).
	usePreamble := extractUseDeclarations(source)
	add := func(kind, name, src string) {
		name = strings.TrimSpace(name)
		if name == "" || strings.TrimSpace(src) == "" {
			return
		}
		key := kind + "/" + name
		if seen[key] {
			return
		}
		seen[key] = true
		bundleLine, preambleLines := bundleAnchorFor(source, src, usePreamble)
		out = append(out, SandboxConstruct{
			Name:                name,
			Kind:                kind,
			Source:              src,
			BundleLine:          bundleLine,
			BundlePreambleLines: preambleLines,
		})
	}

	// Concepts: each concept block (preamble + body) is its own slice; its name
	// comes from parsing the slice.
	for _, slice := range conceptSlices(source) {
		if decls := ExtractConceptDecls(slice); len(decls) > 0 {
			add("concept", decls[0].Name, slice)
		}
	}
	// Function family: query / mutation / logic. The slice carries its own kind
	// (derived from the source keyword). ExtractFunctionSlices deliberately
	// excludes automations (the function-parse pipeline has no automation case),
	// so they are sliced separately below.
	for _, s := range ExtractFunctionSlices(source) {
		add(string(s.Kind), s.Name, s.Source)
	}
	// Struct-form / dedicated-parser kinds the function slicer does not cover:
	// shape, spec, trait (each via the generic keyword slicer).
	for _, kind := range []string{"shape", "spec", "trait"} {
		for _, s := range ExtractKeywordSlices(source, kind) {
			add(kind, s.Name, s.Source)
		}
	}
	// Automations (event-triggered orchestration) -- sliced by the dedicated
	// automation extractor. The Gate-1 sandbox compiles them through the
	// registered automations hook (authoring_sandbox_automation.go).
	for _, s := range ExtractAutomationSlices(source) {
		add(string(s.Kind), s.Name, s.Source)
	}
	// Actions (world-touching capability calls) + capabilities (the typed,
	// side-effect-classified vocabulary) -- the deployment-bundle kinds. Each
	// action slice inherits the file-top `use capabilities.*` imports so the
	// sandbox resolves its bare capability verb.
	for _, s := range extractActionBundleSlices(source) {
		add("action", s.Name, s.Source)
	}
	for _, s := range extractCapabilityBundleSlices(source) {
		add("capability", s.Name, s.Source)
	}
	// Fail-loud backstop: surface every unclassifiable top-level region as an
	// explicit "unrecognized construct" so nothing is silently dropped.
	for _, c := range detectUnrecognizedConstructs(source) {
		add(c.Kind, c.Name, c.Source)
	}
	return out
}

// ValidateBundle runs the Gate-1 sandbox over a `.memql` bundle: it slices the
// source into (kind, name, source) constructs and compiles + binds each in
// ISOLATION against a read-only clone of the live concept registry. It NEVER
// mutates engine state and registers nothing -- it is the read-only validation
// half of the authoring surface (the cockpit's ValidateBundle op, issue #2128).
// A bundle with no recognizable constructs reports OK=false with a single
// diagnostic explaining the empty parse, so the caller always gets a typed
// answer rather than a bare error.
// origin is the bundle's tree-relative path ("planner/queries.memql"), which
// supplies the AMBIENT DOMAIN for signature-concept resolution (memql#3800).
// Empty means an untitled buffer: it has no domain, and the documented
// dir=="" degrade is the right answer for it.
func ValidateBundle(bundleSource, origin string) SandboxReport {
	constructs := WithOrigin(SplitBundleSource(bundleSource), origin)
	if len(constructs) == 0 {
		return SandboxReport{
			OK: false,
			Diagnostics: []SandboxDiagnostic{{
				OK:    false,
				Error: "authoring: no recognizable constructs found in bundle source",
			}},
		}
	}
	return SandboxCompileBundle(constructs)
}

// AuthorSessionBundle validates a `.memql` bundle and registers its constructs
// into the caller-supplied, owner-scoped session registry, NON-DURABLY. It is
// the engine-free core of the MCP `define` op: split -> Gate-1 validate ->
// compile function-family constructs -> register. owner is the caller's userId;
// every construct is keyed to it so one session can never resolve another's.
//
// A bundle that fails validation registers nothing and returns the diagnostics
// (OK=false) plus a non-nil error. On success every recognized construct is
// registered ACTIVE and the function-family ones become callable by name within
// the session.
// origin carries the bundle's tree-relative path for ambient domain resolution
// (memql#3800); empty is an untitled buffer.
func AuthorSessionBundle(reg *AuthoredRuntimeRegistry, owner, bundleSource, origin string) (SessionDefineResult, error) {
	if reg == nil {
		return SessionDefineResult{}, fmt.Errorf("authoring: session define requires a runtime registry")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return SessionDefineResult{}, fmt.Errorf("authoring: session define requires an authenticated owner")
	}

	constructs := WithOrigin(SplitBundleSource(bundleSource), origin)
	if len(constructs) == 0 {
		return SessionDefineResult{}, fmt.Errorf("authoring: no recognizable constructs found in bundle source")
	}

	// Gate 1: isolated compile + bind against a clone of the live concept
	// registry. compileBundle returns the overlay concept registry so the
	// executable-compile step below binds against exactly the same concept set
	// (core + any bundle-defined concepts).
	report, concepts := compileBundle(constructs)
	res := SessionDefineResult{OK: report.OK, Diagnostics: report.Diagnostics}
	if !report.OK {
		return res, fmt.Errorf("authoring: bundle failed validation (%d of %d constructs did not compile)",
			sandboxFailureCount(report), len(report.Diagnostics))
	}

	for _, c := range constructs {
		var compiled any
		switch c.Kind {
		case "query", "mutation", "logic":
			fn, err := compileAuthoredFunction(c, concepts)
			if err != nil {
				return res, fmt.Errorf("authoring: compile %s %q: %w", c.Kind, c.Name, err)
			}
			compiled = fn
		case "spec", "trait":
			spec, err := compileAuthoredSpec(c)
			if err != nil {
				return res, fmt.Errorf("authoring: compile %s %q: %w", c.Kind, c.Name, err)
			}
			compiled = spec
		case "concept":
			// A candidate concept has ALWAYS compiled at Gate 1
			// (sandboxCompileConcept builds a real *Concept and merges it into
			// the ISOLATED clone) -- what it never did was carry that build
			// forward onto .Compiled, so the promote path had nothing to
			// register and refused the kind outright. It does now (memql#3746).
			//
			// Session scope is unchanged: the compiled concept is METADATA on
			// the session entry, not a registration. Nothing merges it into any
			// live registry until the owner-gated promote runs, so defining a
			// concept on a stream still validates it without touching the
			// shared cluster.
			concept, err := compileAuthoredConcept(c)
			if err != nil {
				return res, fmt.Errorf("authoring: compile %s %q: %w", c.Kind, c.Name, err)
			}
			compiled = concept
		default:
			// shape / automation / action / capability register as resolvable
			// session METADATA only (Compiled=nil). The function family is
			// executable-by-name and authored specs/traits are resolvable
			// inside authored query filters via the session spec overlay
			// (#1559).
			//
			// The world-touching kinds are deliberately INERT in session scope
			// (E1 #2372): a session-defined automation gets NO scheduler trigger
			// subscription, an action gets NO executor wiring, a capability gets
			// NO catalog registration -- so defining them on a stream validates
			// they compile + bind WITHOUT going live on the shared cluster. Live
			// activation of an authored automation (scheduler registration +
			// boot re-arm) is the separate owner-gated Gate-3 path
			// (ActivateApprovedBundle), never a stream-scoped define.
			compiled = nil
		}

		// Re-define within the session bumps the version (the registry rejects a
		// non-increasing version on replace).
		version := 1
		if existing, ok := reg.Lookup(owner, c.Kind, c.Name); ok {
			version = existing.Version + 1
		}
		if err := reg.Register(&AuthoredConstruct{
			OwnerUserId: owner,
			Kind:        c.Kind,
			Name:        c.Name,
			Source:      c.Source,
			Version:     version,
			Status:      AuthoredActive,
			Compiled:    compiled,
		}); err != nil {
			return res, fmt.Errorf("authoring: register %s %q: %w", c.Kind, c.Name, err)
		}
		res.Defined = append(res.Defined, DefinedConstruct{Kind: c.Kind, Name: c.Name})
	}
	return res, nil
}

// sandboxFailureCount counts the non-skipped compile failures in a report.
func sandboxFailureCount(report SandboxReport) int {
	n := 0
	for _, d := range report.Diagnostics {
		if !d.OK && !d.Skipped {
			n++
		}
	}
	return n
}

// compileAuthoredFunction compiles a function-family construct's source into the
// engine's executable *Function, binding against the bundle's concept overlay
// (core + bundle-defined concepts) -- the same per-construct parser the unified
// loader uses, so an authored function behaves identically to a core one.
func compileAuthoredFunction(c SandboxConstruct, concepts memoryNodes.Registry) (*Function, error) {
	slices := ExtractFunctionSlices(c.Source)
	if len(slices) == 0 {
		return nil, fmt.Errorf("no %s declaration found in source", c.Kind)
	}
	slice := slices[0]
	for _, s := range slices {
		if s.Name == c.Name {
			slice = s
			break
		}
	}
	return dispatchPerConstructParser(slice, "authored:"+c.Kind+":"+c.Name, concepts)
}

// compileAuthoredSpec compiles a spec/trait construct's source into the engine's
// executable *Spec, mirroring the sandbox's Gate-1 spec path (strip file-top
// imports, parse, convert). Stored as the construct's Compiled form so promotion
// can register it into the durable spec registry.
func compileAuthoredSpec(c SandboxConstruct) (*Spec, error) {
	decl, err := languageParser.ParseSpecDecl(stripUseDeclarations(c.Source))
	if err != nil {
		return nil, err
	}
	return specDeclToSpec(decl, "authored:"+c.Kind+":"+c.Name)
}

// buildAuthoredFunctionOverlay returns a function registry holding the core
// functions plus the owner's STAGED and session-authored functions, with the
// precedence CORE -> STAGED -> SESSION.
//
// Core-first is the one-way guarantee in authoring_resolver.go: an authored
// function whose name a core function already owns is dropped, so authored
// constructs can only ADD owner-private capability and can never shadow sealed
// engine behaviour. "Core" here is everything the shared registry holds, which
// includes anything durably promoted -- a trained construct is what the cluster
// runs, and neither tier below it may take its name.
//
// STAGED SITS BELOW CORE AND ABOVE SESSION (memql#3932), and that middle
// position is the whole ordering question. Below core for the reason above.
// Above session because session-define is the more specific, more immediate
// statement of intent: an author who defines a construct in this connection is
// working on it RIGHT NOW, and a durable staged version of the same name is the
// older draft they are editing. Reversing the two would make the staged copy
// un-overridable without a demote, which is exactly the friction the tier exists
// to remove.
func (e *MemQLEngine) buildAuthoredFunctionOverlay(owner string, staged, reg *AuthoredRuntimeRegistry) *FunctionRegistry {
	overlay := newFunctionRegistry()
	if e.functions != nil {
		for _, fn := range e.functions.Snapshot() {
			_ = overlay.Upsert(fn)
		}
	}
	if strings.TrimSpace(owner) == "" {
		return overlay
	}
	// Staged first, session second: a later Upsert replaces an earlier one, so
	// applying them in precedence order low-to-high is what makes session win.
	for _, layer := range []*AuthoredRuntimeRegistry{staged, reg} {
		if layer == nil {
			continue
		}
		for _, c := range layer.ListForOwner(owner) {
			if c.Status != AuthoredActive {
				continue
			}
			fn, ok := c.Compiled.(*Function)
			if !ok || fn == nil {
				continue
			}
			// Core-first: never shadow a sealed core construct.
			if e.functions != nil {
				if existing, err := e.functions.Get(fn.Name); err == nil && existing != nil {
					continue
				}
			}
			_ = overlay.Upsert(fn)
		}
	}
	return overlay
}

// buildAuthoredSpecOverlay returns a name->spec map holding the core specs plus
// the owner's STAGED and session-authored specs, at the same CORE -> STAGED ->
// SESSION precedence buildAuthoredFunctionOverlay applies to functions and for
// the same reasons (see there). A spec whose name a core spec already owns is
// dropped, so an authored spec can only ADD owner-private predicates and can
// NEVER shadow a sealed core spec.
//
// The resulting map threads through parseWithFunctions ->
// resolveAuthoredSpecOverlay so an authored spec referenced inside an authored
// query's filter resolves on the authored path only. A nil/empty result is fine
// -- a nil overlay short-circuits the authored expansion entirely.
func (e *MemQLEngine) buildAuthoredSpecOverlay(owner string, staged, reg *AuthoredRuntimeRegistry) map[string]*Spec {
	overlay := make(map[string]*Spec)
	if e.specs != nil {
		// LookupIndex (memql#3897): this overlay is KEYED INTO by the bare name
		// an author writes as a filter conjunct, so it must carry both the
		// namespaced key and the bare spelling. Snapshot alone would make every
		// bare conjunct in an authored construct read as an unknown spec.
		for name, spec := range e.specs.LookupIndex() {
			overlay[name] = spec
		}
	}
	if strings.TrimSpace(owner) == "" {
		return overlay
	}
	// Staged first, session second: the later write wins, so applying the
	// layers low-to-high is what puts session above staged.
	for _, layer := range []*AuthoredRuntimeRegistry{staged, reg} {
		if layer == nil {
			continue
		}
		for _, c := range layer.ListForOwner(owner) {
			if c.Status != AuthoredActive {
				continue
			}
			spec, ok := c.Compiled.(*Spec)
			if !ok || spec == nil {
				continue
			}
			// Core-first: never shadow a sealed core spec.
			if e.specs != nil {
				if _, exists := e.specs.Lookup(spec.Name); exists {
					continue
				}
			}
			overlay[spec.Name] = spec
		}
	}
	return overlay
}

// resolveAuthoredSpecOverlay fully expands every SpecReferenceExpression in expr
// against the supplied core-first overlay, inlining each spec's resolved body so
// the runtime evaluator (which only knows e.specs) never has to look a
// session-authored spec up. It recurses through the same expression-tree shapes
// expandSpecReferences covers, with cycle detection on the spec graph. Used only
// on the authored path (#1559); the public path passes a nil overlay and skips
// it.
func (e *MemQLEngine) resolveAuthoredSpecOverlay(expr ExpressionNode, overlay map[string]*Spec) (ExpressionNode, error) {
	return expandSpecReferencesWithOverlay(expr, overlay, make(map[string]struct{}))
}

// expandSpecReferencesWithOverlay is the recursive worker for
// resolveAuthoredSpecOverlay. resolving guards against circular spec references.
func expandSpecReferencesWithOverlay(expr ExpressionNode, overlay map[string]*Spec, resolving map[string]struct{}) (ExpressionNode, error) {
	if expr == nil {
		return nil, nil
	}
	switch node := expr.(type) {
	case *SpecReferenceExpression:
		name := strings.TrimSpace(node.Name)
		spec, ok := overlay[name]
		if !ok || spec == nil {
			return nil, fmt.Errorf("spec reference %q: spec %q not found", node.Name, node.Name)
		}
		if _, cycle := resolving[name]; cycle {
			return nil, fmt.Errorf("circular spec reference detected for %q", name)
		}
		resolving[name] = struct{}{}
		resolved, err := expandSpecReferencesWithOverlay(spec.Expr, overlay, resolving)
		delete(resolving, name)
		return resolved, err
	case *LogicalExpression:
		left, err := expandSpecReferencesWithOverlay(node.Left, overlay, resolving)
		if err != nil {
			return nil, err
		}
		right, err := expandSpecReferencesWithOverlay(node.Right, overlay, resolving)
		if err != nil {
			return nil, err
		}
		return &LogicalExpression{Op: node.Op, Left: left, Right: right}, nil
	case *RelationshipExpression:
		target, err := expandSpecReferencesWithOverlay(node.Target, overlay, resolving)
		if err != nil {
			return nil, err
		}
		return &RelationshipExpression{Function: node.Function, Target: target, Label: node.Label}, nil
	default:
		// Comparisons, builtins, literals, etc. carry no nested spec refs.
		return cloneExpressionNode(expr), nil
	}
}

// ExecuteAuthored runs a query with the owner's session-authored functions
// resolvable by name (core-first). It is the execution counterpart to
// AuthorSessionBundle: the MCP run_query / run_mutation path calls it so a
// just-defined construct is callable within the session. The run is stamped
// with the owner's authz envelope (writer) when ctx carries none, so authored
// constructs execute under the author's identity -- the per-row authz model
// then confines writes to what the author owns, exactly as for a core call.
//
// A session-authored spec referenced inside an authored query's filter resolves
// too (#1559), via the core-first owner-scoped spec overlay; like the function
// overlay it can never shadow a core spec.
//
// With no registry or owner it is identical to Execute, so callers can route
// every query through it unconditionally.
func (e *MemQLEngine) ExecuteAuthored(ctx context.Context, query, owner string, reg *AuthoredRuntimeRegistry) (*ExecuteResult, error) {
	// The STAGED registry makes this call non-trivial for an owner with no
	// session registry (memql#3932), which is why the short-circuit asks about
	// both. A caller holding no session registry -- an HTTP-side execute, an
	// automation firing under its author -- still has to resolve that author's
	// staged constructs, and returning plain Execute would report their own
	// durable construct as an unknown name.
	//
	// IT ASKS PER OWNER, not per registry, and that is a hot-path property
	// rather than a nicety. Both overlay builders copy EVERY core function and
	// EVERY core spec into a fresh registry before layering anything on top, so
	// a short-circuit keyed on "is a registry present" would pay that copy on
	// every query from every caller as soon as one person on the node staged one
	// construct -- or, on the gRPC stream, as soon as a session registry was
	// lazily created. HasOwner answers a boolean without allocating.
	if !authoredResolutionNeeded(owner, e.stagedAuthored, reg) {
		return e.Execute(ctx, query)
	}
	staged := e.stagedAuthored
	overlay := e.buildAuthoredFunctionOverlay(owner, staged, reg)
	specOverlay := e.buildAuthoredSpecOverlay(owner, staged, reg)
	ctx = ensureAuthorEnvelope(ctx, owner)
	return e.executeWith(ctx, query, overlay, specOverlay, false)
}

// ExecuteInline runs ad-hoc inline MemQL query text in the most-flexible mode
// (MCP Tier-3 #1535): it resolves the owner's session-authored constructs by
// name (core-first, like ExecuteAuthored) AND lifts the inline-shape
// pre-rejection (`name := expr`, trailing `@timestamp`/`@latest`). It is the
// engine entrypoint behind the MCP `query` tool; the server gates it to the
// inline tier + owner/developer BEFORE calling, so this never relaxes the parser
// for a non-inline caller. The run is stamped with the owner's writer envelope
// when ctx carries none.
func (e *MemQLEngine) ExecuteInline(ctx context.Context, query, owner string, reg *AuthoredRuntimeRegistry) (*ExecuteResult, error) {
	fns := e.functions
	var specOverlay map[string]*Spec
	if authoredResolutionNeeded(owner, e.stagedAuthored, reg) {
		staged := e.stagedAuthored
		fns = e.buildAuthoredFunctionOverlay(owner, staged, reg)
		specOverlay = e.buildAuthoredSpecOverlay(owner, staged, reg)
		ctx = ensureAuthorEnvelope(ctx, owner)
	}
	return e.executeWith(ctx, query, fns, specOverlay, true)
}

// authoredResolutionNeeded reports whether this owner has anything in either
// authored layer, and therefore whether an overlay is worth building.
//
// ONE PREDICATE FOR BOTH EXECUTE PATHS, so they cannot answer it differently --
// which they would, since ExecuteInline reached the same conclusion by a
// differently-shaped condition and the two drifted the moment a third layer
// arrived.
func authoredResolutionNeeded(owner string, staged, session *AuthoredRuntimeRegistry) bool {
	if strings.TrimSpace(owner) == "" {
		return false
	}
	return staged.HasOwner(owner) || session.HasOwner(owner)
}

// ensureAuthorEnvelope stamps the author's AccessContext onto ctx when none is
// present, so a session-authored construct runs under the author's identity. An
// already-attached envelope (the normal authenticated path) wins.
func ensureAuthorEnvelope(ctx context.Context, owner string) context.Context {
	if _, ok := auth.AccessFromContext(ctx); ok {
		return ctx
	}
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
}

// PromoteAuthoredConstruct registers a validated session-authored construct into
// the engine's DURABLE, SHARED registries -- the same registries the sealed core
// constructs live in -- so it becomes a first-class construct callable by every
// session for the lifetime of the process. This is the owner-gated promotion
// (design §5); the MCP server enforces the owner role + authoring tier before
// calling it.
//
// CORE-FIRST is preserved: a construct whose name a sealed core construct
// already owns is refused, so promotion can never redefine platform behaviour
// (the same one-way guarantee the session overlay enforces). Promotion is
// idempotent-friendly -- re-promoting replaces the prior promoted definition.
//
// NB: this makes the construct durable for the PROCESS. Writing a reviewable,
// DB-persisted v1:authoring:bundle through the Gate-3 ActivateApprovedBundle
// path (so a durable schema change is git/PR-reviewable across restarts) is the
// tracked follow-up; the owner gate + the call-by-name semantics are identical
// either way.
//
// A CONCEPT re-promote whose schema differs from the running version is
// classified (memql#3757) and a breaking change is REFUSED. This entry point
// takes the strict posture -- no override, and the classified diff discarded --
// which is the right default for every caller that does not say otherwise (the
// MCP promote tool reaches the engine through here). The two callers that need
// something else say so explicitly through promoteAuthoredConstructWithGate: the
// durable wire promote, which can carry an operator's override, and the two
// REPLAY paths, which must not re-decide a decision already taken.
func (e *MemQLEngine) PromoteAuthoredConstruct(ctx context.Context, c *AuthoredConstruct) error {
	return e.promoteAuthoredConstructWithGate(ctx, nil, c)
}

// promoteAuthoredConstructWithGate is PromoteAuthoredConstruct with the concept
// schema-change posture made explicit. A nil gate is the strict default:
// classify, refuse breaking, keep no diff.
func (e *MemQLEngine) promoteAuthoredConstructWithGate(ctx context.Context, gate *conceptPromoteGate, c *AuthoredConstruct) error {
	if c == nil {
		return fmt.Errorf("authoring: promote requires a construct")
	}
	switch c.Kind {
	case "query", "mutation", "logic":
		fn, ok := c.Compiled.(*Function)
		if !ok || fn == nil {
			return fmt.Errorf("authoring: promote %s %q: construct is not compiled", c.Kind, c.Name)
		}
		if e.functions == nil {
			return fmt.Errorf("authoring: promote %s %q: function registry is not initialized", c.Kind, c.Name)
		}
		key := "function:" + fn.Name
		if existing, err := e.functions.Get(fn.Name); err == nil && existing != nil {
			if _, promoted := e.promotedAuthored.Load(key); !promoted {
				return fmt.Errorf("authoring: promote %s %q: a core construct already owns that name (promotion cannot redefine core)", c.Kind, c.Name)
			}
		}
		if err := e.functions.Upsert(fn); err != nil {
			return err
		}
		e.promotedAuthored.Store(key, newPromotedMarker(c.Source))
		return nil
	case "spec", "trait":
		spec, ok := c.Compiled.(*Spec)
		if ok && spec == nil {
			// A typed-nil *Spec is compileAuthoredSpec's (nil, nil) output --
			// the #2607 intentional-skip contract for @disabled. The construct
			// compiled fine and was deliberately disabled; name the state, not
			// a compile failure (memql#2643).
			return fmt.Errorf("authoring: promote %s %q: construct is @disabled; enable it (remove @disabled from the source) before promoting", c.Kind, c.Name)
		}
		if !ok {
			return fmt.Errorf("authoring: promote %s %q: construct is not compiled", c.Kind, c.Name)
		}
		if e.specs == nil {
			return fmt.Errorf("authoring: promote %s %q: spec registry is not initialized", c.Kind, c.Name)
		}
		key := "spec:" + spec.Name
		if e.specs.IsDisabled(spec.Name) {
			if _, authored := e.promotedAuthored.Load(key); !authored {
				return fmt.Errorf("authoring: promote %s %q: a @disabled core construct owns that name (re-enable or rename; promotion cannot claim a retired core name)", c.Kind, c.Name)
			}
			// The reservation came from a stored @disabled AUTHORED row
			// (recompileAndPromoteRow): promoting the corrected (enabled)
			// source is the re-enable path -- lift it (memql#2643).
			e.specs.UnmarkDisabled(spec.Name)
		}
		if _, ok := e.specs.Lookup(spec.Name); ok {
			if _, promoted := e.promotedAuthored.Load(key); !promoted {
				return fmt.Errorf("authoring: promote %s %q: a core construct already owns that name (promotion cannot redefine core)", c.Kind, c.Name)
			}
		}
		if err := e.specs.Upsert(spec.Name, spec); err != nil {
			return err
		}
		e.promotedAuthored.Store(key, newPromotedMarker(c.Source))
		return nil
	case "concept":
		// The anchor of the training epic (memql#3746). Unlike the two branches
		// above, merging a concept is not just a registry upsert: the engine
		// DERIVES relationship + node-type state from the concept registry at
		// Init, and a concept merged after Init is absent from all of it. The
		// whole of that -- including the core-first never-shadow check, which
		// keys on the CANONICAL ID rather than the declaration name -- lives in
		// promoteConceptIntoLiveRegistry (authoring_promote_concept.go).
		built, ok := c.Compiled.(*memoryNodes.Concept)
		if !ok || built == nil {
			return fmt.Errorf("authoring: promote %s %q: construct is not compiled", c.Kind, c.Name)
		}
		if err := e.promoteConceptIntoLiveRegistry(ctx, gate, c, built); err != nil {
			return err
		}
		// A PROMOTE IS THE UN-RETIRE PATH (memql#3756). A concept demoted while
		// rows existed under it stays registered and closed to writes; promoting
		// it again is the operation that re-opens it, and it is the only one --
		// there is no separate un-retire API to forget to call. A no-op for the
		// ordinary promote of a concept that was never retired.
		//
		// Every promote route funnels here (session promote, durable promote,
		// boot + cross-node re-hydration), which is what makes that claim true
		// rather than true-of-one-path. The boot walk clearing a retirement it
		// is about to replay is harmless and deliberate: the replay
		// (applyPersistedConceptRetirements) runs AFTER the whole walk, over the
		// persisted rows, so it decides the final state.
		e.clearConceptRetirement(built.Name)
		return nil
	default:
		return fmt.Errorf("authoring: promotion of %s constructs is not supported (concept + function-family + spec only)", c.Kind)
	}
}

// DemoteAuthoredConstruct is the inverse of PromoteAuthoredConstruct: it removes
// a previously AUTHOR-PROMOTED construct from the engine's DURABLE, SHARED
// registries so it is no longer callable by any session for the lifetime of the
// process (memql#2163). It is the in-process half of the durable DEMOTE; the
// MCP / gRPC server enforces the owner role before calling it.
//
// SAFETY-CRITICAL: it removes ONLY a construct that was promoted via the
// authored path. It checks e.promotedAuthored for the (kind:name) key; if the
// name is absent it returns an error and removes NOTHING -- so a sealed core
// construct (which a promotion can never shadow, and so never carries a
// promotedAuthored marker) can NEVER be unregistered by a demote. This mirrors
// the core-first never-shadow invariant PromoteAuthoredConstruct enforces, in
// reverse.
//
// On success it removes the construct from the matching shared registry
// (functions for query/mutation/logic, specs for spec/trait) and clears the
// promotedAuthored marker, so a later re-promote of the same name registers
// fresh. Idempotent-unfriendly by design: demoting a name that was never
// author-promoted is an error, not a no-op, so the caller learns it asked to
// remove something it does not own.
//
// A CONCEPT does not necessarily leave the registry (memql#3756): rows written
// under it outlive its definition, so a demote with rows under the concept
// RETIRES it -- registered, readable, closed to new writes -- and only a demote
// with zero rows removes it. Callers that need to know which happened use
// demoteAuthoredConstructWithOutcome; this signature keeps the error-only shape
// for the paths that only need the safety gate (the cross-node demote
// subscriber).
func (e *MemQLEngine) DemoteAuthoredConstruct(ctx context.Context, kind, name string) error {
	_, err := e.demoteAuthoredConstructWithOutcome(ctx, kind, name)
	return err
}

// demoteAuthoredConstructWithOutcome is DemoteAuthoredConstruct plus the
// structured record of WHICH withdrawal happened -- retired or removed, and the
// row count that chose between them for a concept. The durable demote reports it
// on the wire so a client renders the outcome instead of inferring it from the
// absence of an error (memql#3756).
func (e *MemQLEngine) demoteAuthoredConstructWithOutcome(ctx context.Context, kind, name string) (DemoteOutcome, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return DemoteOutcome{}, fmt.Errorf("authoring: demote requires a construct name")
	}
	switch kind {
	case "query", "mutation", "logic":
		if e.functions == nil {
			return DemoteOutcome{}, fmt.Errorf("authoring: demote %s %q: function registry is not initialized", kind, name)
		}
		key := "function:" + name
		if _, promoted := e.promotedAuthored.Load(key); !promoted {
			return DemoteOutcome{}, fmt.Errorf("authoring: demote %s %q: not an author-promoted construct (demotion cannot remove a core construct)", kind, name)
		}
		e.functions.Remove(name)
		e.promotedAuthored.Delete(key)
		return DemoteOutcome{Kind: kind, Name: name, Outcome: DemoteOutcomeRemoved}, nil
	case "spec", "trait":
		if e.specs == nil {
			return DemoteOutcome{}, fmt.Errorf("authoring: demote %s %q: spec registry is not initialized", kind, name)
		}
		key := "spec:" + name
		if _, promoted := e.promotedAuthored.Load(key); !promoted {
			return DemoteOutcome{}, fmt.Errorf("authoring: demote %s %q: not an author-promoted construct (demotion cannot remove a core construct)", kind, name)
		}
		e.specs.Remove(name)
		// The marker also covers a name reserved by a stored @disabled
		// authored row (never registered): demote is its retire path, so
		// release the reservation too (memql#2643).
		e.specs.UnmarkDisabled(name)
		e.promotedAuthored.Delete(key)
		return DemoteOutcome{Kind: kind, Name: name, Outcome: DemoteOutcomeRemoved}, nil
	case "concept":
		// The kind that is not an unregister. A concept's rows outlive its
		// definition, so the withdrawal is retire-vs-remove and the row count
		// decides -- authoring_concept_retire.go carries the decision, the
		// count's actor, and why each of the two outcomes exists.
		return e.demoteConceptFromLiveRegistry(ctx, name)
	default:
		return DemoteOutcome{}, fmt.Errorf("authoring: demotion of %s constructs is not supported (concept + function-family + spec only)", kind)
	}
}
