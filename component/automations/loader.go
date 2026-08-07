package automations

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/compiler"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

var inlineStepBlockPattern = regexp.MustCompile(`:=\s*(query|mutation|shape|webhook|event|publishEvent)\s*(if\s+[^{]+)?\{`)
var inlineOperationCallPattern = regexp.MustCompile(`:=\s*(?:if\s+[^{]+\{\s*)?(query|mutation)\s*\(`)

// Loader loads automation definitions from the unified DSL tree.
type Loader struct {
	logger   *slog.Logger
	registry memoryNodes.Registry
}

// LoaderOptions configures the automation loader.
type LoaderOptions struct {
	Logger   *slog.Logger
	Registry memoryNodes.Registry
}

// NewLoader creates a new automation loader using embedded files.
// If opts.Logger is nil, a new logger is created using core.NewLogger with the component's
// configured color and level from environment variables.
func NewLoader(opts LoaderOptions) *Loader {
	logger := opts.Logger
	if logger == nil {
		logger = NewLogger()
	}

	return &Loader{
		logger:   logger,
		registry: opts.Registry,
	}
}

// LoadAll loads all automation definitions from the new
// domain-first DSL tree at dsl.Tree(). Each
// `<domain>/automations.memql` file contributes a list of
// automations (extracted via slice-based parsing).
//
// This used to run two further fs.WalkDir passes over an `l.fsys` field, under
// a comment saying they were "retained for tests that inject a custom FS via
// the fsys field". No such test existed -- verified across all history
// (`git log --all -S ".fsys ="`) -- and LoaderOptions exposed no seam to
// inject through, so it was never an intentional injection point and both
// passes always walked the empty `fstest.MapFS{}` NewLoader hardcoded.
//
// (Precisely: an IN-PACKAGE test could have assigned `loader.fsys` directly,
// since every test here is `package automations`. So the seam was reachable in
// principle; it was simply never used. An earlier draft of this comment said
// "and none could", which was wrong.)
//
// Removed in memql#2858, along with the `.json` automation loader they reached,
// which had no live source. If a custom-FS seam is ever actually wanted, add an
// FS field to LoaderOptions and a test that uses it -- that is a feature
// decision, not cleanup.
func (l *Loader) LoadAll() ([]*Automation, error) {
	automations, err := l.LoadFromUnifiedTree()
	if err != nil {
		return nil, err
	}

	if l.logger != nil {
		l.logger.Info("automations loaded",
			"count", len(automations),
		)
	}

	return automations, nil
}

// CompileSource compiles a SINGLE automation from an in-memory .memql
// source string, against the loader's registry, WITHOUT touching any
// filesystem or live engine state. It runs the exact same parse ->
// concept-resolution -> trigger-normalization -> compile -> validate
// pipeline the bootstrap loader uses for on-disk automation files, so a
// caller (e.g. the authoring sandbox's Gate 1) gets identical parse/bind
// diagnostics. `origin` is a synthetic path used only for error context
// and version derivation; it never has to exist on disk.
//
// The registry is consulted read-only for `use`-import concept
// resolution; nothing is registered into it. Pass an isolated registry
// clone to bind candidate constructs in isolation.
func (l *Loader) CompileSource(source, origin string) (*Automation, error) {
	return l.compileMemQL(source, origin)
}

// LoadByName loads a runnable automation by its canonical name.
//
// Canonical-name rule (memql#1663). run_automation runs AUTOMATIONS -- the
// `automation <name>` construct is the runnable entry point (it carries the
// trigger + the step chain). Resolution is deterministic and tries, in order:
//
//  1. Exact automation name (e.g. registerNode / deregisterNode /
//     bootstrapCluster / pruneStaleClusterNodes / expireDelegations). This is
//     the canonical, documented name and the only previously-supported form.
//  2. A wrapped LOGIC construct's name, in either spelling -- the full
//     `logicXxx` construct name OR its bare invocation form `xxx` (strip the
//     `logic` prefix, lowercase the first letter) -- resolving to the unique
//     automation whose step invokes that logic. This makes a logic construct
//     reachable by the name an author naturally reaches for (the QA pain in
//     #1663: `revokeExpiredDelegations` / `revokeExpiredDelegations` both
//     now resolve to the `expireDelegations` automation that wraps them).
//
// If no automation matches but the name corresponds to a logic construct that
// no automation wraps, a clear "not a runnable entry point" error is returned
// (it is internal-only) -- which beats the old misleading "not found".
//
// Both the live and the dry-run run_automation paths resolve through this one
// method, so they cannot diverge.
func (l *Loader) LoadByName(name string) (*Automation, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("automation name is required")
	}

	automations, err := l.LoadAll()
	if err != nil {
		return nil, err
	}

	// 1. Exact automation name -- the canonical entry point.
	for _, a := range automations {
		if a.Name == name {
			return a, nil
		}
	}

	// 2. Logic-alias resolution. A wrapped logic construct resolves to the
	//    automation whose step invokes it. Post-C6 (memql#2036) both the
	//    construct declaration and its invocation are bare, so the match is
	//    a direct name comparison.
	for _, a := range automations {
		for _, invoked := range invokedLogicNames(a) {
			if invoked == name {
				return a, nil
			}
		}
	}

	// 3. Distinguish a logic construct that exists but is internal-only (no
	//    wrapping automation) from a name that matches nothing at all.
	if _, ok := memql.DSLConstructSource(l.logger, "logic", name); ok {
		return nil, fmt.Errorf(
			"%q is a logic construct but no automation wraps it, so it is not a runnable entry point via run_automation", name)
	}

	return nil, fmt.Errorf("automation %q not found", name)
}

// invokedLogicNames returns the bare construct names invoked by an
// automation's function-call steps (walking nested parallel / forEach / switch
// containers). Post-C6 (memql#2036) a `logic <name> { ... }` step is rewritten
// by the parser into a function-call step whose Function.Name is the bare logic
// name, so these are the StepTypeFunction steps. (Sub-automation dispatch is
// StepTypeAutomation and is therefore excluded.)
func invokedLogicNames(a *Automation) []string {
	if a == nil {
		return nil
	}
	var out []string
	var walk func(steps []*Step)
	visit := func(s *Step) {
		if s == nil {
			return
		}
		if s.Type == StepTypeFunction && s.Function != nil {
			out = append(out, s.Function.Name)
		}
		if s.Parallel != nil {
			walk(s.Parallel.Branches)
		}
		if s.ForEach != nil {
			walk(s.ForEach.Do)
		}
		if s.Switch != nil {
			for _, c := range s.Switch.Cases {
				if c != nil {
					walk(c.Steps)
					walk([]*Step{c.Step})
				}
			}
			if s.Switch.Default != nil {
				walk(s.Switch.Default.Steps)
				walk([]*Step{s.Switch.Default.Step})
			}
		}
	}
	walk = func(steps []*Step) {
		for _, s := range steps {
			visit(s)
		}
	}
	walk(a.Steps)
	return out
}

// The logic name prefix bridge (isLogicConstructName / fullLogicName /
// bareLogicName) was removed by C6 (memql#2036): construct names are now bare,
// so logic resolution is a direct name comparison (see LoadByName step 2).

// compileMemQL compiles MemQL source to an Automation struct.
//
// Enforces CQS file composition rules for automations:
//   - Exactly 1 automation per file (required)
//   - Can have supporting queries (helpers)
//   - Cannot have mutations (mutations go in functions/ directory)
func (l *Loader) compileMemQL(source, path string) (*Automation, error) {
	// Close the annotation silent-tolerance gap (#2712): automations reach
	// this dedicated loader instead of the function slicer's gate, so an
	// unknown / dead / retired annotation would otherwise be silently
	// dropped. Run the same allow-list + retired-name gate every function
	// kind uses. The source is already terse-lowered here (unified_loader),
	// and the helper re-lowers idempotently for any direct caller.
	if err := memql.ValidateAutomationAnnotations(source); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	// The three raw-text gates below scan a COMMENT-BLANKED view, not the raw
	// slice (memql#2872).
	//
	// They are substring/regex scans, so a comment merely MENTIONING the thing
	// they forbid tripped them and refused the whole boot: a note reading
	// `/* the old form used $steps.foo */` or a commented-out
	// `mutation(concept: ...)` example is valid, memqllint-clean input that an
	// author writes constantly. That is why the two earlier attempts at
	// #2872's preamble fix had to be reverted -- pulling a comment into the
	// slice made these fire. Blanking is what makes the preamble fix possible.
	//
	// The gates keep firing on REAL code: BlankComments only blanks comment
	// spans, and it is byte-length- and newline-preserving, so a live
	// `$steps.` is untouched.
	//
	// Note the ORIGINAL source is what gets parsed below -- `///` doc comments
	// are semantic (they become @description), so the blanked copy is used for
	// GATING only, never for compilation.
	gateScan := languageParser.BlankComments(source)

	// Enforce .memql automation syntax: do not allow JSON-style $steps references.
	// In .memql, step references should be bare (e.g., "getAgent.result.Bundle.nodes")
	// and are resolved by the evaluator at runtime.
	if strings.Contains(gateScan, "$steps.") {
		return nil, fmt.Errorf("invalid .memql syntax: '$steps.' is not allowed (use bare step references like 'stepId.result.X')")
	}
	if inlineStepBlockPattern.MatchString(gateScan) {
		return nil, fmt.Errorf("inline step blocks are no longer supported in .memql automations; use kind-prefixed named-args call syntax such as query name(k: v), mutation name(k: v), builtin publishEvent(k: v), or webhook name(k: v)")
	}

	// Enforce architectural layering: reject direct query() and mutation() calls
	if inlineOperationCallPattern.MatchString(gateScan) {
		return nil, fmt.Errorf("direct query() and mutation() calls are not allowed in automations\n\n" +
			"Use named query/mutation functions instead:\n" +
			"  - For queries: see queries/v1/ directory\n" +
			"  - For mutations: see mutations/v1/ directory\n\n" +
			"Examples:\n" +
			"  Instead of: query({ query: \"concept==v1:agents:agent; ...\" })\n" +
			"  Use: query activeAgents(tier: \"pro\")\n\n" +
			"  Instead of: mutation({ concept: \"v1:cognition:space\", ... })\n" +
			"  Use: mutation mutationCreateSpace(name: \"My Space\")")
	}

	// Extract + strip first-class precondition blocks (Epic 4 / memql#2139)
	// BEFORE the struct-form rewriter runs -- the rewriter only understands
	// `step` blocks, so a `precondition NAME { ... }` left in the body would
	// fail to parse. We re-attach the parsed preconditions to the compiled
	// Automation below.
	preconditions, source, err := extractPreconditions(source)
	if err != nil {
		return nil, fmt.Errorf("parsing automation preconditions: %w", err)
	}

	// Parse, resolve concept references, then compile
	result, err := l.parseResolveCompile(source, path)
	if err != nil {
		return nil, fmt.Errorf("compiling .memql: %w", err)
	}

	if len(result.Automations) == 0 {
		return nil, fmt.Errorf("no automation definition found in %s", path)
	}

	if len(result.Automations) > 1 {
		return nil, fmt.Errorf("multiple automation definitions in %s (only one allowed per file)", path)
	}

	// Verify no standalone mutations (they should be in functions/ directory)
	for _, fn := range result.Functions {
		if fn.Type == "mutation" {
			return nil, fmt.Errorf("mutation %q should be in functions/ directory, not automations/", fn.Name)
		}
	}

	// Convert the compiled JSON map to Automation struct
	automationOutput := result.Automations[0]

	// Marshal to JSON then unmarshal to Automation struct
	jsonData, err := json.Marshal(automationOutput.JSON)
	if err != nil {
		return nil, fmt.Errorf("marshaling compiled automation: %w", err)
	}

	var automation Automation
	if err := json.Unmarshal(jsonData, &automation); err != nil {
		return nil, fmt.Errorf("unmarshaling to Automation: %w", err)
	}

	// Ensure name is set
	if automation.Name == "" {
		automation.Name = automationOutput.Name
	}

	// Re-attach the first-class preconditions extracted before the rewrite
	// (Epic 4 / memql#2139). The executor evaluates them deterministically
	// at the start of the run; a miss emits the healing.precondition.missed
	// repair-trigger signal and aborts before any step fires.
	if len(preconditions) > 0 {
		if err := validatePreconditions(preconditions); err != nil {
			return nil, fmt.Errorf("invalid preconditions: %w", err)
		}
		automation.Preconditions = preconditions
	}

	// Validate steps
	if err := l.validateSteps(automation.Steps); err != nil {
		return nil, fmt.Errorf("invalid steps: %w", err)
	}

	// G2 (memql#2364, ADR Decision 3): for args-block automations, reject
	// shadowing and unresolvable bare identifiers at compile time -- both the
	// tree loader and the authoring-sandbox hook flow through here, so
	// authored bundles get the same gate. Args-less automations are exempt.
	if err := validateArgsResolution(&automation); err != nil {
		return nil, err
	}

	// G5 (memql#2367, ADR Decision 6): `event.payload.X` reads are RETIRED
	// in automation bodies -- the payload binds to the args { } contract and
	// is read bare (or as args.X). Rejected at the SOURCE level (comments and
	// string literals scrubbed) so authored DSL cannot regress; programmatic
	// Step construction and logic bodies (args.event.payload.X, a logic-arg
	// read) are different surfaces and unaffected.
	if eventPayloadReadPattern.MatchString(scrubSourceForPayloadScan(source)) {
		return nil, fmt.Errorf("automation %q: event.payload.<field> reads are retired (G5, epic #2352) -- declare the field in the args { } block and read it bare. Migrate with: go run ./scripts/migrations/event_payload_args -write <dsl-root>", automation.Name)
	}

	// Validate trigger for potential misconfigurations
	l.validateTrigger(&automation)

	// @secret redaction (memql#3183): stamp ArgsField.Secret from the trigger
	// concept's @secret fields, so a rejected value on one of them is redacted
	// out of the fire-time refusal message -- and out of the WARN log that
	// message is written to. Must run AFTER the trigger normalization above
	// (the topic carries the concept binding) and it is the single choke point
	// for every automation the tree loader and the authoring sandbox produce.
	// Fails open on a nil registry / non-concept trigger; see args_secret.go.
	markSecretArgsFields(&automation, l.registry)

	if l.logger != nil {
		l.logger.Debug("automation compiled from .memql",
			"name", automation.Name,
			"path", path,
			"stepCount", len(automation.Steps),
		)
	}

	return &automation, nil
}

// parseResolveCompile parses source, runs concept resolution on the AST, then compiles.
// This replaces compiler.CompileSource to insert the resolution step.
func (l *Loader) parseResolveCompile(source, path string) (*compiler.CompileResult, error) {
	// Apply struct-form rewriters before tokenisation. The automation
	// loader bypasses compiler.CompileSource (so it can interleave
	// concept resolution between parse and compile), which means it
	// also has to run the rewriters that CompileSource normally fires.
	if languageParser.LooksLikeStructLogic(source) {
		rewritten, err := languageParser.NormaliseLogicSource(source)
		if err != nil {
			return nil, fmt.Errorf("logic rewrite: %w", err)
		}
		source = rewritten
	}
	if languageParser.LooksLikeStructAutomation(source) {
		rewritten, err := languageParser.NormaliseAutomationSource(source)
		if err != nil {
			return nil, fmt.Errorf("automation rewrite: %w", err)
		}
		source = rewritten
	} else if languageParser.LooksLikeLegacyAutomation(source) {
		return nil, fmt.Errorf("automation source: `func (Automation) NAME(...)` is retired -- author the struct form: `automation NAME { step <name> { logic <bareName> { ... } } }`. See dsl/v1/automations/v1/identity/expireDelegations/automation.memql for a worked example.")
	}

	// Tokenize
	lexer := languageParser.NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil, fmt.Errorf("lexer error: %w", err)
	}

	// Parse
	p := languageParser.NewParser(tokens)
	p.SetDocComments(lexer.DocComments())
	ast, err := p.Parse()
	if err != nil {
		return nil, fmt.Errorf("parser error: %w", err)
	}

	file, ok := ast.(*languageParser.File)
	if !ok {
		// Fall back to CompileSource for non-file AST (shouldn't happen for automations)
		return compiler.CompileSource(source)
	}

	// Resolve use declarations if present
	if len(file.Uses) > 0 && l.registry != nil {
		version := versionFromFilePath(path)
		if version == "" {
			version = "v1"
		}
		if err := resolveFileUseDeclarations(file, version, l.registry); err != nil {
			return nil, fmt.Errorf("concept resolution: %w", err)
		}
	}

	// Normalize structured @trigger forms to the canonical 5-segment
	// topic string. Runs regardless of whether the file carries `use`
	// declarations -- the unified automation loader extracts a single
	// automation slice that strips any sibling `use` lines.
	if err := normalizeStructuredTriggers(file); err != nil {
		return nil, fmt.Errorf("trigger normalization: %w", err)
	}

	// Compile the resolved AST
	comp := compiler.NewDefault()
	return comp.CompileFile(file)
}

// resolveFileUseDeclarations resolves symbolic concept references in a parsed file.
// This is the automation-side equivalent of memql.ConceptResolver.ResolveFile,
// duplicated here to avoid a circular import between automations and component/memql.
func resolveFileUseDeclarations(file *languageParser.File, version string, registry memoryNodes.Registry) error {
	symbols := make(map[string]string, len(file.Uses))

	for _, u := range file.Uses {
		parts := u.Parts
		resolvedVersion := version
		startIdx := 0

		// Check if first part looks like a version (v1, v2, etc.)
		if len(parts) > 0 && len(parts[0]) >= 2 && parts[0][0] == 'v' && parts[0][1] >= '0' && parts[0][1] <= '9' {
			resolvedVersion = parts[0]
			startIdx = 1
		}

		if len(parts)-startIdx < 2 {
			return fmt.Errorf("use declaration %q requires at least domain.entity", u.Path)
		}

		// Form B (`use <ns>.<construct>.{ name1, name2 }`): the dotted path
		// is a MODULE HINT, not a concept id. Resolve each imported NAME by
		// trailing-segment match against the registry. Names that don't
		// resolve to a concept are non-concept construct imports (logic /
		// shapes / specs / mutations / queries / traits / builtins / ...)
		// and are TOLERATED -- they're symbols the function registry resolves
		// at runtime, never concepts the automation compile must bind. This
		// mirrors memql.ConceptResolver.resolveUseDeclarations. Treating the
		// path itself as a concept id (the legacy Form-A branch below) would
		// synthesize a bogus `v1:<ns>:logic` id from `use <ns>.logic.{ ... }`
		// and fail every Form-B automation at the dry-run compile step --
		// the regression behind memql#1681 (the dry-run source extractor
		// prepends the file-top `use` imports, which the unified-tree load
		// path strips, so only the dry-run/inline path exercised this).
		if len(u.Names) > 0 {
			nsHint := parts[startIdx] // namespace segment (after any version)
			for _, name := range u.Names {
				if _, dup := symbols[name]; dup {
					continue
				}
				conceptId, err := resolveConceptByTrailingSegment(registry, name, nsHint)
				if err != nil {
					// Non-concept construct import (e.g. a `logic` symbol) --
					// tolerate it and let the function registry bind it at run
					// time.
					continue
				}
				symbols[name] = conceptId
			}
			continue
		}

		// Form A (legacy, no brace-list): the dotted path IS the concept id.
		conceptParts := parts[startIdx:]
		conceptId := resolvedVersion + ":" + strings.Join(conceptParts, ":")

		if _, err := registry.Get(conceptId); err != nil {
			return fmt.Errorf("use %s: concept %q not found in registry", u.Path, conceptId)
		}

		u.ResolvedId = conceptId
		leafName := u.LeafName()
		if _, ok := symbols[leafName]; ok {
			return fmt.Errorf("ambiguous reference %q (use 'as' alias to disambiguate)", leafName)
		}
		symbols[leafName] = conceptId
	}

	// Walk definitions and resolve concept references in attributes
	for _, def := range file.Definitions {
		fd, ok := def.(*languageParser.FunctionDef)
		if !ok {
			continue
		}
		for _, attr := range fd.Attributes {
			if attr.Name != languageParser.AttrTrigger {
				continue
			}
			onVal, hasOn := attr.Args["on"]
			if !hasOn {
				continue
			}
			onStr, ok := onVal.(string)
			if !ok {
				return fmt.Errorf("@trigger on= must be a string, got %T", onVal)
			}
			lastDot := strings.LastIndex(onStr, ".")
			if lastDot < 0 {
				return fmt.Errorf("@trigger on=%q must be <concept>.<eventType>", onStr)
			}
			conceptRef := onStr[:lastDot]
			eventType := onStr[lastDot+1:]

			switch eventType {
			case "created", "updated", "deleted":
			default:
				return fmt.Errorf("@trigger on= unknown event type %q", eventType)
			}

			conceptId, ok := symbols[conceptRef]
			if !ok {
				// Try full path match
				for _, u := range file.Uses {
					if u.Path == conceptRef {
						conceptId = u.ResolvedId
						ok = true
						break
					}
				}
			}
			if !ok {
				return fmt.Errorf("@trigger on=%s: unresolved concept reference %q", onStr, conceptRef)
			}

			delete(attr.Args, "on")
			attr.Args["event"] = fmt.Sprintf("graph.node.%s.%s", eventType, conceptId)
		}
	}

	return nil
}

// resolveConceptByTrailingSegment resolves a bare imported name (the leaf of a
// Form-B `use <ns>.<construct>.{ <name> }` import) to its canonical concept id
// by trailing-segment match against the registry, disambiguating an ambiguous
// trailing segment with nsHint (the namespace segment of the importing path).
// Returns an error when no concept matches -- the caller treats that as a
// non-concept construct import (logic / shape / spec / mutation / query / ...)
// and tolerates it. Mirrors
// memql.ConceptResolver.resolveBareConceptNameWithNamespace; kept local to the
// automations package to avoid widening the memql resolver's exported surface.
func resolveConceptByTrailingSegment(registry memoryNodes.Registry, name, nsHint string) (string, error) {
	if registry == nil {
		return "", fmt.Errorf("concept registry not available")
	}
	var matches []string
	for _, c := range registry.List() {
		if c == nil {
			continue
		}
		idx := strings.LastIndex(c.Name, ":")
		if idx < 0 {
			continue
		}
		if c.Name[idx+1:] == name {
			matches = append(matches, c.Name)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no registered concept has trailing segment %q", name)
	case 1:
		return matches[0], nil
	}
	// Ambiguous trailing segment: disambiguate by the namespace hint from the
	// importing `use` path, keeping only matches that carry it as an interior
	// segment (`:<nsHint>:`).
	if nsHint != "" {
		needle := ":" + nsHint + ":"
		var nsMatches []string
		for _, m := range matches {
			if strings.Contains(m, needle) {
				nsMatches = append(nsMatches, m)
			}
		}
		if len(nsMatches) == 1 {
			return nsMatches[0], nil
		}
	}
	return "", fmt.Errorf("ambiguous concept name %q matches %d concepts: %s", name, len(matches), strings.Join(matches, ", "))
}

// normalizeStructuredTriggers walks every FunctionDef in the file and
// rewrites structured @trigger annotations into the canonical
// 5-segment topic string. The structured form is:
//
//	@trigger(event="node.created", concept="v1:foo:bar"[, partition="*"])
//
// Recognised when event= is one of the allowed action kinds (no
// "graph." prefix) AND a concept= field is present. Other event-only
// forms (system.startup, cognition.*, schedule=) are left alone.
//
// concept= must be a fully-qualified concept id string -- the structured
// form in the new tree carries the literal id, since the unified
// automation loader strips sibling `use` decls from each slice.
func normalizeStructuredTriggers(file *languageParser.File) error {
	for _, def := range file.Definitions {
		fd, ok := def.(*languageParser.FunctionDef)
		if !ok {
			continue
		}
		autoBody, _ := fd.Body.(*languageParser.AutomationDef)
		for _, attr := range fd.Attributes {
			if attr.Name != languageParser.AttrTrigger {
				continue
			}
			eventVal, hasEvent := attr.Args["event"]
			if !hasEvent {
				continue
			}
			eventStr, isStr := eventVal.(string)
			if !isStr || !ast.EventKindAllowed(eventStr) {
				continue
			}
			parsed, err := ast.ParseStructuredTriggerArgs(attr.Args)
			if err != nil {
				return fmt.Errorf("automation %q: @trigger: %w", fd.Name, err)
			}
			if !parsed.HasConcept {
				return fmt.Errorf("automation %q: @trigger event=%q (structured form) requires a concept= field", fd.Name, eventStr)
			}
			conceptId := strings.TrimSpace(parsed.Concept)
			if conceptId == "" || !strings.Contains(conceptId, ":") {
				return fmt.Errorf("automation %q: @trigger concept=%q must be a fully-qualified concept id", fd.Name, parsed.Concept)
			}
			topic, err := ast.BuildTriggerTopic(eventStr, conceptId)
			if err != nil {
				return fmt.Errorf("automation %q: @trigger: %w", fd.Name, err)
			}
			delete(attr.Args, "concept")
			delete(attr.Args, "partition")
			attr.Args["event"] = topic
			// The parser already copied the unresolved event= into the
			// AutomationDef body. Patch it here so the compiler sees the
			// canonical topic.
			if autoBody != nil {
				if autoBody.Trigger == nil {
					autoBody.Trigger = &languageParser.TriggerDef{}
				}
				autoBody.Trigger.Event = topic
			}
		}
	}
	return nil
}

// versionFromFilePath extracts the version directory from a file path.
func versionFromFilePath(path string) string {
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if len(part) >= 2 && part[0] == 'v' && part[1] >= '0' && part[1] <= '9' {
			allDigits := true
			for _, ch := range part[1:] {
				if ch < '0' || ch > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				return part
			}
		}
	}
	return ""
}

// parseJSON parses automation JSON data.
func (l *Loader) parseJSON(data []byte, path string) (*Automation, error) {
	var automation Automation
	if err := json.Unmarshal(data, &automation); err != nil {
		return nil, fmt.Errorf("parsing JSON: %w", err)
	}

	// Validate required fields.
	//
	// The `.json` suffix trim is vestigial: since memql#2858 removed the
	// on-disk .json loader, the sole caller is the LogicRunner
	// (logic_runner.go compileBodyToAutomation) passing a synthetic
	// "logic:<name>" path, which never carries the suffix. Harmless, and
	// left rather than changed because `path` is caller-supplied and a
	// future caller may again pass a filename.
	if automation.Name == "" {
		base := filepath.Base(path)
		automation.Name = strings.TrimSuffix(base, ".json")
	}

	if len(automation.Steps) == 0 && automation.OnComplete == nil {
		return nil, fmt.Errorf("automation must have at least one step")
	}

	// Validate steps
	if err := l.validateSteps(automation.Steps); err != nil {
		return nil, fmt.Errorf("invalid steps: %w", err)
	}

	// Validate trigger for potential misconfigurations
	l.validateTrigger(&automation)

	// Same @secret stamp as compileMemQL (memql#3183). A LogicRunner-compiled
	// body carries no graph trigger today, so this is a no-op in practice --
	// present so the second construction path cannot silently become the
	// unredacted one if a caller ever hands it a concept-scoped trigger.
	markSecretArgsFields(&automation, l.registry)

	if l.logger != nil {
		// Not ".json" any more: memql#2858 removed the on-disk .json
		// loader, so every call now comes from the LogicRunner compiling a
		// logic body in memory.
		l.logger.Debug("automation parsed from compiler JSON",
			"name", automation.Name,
			"path", path,
			"stepCount", len(automation.Steps),
		)
	}

	return &automation, nil
}

// validateSteps validates a list of steps.
func (l *Loader) validateSteps(steps []*Step) error {
	ids := make(map[string]bool)

	for i, step := range steps {
		if step == nil {
			return fmt.Errorf("step %d is nil", i)
		}

		if step.ID == "" {
			return fmt.Errorf("step %d: id is required", i)
		}

		if ids[step.ID] {
			// Provide more helpful error message for forEach loops with duplicate variable names
			if strings.HasPrefix(step.ID, "forEach_") {
				loopVar := strings.TrimPrefix(step.ID, "forEach_")
				return fmt.Errorf("step %d: duplicate id %q - multiple for-loops use the same variable name %q; each for-loop must use a unique variable name", i, step.ID, loopVar)
			}
			return fmt.Errorf("step %d: duplicate id %q", i, step.ID)
		}
		ids[step.ID] = true

		if step.Type == "" {
			return fmt.Errorf("step %q: type is required", step.ID)
		}

		// Validate type-specific configuration
		switch step.Type {
		case StepTypeQuery:
			if step.Query == nil {
				return fmt.Errorf("step %q: query configuration required for type 'query'", step.ID)
			}
		case StepTypeMutation:
			if step.Mutation == nil {
				return fmt.Errorf("step %q: mutation configuration required for type 'mutation'", step.ID)
			}
		case StepTypeWebhook:
			if step.Webhook == nil {
				return fmt.Errorf("step %q: webhook configuration required for type 'webhook'", step.ID)
			}
		case StepTypeEvent:
			if step.Event == nil {
				return fmt.Errorf("step %q: event configuration required for type 'event'", step.ID)
			}
		case StepTypeFunction:
			if step.Function == nil {
				return fmt.Errorf("step %q: function configuration required for type 'function'", step.ID)
			}
		case StepTypeAction:
			// Action-library replay step (#1758): one external capability on
			// a resolved surface. Consumed by the deploy bundle's
			// deployEngineCluster (I10 #2224), the first authored automation to
			// drive action steps through this loader.
			if step.Action == nil {
				return fmt.Errorf("step %q: action configuration required for type 'action'", step.ID)
			}
			if step.Action.Ref == "" {
				return fmt.Errorf("step %q: action step requires a non-empty ref", step.ID)
			}
		case StepTypeForEach:
			if step.ForEach == nil {
				return fmt.Errorf("step %q: forEach configuration required for type 'forEach'", step.ID)
			}
			if err := l.validateSteps(step.ForEach.Do); err != nil {
				return fmt.Errorf("step %q forEach: %w", step.ID, err)
			}
		case StepTypeParallel:
			if step.Parallel == nil {
				return fmt.Errorf("step %q: parallel configuration required for type 'parallel'", step.ID)
			}
			if err := l.validateSteps(step.Parallel.Branches); err != nil {
				return fmt.Errorf("step %q parallel: %w", step.ID, err)
			}
		case StepTypeSwitch:
			if step.Switch == nil {
				return fmt.Errorf("step %q: switch configuration required for type 'switch'", step.ID)
			}
		case StepTypeShape:
			if step.Shape == nil {
				return fmt.Errorf("step %q: shape configuration required for type 'shape'", step.ID)
			}
		case StepTypeDetectLeadSignal:
			if step.DetectLeadSignal == nil {
				return fmt.Errorf("step %q: detectLeadSignal configuration required for type 'detectLeadSignal'", step.ID)
			}
		case StepTypeEmitConceptCard:
			if step.EmitConceptCard == nil {
				return fmt.Errorf("step %q: emitConceptCard configuration required for type 'emitConceptCard'", step.ID)
			}
		case StepTypeAutomation:
			if step.Automation == nil {
				return fmt.Errorf("step %q: automation configuration required for type 'automation'", step.ID)
			}
		default:
			return fmt.Errorf("step %q: unknown type %q", step.ID, step.Type)
		}
	}

	return nil
}

// Validate performs full validation on an automation.
func (l *Loader) Validate(automation *Automation) error {
	if automation == nil {
		return fmt.Errorf("automation is nil")
	}

	if automation.Name == "" {
		return fmt.Errorf("automation name is required")
	}

	if len(automation.Steps) == 0 {
		return fmt.Errorf("automation must have at least one step")
	}

	// Validate trigger configuration for potential misconfigurations
	l.validateTrigger(automation)

	return l.validateSteps(automation.Steps)
}

// validateTrigger checks for potential misconfigurations in the automation trigger.
// It warns about contradicting concept filters but does not return an error since
// some edge cases might be intentional.
func (l *Loader) validateTrigger(automation *Automation) {
	if automation.Trigger == nil || automation.Trigger.Event == "" {
		return
	}

	trigger := automation.Trigger
	topicConcept := extractConceptFromTopic(trigger.Event)
	filterConcept := extractConceptFromFilter(trigger.Filter)

	// No concept in either - nothing to validate
	if topicConcept == "" || filterConcept == "" {
		return
	}

	// Check for contradiction
	if topicConcept != filterConcept {
		if l.logger != nil {
			l.logger.Warn("trigger has contradicting concept filter - automation may never fire",
				"automation", automation.Name,
				"topicConcept", topicConcept,
				"filterConcept", filterConcept,
				"topic", trigger.Event,
				"filter", trigger.Filter,
			)
		}
		return
	}

	// Concept matches - the filter is redundant (but valid)
	if l.logger != nil {
		l.logger.Debug("trigger filter has redundant concept check (already in topic)",
			"automation", automation.Name,
			"concept", topicConcept,
		)
	}
}

// extractConceptFromTopic extracts the concept ID from a graph CDC event
// pattern of the form graph.node.{action}.{concept} emitted by
// BuildTopicWithConcept.
//
// Returns "" when the topic doesn't identify a single concept -- any
// wildcard in the concept position, fewer than 4 segments, or not a
// graph.node event.
func extractConceptFromTopic(topic string) string {
	parts := strings.Split(topic, ".")
	if len(parts) < 4 {
		return ""
	}
	if parts[0] != "graph" || parts[1] != "node" {
		return ""
	}

	// Concepts don't contain dots, so joining any trailing segments with "."
	// is safe: a 4-segment topic yields parts[3] alone.
	concept := strings.Join(parts[3:], ".")
	if concept == "" || strings.ContainsAny(concept, "*#") {
		return ""
	}
	return concept
}

// extractConceptFromFilter extracts the concept from a filter like "concept==v1:cognition:participant;...".
// Returns empty string if no concept filter is found.
func extractConceptFromFilter(filter string) string {
	if filter == "" {
		return ""
	}

	// Split by semicolons (AND) and commas (OR)
	// Look for concept== patterns
	for _, part := range strings.FieldsFunc(filter, func(r rune) bool {
		return r == ';' || r == ','
	}) {
		part = strings.TrimSpace(part)

		// Check for concept== pattern
		if strings.HasPrefix(part, "concept==") {
			value := strings.TrimPrefix(part, "concept==")
			// Remove quotes if present
			value = strings.Trim(value, `"'`)
			return value
		}
	}

	return ""
}
