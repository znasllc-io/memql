package automations

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/compiler"
	"github.com/znasllc-io/memql/component/language/dslclause"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// Loader loads automation definitions from the unified DSL tree.
type Loader struct {
	logger   *slog.Logger
	registry memoryNodes.Registry
	// functions is what the static loop check walks (loop_check.go), and
	// loopLogOnce keeps its log to the loader's first load.
	functions   *memql.FunctionRegistry
	loopLogOnce sync.Once
}

// LoaderOptions configures the automation loader.
type LoaderOptions struct {
	Logger   *slog.Logger
	Registry memoryNodes.Registry
	// Functions is the engine's function registry, from which the static
	// loop check reads what each automation writes (memql#5381). Without it
	// the check does not run, and the load logs that it did not.
	Functions *memql.FunctionRegistry
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
		logger:    logger,
		registry:  opts.Registry,
		functions: opts.Functions,
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
//  2. A logic's name, resolving to the automation whose statement calls it.
//     This makes a logic reachable by the name an author naturally reaches
//     for (the QA pain in #1663: `revokeExpiredDelegations` resolves to the
//     `expireDelegations` automation that calls it).
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

	// 2. Logic-alias resolution: a logic resolves to the automation whose
	//    statement calls it.
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

// invokedLogicNames returns the names of the logic an automation's
// statements call, walking the lists a `for` and a parallel's branches hold.
// A sub-automation is a StepTypeAutomation step, and is not one.
func invokedLogicNames(a *Automation) []string {
	if a == nil {
		return nil
	}
	var out []string
	var walk func(steps []*Step)
	walk = func(steps []*Step) {
		for _, s := range steps {
			if s == nil {
				continue
			}
			if s.Type == StepTypeFunction && s.Function != nil && s.Function.Kind == "logic" {
				out = append(out, s.Function.Name)
			}
			if s.ForEach != nil {
				walk(s.ForEach.Do)
			}
			if s.Parallel != nil {
				walk(s.Parallel.Branches)
			}
			if s.Block != nil {
				walk(s.Block.Steps)
			}
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
	return l.compileMemQLFrom(source, source, path)
}

// compileMemQLFrom is compileMemQL for a source derived from authored -- a
// slice of a file placed on its line there: a parse error or a refusal is
// reported where authored has it (memql#5364).
func (l *Loader) compileMemQLFrom(authored, source, path string) (*Automation, error) {
	// An unknown / dead / retired annotation on an automation (#2712) is
	// refused by the parser below, against the annotation registry, when
	// parseResolveCompile parses the source (memql#5359) -- the same gate
	// every construct runs, so there is no second text scan here.

	// Extract first-class precondition blocks (Epic 4 / memql#2139) from the
	// source: the statement parser steps over them, and the parsed
	// preconditions are re-attached to the compiled Automation below.
	preconditions, source, err := extractPreconditions(source)
	if err != nil {
		return nil, fmt.Errorf("parsing automation preconditions: %w", err)
	}

	// Parse, resolve concept references, then compile. The text compileMemQL
	// was handed is what the author wrote (or its slice): parse positions are
	// reported against it, not against the precondition-stripped source.
	result, err := l.parseResolveCompile(authored, source, path)
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

	// An automation's expressions are parsed ONCE, here, and cached on the
	// steps (memql#5367): a parse error or an expression over the M tier's
	// static cost limit refuses the automation at load rather than at its
	// first run. Before the name check, which reads the parsed nodes.
	if err := prepareExpressions(&automation, l.registry); err != nil {
		return nil, err
	}
	// @loop and @mode (epic memql#5380), judged against the trigger filter
	// prepareExpressions just parsed (loop_prepare.go).
	if err := prepareLoopAndMode(&automation); err != nil {
		return nil, err
	}

	// The trigger filter's and the preconditions' names: the body's were
	// compiler.CheckBody's (args_resolution.go). Both the tree loader and the
	// authoring-sandbox hook flow through here, so authored bundles get the
	// same gate.
	if err := validateOuterExpressionNames(&automation); err != nil {
		return nil, err
	}

	// G5 (memql#2367, ADR Decision 6): `event.payload.X` reads are RETIRED
	// in automation bodies -- the payload binds to the args { } contract and
	// is read args.<field>. Rejected at the SOURCE level (comments and
	// string literals scrubbed) so authored DSL cannot regress; programmatic
	// Step construction and logic bodies (args.event.payload.X, a logic-arg
	// read) are different surfaces and unaffected.
	if eventPayloadReadPattern.MatchString(scrubSourceForPayloadScan(source)) {
		return nil, fmt.Errorf("automation %q: dotted `event.<field>` reads are retired (G5, epic #2352; widened in memql#3610) -- declare the field in the args { } block and read it as args.<field>. Note that `event.node.<field>` never resolved to anything at all: the CDC envelope has no `node` key, so it silently decided false rather than erroring. Migrate with: memqlmigrate --rewrite=bodies <dsl-root>", automation.Name)
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

// parseAutomationFile parses an automation slice, with the author's positions
// (authored) carried in the tokens. It is the parse half of
// parseResolveCompile. An automation is read as written: the parser reads its
// statements and refuses a retired form by name (epic memql#5370). file is
// nil when the source parses to something other than a file.
func parseAutomationFile(authored, source string) (file *languageParser.File, err error) {
	if err := languageParser.RejectLegacyProceduralAuthorForm(source); err != nil {
		return nil, languageParser.PositionRewriteError(authored, err)
	}

	// Tokenize with the author's positions carried in the tokens, so a
	// refusal names the author's line and column.
	lexer := languageParser.NewLexer(languageParser.PositionLowering(authored, source))
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil, fmt.Errorf("lexer error: %w", err)
	}

	p := languageParser.NewParser(tokens)
	p.SetDocComments(lexer.DocComments())
	ast, err := p.Parse()
	if err != nil {
		return nil, fmt.Errorf("parser error: %w", err)
	}

	f, _ := ast.(*languageParser.File)
	return f, nil
}

// parseResolveCompile parses source, runs concept resolution on the AST, then compiles.
// This replaces compiler.CompileSource to insert the resolution step.
//
// authored is the text source was derived from; parse positions are reported
// against it (languageParser.PositionLowering, memql#5364).
func (l *Loader) parseResolveCompile(authored, source, path string) (*compiler.CompileResult, error) {
	file, err := parseAutomationFile(authored, source)
	if err != nil {
		return nil, err
	}
	if file == nil {
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
	if err := normalizeStructuredTriggers(file, l.registry); err != nil {
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
// forms (system.startup, cognition.*, schedule=) are left alone -- an
// `event=` the structured form does not recognise is a RAW TOPIC and
// subscribes verbatim.
//
// concept= must be a fully-qualified concept id string that RESOLVES in the
// supplied registry -- the structured form in the new tree carries the literal
// id, since the unified automation loader strips sibling `use` decls from each
// slice. registry may be nil (in-package tests, the LogicRunner's loader), in
// which case existence is not checked; every production construction site
// passes the live registry.
//
// Two load-time refusals live here (memql#3614); the third, exactly one
// @trigger per automation, is the annotation registry's repeat rule at parse
// time (memql#5359):
//
//   - An unrecognised `event=` may not carry concept=. That kwarg is
//     meaningful ONLY to the structured node.* form: for any other event kind
//     the old code hit `continue`, dropped it on the floor, and subscribed to
//     the bare `event=` string. `dsl/deployment/automations.memql` shipped
//     `@trigger(event="deploy.requested", concept="v1:cluster:deployment")`
//     and got plain `deploy.requested` -- the concept scoping the author wrote
//     was not in effect and nothing said so. The fix REFUSES
//     rather than inventing a `deploy.requested.<concept>` topic shape: the
//     concept segment exists because the graph CDC publisher composes it into
//     `graph.node.<action>.<concept>`, and an arbitrary application topic has
//     no such convention to honour. An author who wants concept scoping wants a
//     node.* kind; an author who wants a raw topic wants no concept= kwarg.
//
//   - concept= must name a registered concept. `strings.Contains(id, ":")` was
//     the whole check, so `v1:cluster:nodeZZZ` compiled to a topic no CDC event
//     will ever carry. The loader already holds the registry for
//     `use`-import resolution.
func normalizeStructuredTriggers(file *languageParser.File, registry memoryNodes.Registry) error {
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
				// schedule-only trigger, or a trigger with no wiring at all.
				// validateTriggerWiring is what refuses the latter, after the
				// compile has produced the Automation.
				if err := rejectStrayStructuredKwargs(fd.Name, attr.Args, ""); err != nil {
					return err
				}
				continue
			}
			eventStr, isStr := eventVal.(string)
			if !isStr {
				return fmt.Errorf("automation %q: @trigger event= must be a string, got %T", fd.Name, eventVal)
			}
			if !ast.EventKindAllowed(eventStr) {
				// Raw-topic subscription (system.startup, an already-composed
				// graph.node.* topic, an application topic). Legal -- but the
				// structured kwargs mean nothing here and were being dropped.
				if err := rejectStrayStructuredKwargs(fd.Name, attr.Args, eventStr); err != nil {
					return err
				}
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
			// An EMPTY registry means concepts have not been loaded yet, not
			// that every concept is unknown -- and the loader cannot tell the
			// two apart from in here (memql#3614).
			//
			// Concepts load in the database phase, before automations, on the
			// real boot path. They do NOT on every other path that reaches
			// this loader: `LoadByName` serves MCP `run_automation` at runtime,
			// and several test binaries construct a loader directly. Validating
			// against a registry this function does not populate made all 13
			// concept-scoped automations in the shipped tree "unregistered"
			// under `-tags mcp`, which is how CI caught it.
			//
			// So the check runs only when there is a registry to check against.
			// This is the same decidability rule the integration-executor check
			// below applies: refuse what this binary can actually judge, and
			// stay quiet about what it cannot.
			if registry != nil && len(registry.List()) > 0 {
				if _, cerr := registry.Get(conceptId); cerr != nil {
					return fmt.Errorf("automation %q: @trigger concept=%q is not a registered concept, so the compiled topic %q can never match a CDC event: %w",
						fd.Name, conceptId, "graph."+eventStr+"."+conceptId, cerr)
				}
			}
			topic, err := ast.BuildTriggerTopic(eventStr, conceptId)
			if err != nil {
				return fmt.Errorf("automation %q: @trigger: %w", fd.Name, err)
			}
			delete(attr.Args, "concept")
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

// rejectStrayStructuredKwargs refuses concept= on a @trigger whose event= is
// not one of the structured node.* kinds (or is absent entirely). The kwarg
// is consumed ONLY by the structured rewrite above; anywhere else it was read,
// discarded, and never mentioned again.
func rejectStrayStructuredKwargs(automationName string, args map[string]any, eventStr string) error {
	if _, ok := args["concept"]; !ok {
		return nil
	}
	where := "a @trigger with no event="
	if eventStr != "" {
		where = fmt.Sprintf("@trigger event=%q", eventStr)
	}
	return fmt.Errorf("automation %q: %s carries concept=, which only the structured graph-CDC form consumes -- "+
		"it was being dropped, so the scoping you wrote was not in effect. "+
		"Either use a structured event kind (%s) to get concept scoping, or drop concept= and subscribe to the raw topic as written",
		automationName, where, strings.Join(ast.AllowedEventKinds(), " / "))
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
	// on-disk .json loader, the callers pass the compiled JSON of a loaded
	// automation or the LogicRunner's synthetic "logic:<name>" path, neither
	// of which carries the suffix. Harmless, and left rather than changed
	// because `path` is caller-supplied and a future caller may again pass a
	// filename.
	if automation.Name == "" {
		base := filepath.Base(path)
		automation.Name = strings.TrimSuffix(base, ".json")
	}

	if len(automation.Steps) == 0 {
		return nil, fmt.Errorf("automation must have at least one step")
	}

	// Validate steps
	if err := l.validateSteps(automation.Steps); err != nil {
		return nil, fmt.Errorf("invalid steps: %w", err)
	}

	// The same one-time parse compileMemQL runs (memql#5367).
	if err := prepareExpressions(&automation, l.registry); err != nil {
		return nil, err
	}

	// Validate trigger for potential misconfigurations
	l.validateTrigger(&automation)

	// Same @secret stamp as compileMemQL (memql#3183). A LogicRunner-compiled
	// body carries no graph trigger today, so this is a no-op in practice --
	// present so the second construction path cannot silently become the
	// unredacted one if a caller ever hands it a concept-scoped trigger.
	markSecretArgsFields(&automation, l.registry)

	if l.logger != nil {
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
		case StepTypeAutomation:
			if step.Automation == nil {
				return fmt.Errorf("step %q: automation configuration required for type 'automation'", step.ID)
			}
		case StepTypeExpression:
			if strings.TrimSpace(step.Expression) == "" {
				return fmt.Errorf("step %q: an expression step requires its expression", step.ID)
			}
		case StepTypeReturn:
			if step.Return == nil {
				return fmt.Errorf("step %q: return configuration required for type 'return'", step.ID)
			}
		case StepTypeBlock:
			if step.Block == nil {
				return fmt.Errorf("step %q: block configuration required for type 'block'", step.ID)
			}
			if err := l.validateSteps(step.Block.Steps); err != nil {
				return fmt.Errorf("step %q block: %w", step.ID, err)
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

// extractConceptFromFilter extracts the concept a trigger filter narrows to:
// a top-level conjunct `<param>.concept == "<id>"` of the filter's lambda
// (`@filter(row => ...)`), read as a tree. Returns "" when there is none, and
// for a filter that is not a one-parameter lambda -- which PrepareExpressions
// refuses, so no loaded automation carries one (memql#5367).
func extractConceptFromFilter(filter string) string {
	if filter == "" || !dslclause.OpensLambda(filter) {
		return ""
	}
	lam, err := languageParser.ParseV1Lambda(filter)
	if err != nil || len(lam.Params) != 1 {
		return ""
	}
	for _, c := range ast.Conjuncts(lam.Body) {
		b, ok := c.(*ast.BinaryExpr)
		if !ok || b.Op != "==" {
			continue
		}
		root, fields, isPath := ast.MemberPath(ast.Unparen(b.Left))
		lit, isLit := ast.Unparen(b.Right).(*ast.LiteralExpr)
		if isPath && isLit && root == lam.Params[0] && len(fields) == 1 && fields[0] == "concept" {
			if s, ok := lit.Value.(string); ok {
				return s
			}
		}
	}
	return ""
}
