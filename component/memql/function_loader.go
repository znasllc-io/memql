package memql

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/compiler"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// builtinFunctionDefinition represents the JSON schema for builtin function files.
type builtinFunctionDefinition struct {
	Name        string                        `json:"name"`
	Description string                        `json:"description"`
	Type        string                        `json:"type"`
	Executor    string                        `json:"executor"`
	Aliases     []string                      `json:"aliases,omitempty"`
	Args        *builtinArgContractDefinition `json:"args,omitempty"`
}

type builtinArgContractDefinition struct {
	Profile              string            `json:"profile,omitempty"`
	StringKey            string            `json:"stringKey,omitempty"`
	Required             []string          `json:"required,omitempty"`
	Properties           map[string]string `json:"properties,omitempty"`
	AdditionalProperties *bool             `json:"additionalProperties,omitempty"`
}

// loadBuiltinFunctions / loadEmbeddedFunctions are stubs as of
// Pass 3 of the DSL restructure migration. The legacy walk over
// dsl/v1/queries/, dsl/v1/mutations/, dsl/v1/logic/ was retired;
// the real loading happens in LoadUnifiedFunctions + LoadUnifiedBuiltins
// (component/memql/unified_functions_loader.go +
// unified_kinds_loader.go) which read from dsl/<domain>/*.memql.
// engine.go calls these stubs first (giving an empty registry the
// unified loaders then fill) so the bootstrap signature is preserved.

// loadBuiltinFunctions returns an empty map. See note above.
func loadBuiltinFunctions(logger *slog.Logger) (map[string]*Function, error) {
	_ = logger
	return map[string]*Function{}, nil
}

// loadEmbeddedFunctions returns an empty registry. See note above.
func loadEmbeddedFunctions(logger *slog.Logger, registry memoryNodes.Registry) (*FunctionRegistry, error) {
	_ = logger
	_ = registry
	return newFunctionRegistry(), nil
}

// === Legacy walker code retired in Pass 3 ===
// discoverBuiltinDirs / discoverFunctionVersionDirs / collectVersionDirs
// / loadFlatFunctionFile / readEmbeddedFunctionFile /
// expectedFunctionNameFromFile -- all deleted with the legacy walk.
// Inert placeholder follows; the next block was the legacy walker.

var _legacyWalkRetired = true // marker so the next deletion attempt is grep-able

/*
// LEGACY: the deleted block was a ~540-line implementation that
// walked dsl/v1/queries/, dsl/v1/mutations/, dsl/v1/logic/ and
// loaded each .memql file in turn. The unified loader's
// LoadUnifiedFunctions / LoadUnifiedBuiltins handle the same work
// against the new dsl/<domain>/*.memql tree.
*/

// discoverBuiltinDirsRetired is a sentinel-shaped function preserved
// for grep history. It returns nothing.
func discoverBuiltinDirsRetired(versionDir string) ([]string, error) { _ = versionDir; return nil, nil }

// (allowing blank lines) from a function .memql file. This is used for on-demand
// agent guidance (describeFunction/help), not for execution.
func extractLeadingCommentBlock(content string) string {
	lines := strings.Split(content, "\n")
	var out []string
	started := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			started = true
			// Strip the leading // and one optional space.
			txt := strings.TrimSpace(strings.TrimPrefix(trimmed, "//"))
			out = append(out, txt)
			continue
		}
		if trimmed == "" && started {
			out = append(out, "")
			continue
		}
		// Stop at the first non-comment content after we've started.
		if started {
			break
		}
		// Skip leading blank lines before comments.
	}
	joined := strings.TrimSpace(strings.Join(out, "\n"))
	return joined
}

// withRewriteCause augments a legacy-parser failure with the struct-form
// rewrite error that produced the un-normalised source, when there is
// one. The legacy parser rejects an un-rewritten struct construct with a
// generic "unexpected token <keyword>" that hides the real cause (e.g. a
// retired `insert <Concept> { ... }` named-write form -- memql#1055). The
// rewrite error names the exact construct and the migration, so callers
// (and the unifiedFunctionLoader's skip-slice warning) see why the slice
// failed rather than a misleading token complaint.
func withRewriteCause(parseErr, rewriteErr error) error {
	if rewriteErr == nil {
		return parseErr
	}
	return fmt.Errorf("%w (struct-form rewrite failed: %v)", parseErr, rewriteErr)
}

// tryParseNewFunctionSyntax attempts to parse a function .memql file using the new syntax:
// @enabled
// @description("...")
// query functionName() { expression }
//
// Enforces CQS file composition rules:
//   - Max 1 mutation per file (can have supporting queries)
//   - Unlimited queries per file
//   - Automations should be in automations/ directory, not queries/mutations
func tryParseNewFunctionSyntax(expectedName, expectedKind, content, origin string, registry memoryNodes.Registry) (*Function, error) {
	// Author-facing retirement (memql#303): reject the legacy
	// procedural form `func (Receiver) name(ctx any) ...` before any
	// rewriting runs. The internal IR is still procedural (the
	// rewriter below synthesises it from struct form), but no .memql
	// author may write that shape directly. The compiler test fixtures
	// that consume procedural source bypass this loader entirely.
	if err := languageParser.RejectLegacyProceduralAuthorForm(content); err != nil {
		return nil, fmt.Errorf("%s: %w", origin, err)
	}

	// Snapshot the signature-bound concepts BEFORE NormaliseAll
	// rewrites the struct form to procedural -- once the rewrite runs
	// the `<kind> <Concept> <name> {` shape is gone and the regex
	// in extractAllSignatureConceptNames has nothing to match.
	signatureConcepts := extractAllSignatureConceptNames(content)

	// Apply every struct-form rewriter defensively here too -- callers
	// that don't go through loadFlatFunctionFile (tests, ad-hoc uses)
	// would otherwise see a raw struct-form shape the legacy parser
	// doesn't grok. A per-stage rewrite error leaves `content` as the
	// un-normalised struct form, which then fails the legacy parser
	// below with a cryptic "unexpected token <keyword>" -- the rewrite
	// error is the ACTUAL cause (e.g. a retired `insert <Concept> { ... }`
	// named-write form, memql#1055). Keep it and attach it to the parse
	// failure so the real reason surfaces instead of being swallowed.
	var rewriteErr error
	if rewritten, rerr := languageParser.NormaliseAll(content); rerr == nil {
		content = rewritten
	} else {
		rewriteErr = rerr
	}

	// Keep the pre-translation source for the declared-usage validator
	// (Phase G.3.g pt 4). The validator scans `@use*(...)` annotation
	// targets + args fields against `\b<name>\.` references in the
	// body, which would already be translated to `payload.` if we ran
	// it on the post-translation source.
	rawSourceForUsage := content

	// Concept-namespace path translation. A function bound via the
	// canonical signature form (`mutation <Concept> <name>`) writes
	// payload references in the `<Concept>.X` form (e.g. `space.name`).
	// Translate every occurrence of `\b<name>\.` to `payload.` BEFORE
	// the parser sees the source, so downstream evaluator code keeps
	// reading rows under its existing `payload.X` accessor without
	// any AST-level changes. Translation is naive `\b<name>\.`
	// replacement -- it intentionally rewrites `<name>.` occurrences
	// in comments / docstrings too (comments don't affect parsing).
	// The signature concepts were captured from the PRE-rewrite
	// source above; we apply the translation against the post-rewrite
	// content here.
	for _, name := range signatureConcepts {
		content = translateSignatureConceptPathsToPayload(content, name)
	}

	// #987: resolve the typed foreign-concept form
	// `canonicalId(x, <importedConceptName>)` to the canonical-id string form
	// against the file's `use ...concepts.{ ... }` imports + the registry. The
	// quoted string form passes through unchanged (additive).
	//
	// BOTH arguments are the ASSEMBLY directory's, not the last path segment's
	// (memql#3026). The ambient rule admits an id this domain could have
	// declared, which means it has to be handed the same directory
	// AssembleConceptIdFromDeclInDir was -- unified_loader.go's
	// `dir := firstPathSegment(p)`, i.e. RootDomainFromFilePath here -- plus
	// that directory's namespace.pin.
	//
	// Passing DomainFromFilePath's LAST segment here is a WIDENING, not merely
	// a miss: for agents/tools/askSpecialist.memql it would admit any id whose
	// namespace is `tools`, which is a foreign domain's namespace that assembly
	// for this file can never emit -- a cross-namespace bind with no import, on
	// the path that derives row ids. The same-domain scope the last segment
	// describes is a different question, and this is not it.
	if resolved, cerr := NewConceptResolver(registry).ResolveCanonicalIdConceptRefsInNamespace(
		content, RootDomainFromFilePath(origin), declaredNamespaceForOrigin(origin)); cerr != nil {
		return nil, fmt.Errorf("%s: %w", origin, cerr)
	} else {
		content = resolved
	}

	// Try parsing with the full parser
	lexer := languageParser.NewLexer(content)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil, withRewriteCause(err, rewriteErr)
	}

	p := languageParser.NewParser(tokens)
	p.SetDocComments(lexer.DocComments())
	// Record the (fully-rewritten) source so a collection-chain logic step
	// RHS can be sliced back to its exact span during parsing (#2317). The
	// tokens were lexed from this same `content`, so the rune offsets line up.
	p.SetSource(content)
	ast, err := p.Parse()
	if err != nil {
		return nil, withRewriteCause(err, rewriteErr)
	}

	// Check if we got a File with definitions
	file, ok := ast.(*languageParser.File)
	if !ok {
		return nil, fmt.Errorf("expected File, got %T", ast)
	}

	if len(file.Definitions) == 0 {
		return nil, fmt.Errorf("no definitions found")
	}

	// Resolve symbolic concept references. Trigger when EITHER a
	// file-top `use` directive OR a signature-bound concept is
	// present -- the resolver handles both.
	if registry != nil && (len(file.Uses) > 0 || len(signatureConcepts) > 0) {
		version := VersionFromFilePath(origin)
		if version == "" {
			version = "v1" // default
		}
		resolver := NewConceptResolver(registry)
		// The ROOT domain, not the last path segment (memql#3084). Boot
		// assembles a concept id from the FIRST segment (unified_loader.go:
		// `dir := firstPathSegment(p)`), so for
		// `agents/tools/askSpecialist.memql` the hint must be `agents`.
		// DomainFromFilePath returns `tools` -- a namespace this file cannot
		// have declared anything under, and therefore cannot bind ambiently.
		if err := resolver.ResolveFileWithSignatureConceptsInDomain(file, version, signatureConcepts, RootDomainFromFilePath(origin)); err != nil {
			return nil, fmt.Errorf("concept resolution: %w", err)
		}
	}

	// Set BoundConcept from the resolved use declaration OR the
	// canonical construct-signature concept (`mutation <Concept>
	// <name> { ... }`). The resulting fully-qualified concept ID is
	// the implicit binding for bare `concept` in query expressions
	// and argument-less `insert()` calls. Exactly one binding source
	// is permitted; the concept-resolver guards against collisions.
	var boundConcept string
	// The single-use fallbacks below are LEGACY: they served the
	// procedural single-construct layout where the file's one import
	// WAS the concept binding. They must never fire for a construct
	// whose binding lives in its SIGNATURE -- before #2617 that held
	// only by accident of use-line counts, and the same-domain import
	// strip exposed the hijack: worker/queries.memql dropped to a
	// single (cross-domain) use line and every worker query silently
	// bound v1:planner:plan, which ensureBoundConceptFilter would
	// have injected into their row filters. Caught by the boot-
	// equivalence probe in the #2661 review round.
	legacyBindingEligible := len(signatureConcepts) == 0
	if legacyBindingEligible && len(file.Definitions) > 0 {
		fd, ok := file.Definitions[0].(*languageParser.FunctionDef)
		legacyBindingEligible = ok && (fd.Type == languageParser.FunctionTypeQuery || fd.Type == languageParser.FunctionTypeMutation)
	}
	// Legacy Form A (`use cognition.space`) is rejected at parse
	// time, but the resolver still stamps ResolvedId on any
	// declaration it manages to canonicalize. Pick that up first.
	// Form B (`use cognition.concepts.{ space, participant }`)
	// leaves ResolvedId empty -- the binding comes from one of the
	// fallbacks below.
	if legacyBindingEligible && len(file.Uses) == 1 && file.Uses[0].ResolvedId != "" {
		boundConcept = file.Uses[0].ResolvedId
	}
	// Form B with a single imported Name: resolve that name through
	// the registry. Procedural queries with bare `concept;` rely on
	// this when the file imports exactly one concept.
	if legacyBindingEligible && boundConcept == "" && registry != nil && len(file.Uses) == 1 && len(file.Uses[0].Names) == 1 {
		resolver := NewConceptResolver(registry)
		nsHint := ""
		if len(file.Uses[0].Parts) > 0 {
			nsHint = file.Uses[0].Parts[0]
		}
		if id, err := resolver.resolveBareConceptNameWithNamespace(file.Uses[0].Names[0], nsHint); err == nil {
			boundConcept = id
		}
	}
	// Post-PR-48 canonical shape: the concept binding lives in the
	// signature (`mutation <Concept> <name> { ... }`). When no Form A
	// `use` directive is present, resolve the signature concept via
	// the registry's trailing-segment match.
	if boundConcept == "" && registry != nil && len(signatureConcepts) == 1 {
		resolver := NewConceptResolver(registry)
		// An EXPLICIT import wins and is not scope-checked: naming the
		// namespace is exactly how an author reaches a foreign one.
		imported := namespaceHintForName(file.Uses, signatureConcepts[0])
		nsHint := imported
		if nsHint == "" {
			// AMBIENT same-domain scope (#2617), corrected in memql#3084.
			//
			// Two defects lived here, the same pair #3026 removed from the
			// canonicalId path in this file:
			//
			//  1. the hint came from DomainFromFilePath -- the LAST path
			//     segment -- while boot assembles ids from the FIRST, so a
			//     nested `agents/tools/*.memql` was hinted `tools`;
			//  2. nothing checked that the resolved id was one this file's
			//     domain could have DECLARED, so the hint was a preference
			//     rather than a bound.
			//
			// Together they let a nested file bind a foreign `v1:tools:widget`
			// with no import. Latent in-tree (nothing assembles under
			// v1:tools / v1:roles / v1:skills) and live as soon as a
			// MEMQL_DSL_PATH bundle claims one of those domain names.
			nsHint = RootDomainFromFilePath(origin)
		}
		if id, err := resolver.resolveBareConceptNameWithNamespace(signatureConcepts[0], nsHint); err == nil {
			// The AMBIENT bind is gated; an imported one is not. The gate is
			// the exact inverse of assembly (idIsInDomainAmbientScope, #3080),
			// so it admits precisely the namespaces this file could have
			// declared under -- its directory, a colon-extension of it, or its
			// namespace.pin -- and nothing else.
			if imported != "" || idIsInDomainAmbientScope(id, RootDomainFromFilePath(origin), declaredNamespaceForOrigin(origin)) {
				boundConcept = id
			}
		}
	}

	// Enforce single-use rule for queries and mutations -- LEGACY form
	// only. In the legacy single-construct file layout, each query or
	// mutation operated on at most one concept declared via its single
	// `use cognition.concepts.{ space }` declaration; multiple uses were
	// a parse error. The post-PR-48 canonical shape puts the concept
	// binding in the construct signature (`mutation <Concept> <name>`),
	// and the multi-construct file layout (per `function_slices.go`
	// header) prepends every file-top use declaration to every emitted
	// slice so the slice parser has the imports it needs.
	//
	// Skip the check when the construct has a signature-bound concept;
	// fall through to it only for legacy procedural-form queries /
	// mutations whose single use *was* the concept binding.
	if len(signatureConcepts) == 0 && len(file.Definitions) > 0 {
		if fd, ok := file.Definitions[0].(*languageParser.FunctionDef); ok {
			switch fd.Type {
			case languageParser.FunctionTypeQuery, languageParser.FunctionTypeMutation:
				if len(file.Uses) > 1 {
					return nil, fmt.Errorf("function %q: query/mutation functions must have at most one use declaration (found %d)", expectedName, len(file.Uses))
				}
			}
		}
	}

	// Validate file composition (CQS rules)
	if err := compiler.ValidateFileComposition(file); err != nil {
		return nil, fmt.Errorf("file composition error: %w", err)
	}

	// CQS call-graph enforcement (Phase 2 hoist: was compiler-only).
	// Catches in-file violations:
	//   - Query   -> Mutation : forbidden (read path side-effect free)
	//   - Mutation -> Mutation: forbidden (one observable write per body)
	//   - Spec     -> Mutation: forbidden (predicates are read-only)
	// Cross-file CQS (caller in file A, callee in file B) is enforced
	// by ValidateCQSAcrossRegistry after all files have been loaded.
	if err := compiler.ValidateCQS(collectFunctionDefsFromFile(file)); err != nil {
		return nil, fmt.Errorf("CQS violation in %s: %w", origin, err)
	}

	// Check for automations in named-function directories (should be in
	// automations/). The check applies only when this entry point was
	// invoked for a non-automation construct -- the unified loader and
	// the per-construct parsers route automation slices here with
	// expectedKind="automation" to share the rewrite + AST conversion
	// machinery, so legitimate automation sources must not be rejected.
	if !strings.EqualFold(expectedKind, "automation") {
		comp := compiler.AnalyzeComposition(file)
		if comp.Automations > 0 {
			return nil, fmt.Errorf("automation definitions should be in automations/ directory, not query/mutation files")
		}
	}

	// Get the primary definition (mutation or first query)
	def := file.Definitions[0]
	funcDef, ok := def.(*languageParser.FunctionDef)
	if !ok {
		return nil, fmt.Errorf("expected FunctionDef, got %T", def)
	}

	// Find the primary definition (mutation takes precedence over queries)
	for _, d := range file.Definitions {
		if fd, ok := d.(*languageParser.FunctionDef); ok {
			if fd.Type == languageParser.FunctionTypeMutation {
				funcDef = fd
				break
			}
		}
	}

	if err := validateStrictFunctionContract(funcDef, expectedName, content); err != nil {
		return nil, err
	}

	// Naming-prefix enforcement is RETIRED (DSL grammar redesign epic
	// #2031, C2/#2042). Construct names are now free (a query can be
	// `byId`, a mutation `create`); references resolve structurally by
	// slot keyword + enclosing concept. Correctness is enforced by the
	// dependency-tree validator (ValidateDependencyTree, C3/#2043) at
	// engine load time instead of by a name prefix.

	// Declared-must-be-used enforcement (Phase G.3.g pt 4). Every
	// `@use*(target)` annotation and every declared args field must be
	// referenced in the body somewhere; stale declarations error here.
	if err := validateDeclaredUsage(rawSourceForUsage, funcDef); err != nil {
		return nil, err
	}

	// First-class event-context binding (memql#1706): a Logic that reads the
	// triggering event must declare `event` as an input so the runner threads
	// it into every nested step's argument-resolution scope. The inverse of
	// the declared-usage Logic exemption above -- declared-but-unused `event`
	// is fine (cron logics), used-but-undeclared `event` is a hard error.
	if err := validateLogicEventBinding(rawSourceForUsage, funcDef); err != nil {
		return nil, err
	}

	// Actor-binding enforcement (#2621): reading the auth envelope is a
	// declared capability -- `actor.*` in the body requires `@actor` in
	// the preamble, exactly the used-requires-declared shape of the
	// event-binding rule above. Landing in this chain means strict boot
	// and memqllint's engine-parity tier inherit it with no lint-side
	// changes.
	if err := validateActorBinding(rawSourceForUsage, funcDef); err != nil {
		return nil, err
	}

	// Closed-set member validation (#2625): an unknown actor member was
	// a runtime error on two surfaces and a SILENT NIL on the other two;
	// it is a load error on all four now.
	if err := validateActorMemberPaths(rawSourceForUsage, funcDef); err != nil {
		return nil, err
	}

	// Field-level event-schema validation (memql#1743): a Logic that OPTS IN by
	// declaring `@eventField(...)` has every `event.payload.<field>` reference
	// checked against that declared schema -- rejecting typos / fields the
	// (possibly synthetic) triggering event cannot carry. Opt-in, so unannotated
	// logics are unaffected (zero false-rejects).
	if err := validateLogicEventFields(rawSourceForUsage, funcDef); err != nil {
		return nil, err
	}

	// Arithmetic-operand parity (#2542): reject the unparenthesized-comparison
	// trap (`a - b > 0` -> `a - (b > 0)`) across the WHOLE logic body, including
	// intermediate `:=` steps the loader otherwise stashes un-converted. The
	// terminal-return position is already covered by convertArithmeticExpr; this
	// extends the same clear rejection to every step so it fires through the
	// lint/boot-parity pass, not just at RunLogic.
	if err := validateLogicArithmeticOperands(funcDef); err != nil {
		return nil, err
	}
	if err := validateLogicCondBareIdentifierPredicate(funcDef); err != nil {
		return nil, err
	}
	if err := validateLogicCondBranchValues(funcDef); err != nil {
		return nil, err
	}

	// Validate the function name matches the file-derived function name.
	if funcDef.Name != "" && funcDef.Name != expectedName {
		return nil, fmt.Errorf("function name %q does not match expected name %q", funcDef.Name, expectedName)
	}
	if expectedKind != "" && !strings.EqualFold(string(funcDef.Type), expectedKind) {
		return nil, fmt.Errorf("function %q kind %q does not match expected kind %q from filename", expectedName, funcDef.Type, expectedKind)
	}

	// Convert to Function struct
	fn := &Function{
		Name:         expectedName,
		Description:  languageParser.EffectiveDescription(funcDef.DocComment, funcDef.Description),
		DocComment:   funcDef.DocComment,
		FunctionKind: string(funcDef.Type),
		BoundConcept: boundConcept,
		Origin:       origin,
		// Attribute values
		Enabled:    funcDef.Enabled,
		Deprecated: funcDef.Deprecated,
		Version:    funcDef.Version,
		Timeout:    funcDef.Timeout,
		CacheTTL:   funcDef.CacheTTL,
		Retry:      funcDef.Retry,
		Idempotent: funcDef.Idempotent,
		Audit:      funcDef.Audit,
		// @mcp promotion (epic memql#1529 Phase 4 #1534) is a flag attribute the
		// langparser surfaces on Attributes rather than a typed FunctionDef field.
		MCPPromoted: hasFlagAttribute(funcDef.Attributes, "mcp"),
		// @serverOnly (memql#2800) -- same flag-attribute shape. Bars the
		// construct from client-originated calls while leaving server-side Go
		// free to call it; see auth.CallOrigin for why that distinction needed
		// a new signal rather than the retired @internal.
		ServerOnly: hasFlagAttribute(funcDef.Attributes, "serverOnly"),
	}

	// Handle rate limit
	if funcDef.RateLimit != nil {
		fn.RateLimitRequests = funcDef.RateLimit.Requests
		fn.RateLimitPer = funcDef.RateLimit.Per
	}

	// Handle args assertions populated from the function's args block.
	if funcDef.ArgsSchema != nil {
		schema, err := convertArgsSchema(funcDef.ArgsSchema)
		if err != nil {
			return nil, fmt.Errorf("function %q args schema: %w", expectedName, err)
		}
		fn.ArgsSchema = schema
		markSecretArgsFields(fn.ArgsSchema, boundConcept, registry)
	}

	// Extract and convert expression from the body
	// The parser produces languageParser.ExpressionNode types, but the executor expects
	// memql.ExpressionNode types. We use the AST converter to bridge this gap.
	if funcDef.Body != nil {
		switch funcDef.Type {
		case languageParser.FunctionTypeMutation:
			// Mutation functions: parse the mutation statement into a runtime template.
			stmt, ok := funcDef.Body.(*languageParser.MutationStmt)
			if !ok || stmt == nil {
				return nil, fmt.Errorf("function %q mutation body must be an insert() statement, got %T", expectedName, funcDef.Body)
			}

			payloadObj, err := parsePayloadRawToTemplate(stmt.PayloadRaw)
			if err != nil {
				return nil, fmt.Errorf("function %q: parse payload: %w", expectedName, err)
			}

			// C5 (memql#2035): a caller-supplied arg can never write a
			// field the concept marks @internal or @serverSet -- those
			// are server-only / server-stamped. This gate makes the
			// accept/stamp sugar safe (an `accept { internalField }`
			// desugars to `internalField: args.internalField`, which is
			// rejected here) AND catches a hand-written `insert` that
			// binds a sensitive field straight from caller args. The
			// concept is resolved from the signature/use binding; an
			// unannotated concept (today's whole tree) has no sensitive
			// fields, so this is a no-op until concepts adopt the
			// annotations.
			sensitiveConcept := stmt.Concept
			if sensitiveConcept == "" {
				sensitiveConcept = boundConcept
			}
			if err := validateMutationCallerArgs(registry, sensitiveConcept, expectedName, payloadObj); err != nil {
				return nil, err
			}

			mergeFields, err := mutationMergeFields(funcDef, stmt.Kind)
			if err != nil {
				return nil, fmt.Errorf("function %q: %w", expectedName, err)
			}

			appendFields, err := mutationAppendFields(funcDef, stmt.Kind)
			if err != nil {
				return nil, fmt.Errorf("function %q: %w", expectedName, err)
			}

			createOnlyFields, err := mutationCreateOnlyFields(funcDef, stmt.Kind)
			if err != nil {
				return nil, fmt.Errorf("function %q: %w", expectedName, err)
			}

			scrubPii, err := mutationScrubPii(funcDef, stmt.Kind)
			if err != nil {
				return nil, fmt.Errorf("function %q: %w", expectedName, err)
			}

			// Handle object-literal syntax: insert("concept", { id: ..., payload: {...} })
			// If the payloadObj includes an id or payload key, normalize them.
			var idTemplate any = stmt.IDTemplate
			var createdAtTemplate any = stmt.CreatedAtTemplate
			var payloadTemplate any = payloadObj
			var payloadOverlay map[string]any
			if payloadObj != nil {
				if idVal, ok := payloadObj["id"]; ok && idTemplate == nil {
					idTemplate = idVal
				}
				if createdAtVal, ok := payloadObj["createdAt"]; ok && createdAtTemplate == nil {
					createdAtTemplate = createdAtVal
				}
				payloadVal, hasPayloadKey := payloadObj["payload"]
				if hasPayloadKey {
					// payload can itself be an expression (e.g., args.payload) that evaluates to an object at runtime.
					payloadTemplate = payloadVal
				}
				// Remove id if it was embedded inside the object literal.
				delete(payloadObj, "id")
				delete(payloadObj, "createdAt")
				delete(payloadObj, "payload")
				// If we didn't have an explicit payload wrapper, payloadTemplate remains the entire object.
				if payloadTemplate == nil {
					payloadTemplate = payloadObj
				} else if hasPayloadKey && len(payloadObj) > 0 {
					// The insert block mixed `args.payload` (the splat) with
					// explicit fields like `ownerUserId: actor.userId`. Keep
					// the explicit fields as an overlay -- renderMutationTemplate
					// evaluates the splat first, then overlays these, so an
					// authz-relevant server-side stamp wins over any caller-
					// supplied value in the splat payload (memql#401). Without
					// this branch the explicit fields were silently dropped.
					payloadOverlay = payloadObj
				}
			}

			parentTemplate := stmt.ParentTemplate
			aliasOfTemplate := stmt.AliasOfTemplate

			// If the mutation body didn't specify a concept (implicit from use),
			// fill it from BoundConcept.
			mutationConcept := stmt.Concept
			if mutationConcept == "" && boundConcept != "" {
				mutationConcept = boundConcept
			}
			fn.MutationTemplate = &FunctionMutationTemplate{
				Kind:                   stmt.Kind,
				Concept:                mutationConcept,
				IDTemplate:             idTemplate,
				CreatedAtTemplate:      createdAtTemplate,
				PayloadTemplate:        payloadTemplate,
				PayloadOverlayTemplate: payloadOverlay,
				ParentTemplate:         parentTemplate,
				AliasOfTemplate:        aliasOfTemplate,
				MergeFields:            mergeFields,
				AppendFields:           appendFields,
				CreateOnlyFields:       createOnlyFields,
				ScrubPii:               scrubPii,
			}
			fn.ExprSource = extractExpressionFromContent(content)

		case languageParser.FunctionTypeLogic:
			// Logic functions: the parser produces an *AutomationDef body
			// (a sequence of `name := <call>` steps plus a synthetic
			// `_return` step). For single-statement bodies (`body { return
			// <expr> }`) the AutomationDef has exactly one `_return` step
			// whose Query is the expression — we extract it as fn.Expr so
			// the standard expression evaluator runs the call directly.
			//
			// Multi-statement Logic bodies (with intermediate `:=` steps)
			// are not yet executed end-to-end through this path: the
			// intermediate steps' side effects (mutations, publishEvent)
			// would be lost. Those logics need a step-runner-backed
			// invocation flow on the engine side; tracked as a follow-up.
			if auto, ok := funcDef.Body.(*languageParser.AutomationDef); ok {
				// Always extract the `_return` expression as fn.Expr.
				// Single-statement bodies (no intermediate `name := <call>`
				// steps) run through the standard expression evaluator
				// via this path. Multi-step bodies ALSO keep fn.Expr set
				// so callers that aren't routing through the LogicRunner
				// (e.g. validation paths) still see something callable;
				// the engine's dispatch path checks LogicSteps first.
				retExpr, err := extractLogicReturnExpression(auto)
				if err != nil {
					return nil, fmt.Errorf("function %q: %w", expectedName, err)
				}
				// Logic bodies admit the Story 4 collection-method + lambda
				// surface (ADR §2.2). Specs and query filters use the default
				// converter, which rejects it.
				converter := NewASTConverter(WithCollectionMethods())
				engineExpr, err := converter.ConvertExpression(retExpr)
				if err != nil {
					return nil, fmt.Errorf("convert function %q body: %w", expectedName, err)
				}
				if boundConcept != "" {
					engineExpr = resolveBareConcept(engineExpr, boundConcept)
				}
				fn.Expr = engineExpr
				fn.ExprSource = extractExpressionFromContent(content)
				// F.5: when the body has intermediate steps, stash the
				// full AutomationDef on the function so the engine can
				// dispatch through the wired LogicRunner. The runner
				// walks the intermediate steps in order, binds each
				// result for later step references, and evaluates the
				// `_return` expression as the function's return.
				if nonReturnStepCount(auto.Steps) > 0 {
					fn.LogicSteps = auto
				}
			} else {
				return nil, fmt.Errorf("function %q logic body must be a procedural block, got %T", expectedName, funcDef.Body)
			}

		default:
			// Query functions: convert the expression AST to executable engine AST.
			if parserExpr, ok := funcDef.Body.(languageParser.ExpressionNode); ok {
				converter := NewASTConverter()
				engineExpr, err := converter.ConvertExpression(parserExpr)
				if err != nil {
					return nil, fmt.Errorf("convert function %q body: %w", expectedName, err)
				}
				// Resolve bare `concept` keyword: if the expression (or
				// any sub-expression) is a SpecReferenceExpression with
				// Name "concept", replace it with a ComparisonExpression
				// that binds to the function's use-declared concept.
				if boundConcept != "" {
					engineExpr = resolveBareConcept(engineExpr, boundConcept)
				}
				// For queries bound to a single concept via @useConcept,
				// fold an implicit `concept == boundConcept` into the
				// filter when the author hasn't written one explicitly.
				// Without this, `magicLinkRequest.tokenHash == ...` gets
				// translated to `payload.tokenHash == ...` before the
				// engine sees it, leaving no concept handle for
				// extractConceptFromExpression -> partitionForConcept to
				// latch onto. Global-scope concepts then route to the
				// envelope's partition (typically "default") instead of
				// "_system" where their rows live, and the read silently
				// returns nothing. Mutations are unaffected: their
				// executor reads BoundConcept directly off the function.
				if boundConcept != "" && funcDef.Type == languageParser.FunctionTypeQuery {
					engineExpr = ensureBoundConceptFilter(engineExpr, boundConcept)
				}
				// 5.5: turn the struct-form `@cache(ttl=N)` annotation
				// (parsed into fn.CacheTTL) into the per-plan cache hint the
				// engine already consumes. The result-cache hint mechanism is
				// keyed off `CacheHintSeconds` on the `concept==X` comparison
				// node (collectCacheHints), so we stamp the TTL there. Without
				// this, `@cache(ttl=N)` on a struct query is parsed but
				// silently dropped -- exactly the dormant-cache state 5.5
				// turns on. `@cache(ttl=0)` stamps 0, which cacheTTLForBundle
				// reads as the explicit "never cache" escape.
				if funcDef.Type == languageParser.FunctionTypeQuery && strings.TrimSpace(fn.CacheTTL) != "" {
					if err := applyQueryCacheTTL(engineExpr, fn.CacheTTL); err != nil {
						return nil, fmt.Errorf("function %q cache ttl: %w", expectedName, err)
					}
				}
				fn.Expr = engineExpr
				fn.ExprSource = extractExpressionFromContent(content)

				// Temporal-access visibility (core-builtins ADR §2.3,
				// story memql#2305). A query that reads `asOf latest`
				// returns clock-dependent (non-reproducible) data, so
				// its loaded contract is marked time-dependent. An
				// `asOf <explicit timestamp>` is deterministic and is
				// NOT marked. The flag is auto-derived from the clause
				// (authoritative -- cannot drift from the body) and may
				// also be asserted by an explicit `@latestMode`
				// annotation (the author-facing contract surface).
				if funcDef.Type == languageParser.FunctionTypeQuery {
					fn.LatestMode = queryLatestMode(engineExpr) || hasFlagAttribute(funcDef.Attributes, "latestMode")
				}
			} else {
				return nil, fmt.Errorf("function %q body is not an expression node: %T", expectedName, funcDef.Body)
			}
		}
	} else {
		slog.Warn("function definition body is nil", "expectedName", expectedName)
	}

	return fn, nil
}

// signatureConceptRe matches the canonical post-PR-48 struct-form
// signature `<kind> <Concept> <name> {` and captures the concept
// bare name. Used to feed the per-name payload-translation pipeline
// that rewrites `<Concept>.X` references in the construct body to
// `payload.X` before the expression parser tokenises.
//
// The mutation declaration keyword is the C1 (memql#2041) verb `mutate`;
// it binds the signature concept the same way query/shape/seed do. The
// transitional `mutation` noun alias was dropped by C6 (memql#2036).
var signatureConceptRe = regexp.MustCompile(
	`(?m)^[ \t]*(?:query|mutate|shape|seed)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{`,
)

// extractAllSignatureConceptNames scans raw .memql source for every
// signature-bound concept binding (`mutation <Concept> <name> {`,
// `query <Concept> <name> {`, etc.) and returns the distinct concept
// names in declaration order.
func extractAllSignatureConceptNames(source string) []string {
	// Scan a comment-blanked view so a `<kind> <Concept> <name> {`
	// shape inside a `//` / `/* */` comment isn't captured as a real
	// signature binding (memql#1074).
	matches := signatureConceptRe.FindAllStringSubmatch(languageParser.BlankComments(source), -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		name := m[1]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// translateSignatureConceptPathsToPayload rewrites every
// `\b<bareName>\.` occurrence in `source` to `payload.`. The
// signature itself (`mutation <Concept> <name>`) is preserved
// because the bare name is followed by a space and the construct
// name, not a `.`, so the regex doesn't match it.
func translateSignatureConceptPathsToPayload(source, bareName string) string {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(bareName) + `\.`)
	return re.ReplaceAllString(source, "payload.")
}

// ensureBoundConceptFilter returns expr AND'd with a
// `concept == boundConcept` comparison, unless the expression
// already contains that same equality somewhere in an AND chain.
// A nil input returns the bare comparison. The wrap makes the
// implicit concept binding from @useConcept explicit in the AST so
// downstream stages (concept-extraction for partition routing,
// SQL concept filter, post-filter validation) all see the same
// concept handle the author intended.
//
// Directive wrappers (shape, sort, paginate, select, timestamp,
// depth) must be the outermost nodes in a query expression --
// parser.go's planQuery enforces this. Descending into the
// wrapper's Target keeps the directive on the outside and lands
// the AND on the inner filter where it belongs.
// applyQueryCacheTTL stamps the query function's `@cache(ttl=N)` TTL
// (N seconds) onto the `concept==X` comparison node(s) of its compiled
// expression. The engine's result-cache adoption (5.5) reads the TTL
// back out via collectCacheHints, which only inspects CacheHintSeconds
// on concept-equality comparisons -- so stamping it here is the single
// wiring point that makes `@cache(ttl=N)` on a struct query take effect.
//
// ttlRaw is the raw annotation string (`@cache(ttl=30)` -> "30",
// `@cache(0)` -> "0"); it must be a non-negative integer count of
// seconds. A value of 0 is preserved verbatim (the explicit "never
// cache" escape cacheTTLForBundle recognises).
func applyQueryCacheTTL(expr ExpressionNode, ttlRaw string) error {
	seconds, err := strconv.Atoi(strings.TrimSpace(ttlRaw))
	if err != nil {
		return fmt.Errorf("invalid @cache(ttl=%q): expected a whole number of seconds", ttlRaw)
	}
	if seconds < 0 {
		return fmt.Errorf("invalid @cache(ttl=%d): seconds must be >= 0", seconds)
	}
	if !stampConceptCacheHint(expr, seconds) {
		return fmt.Errorf("@cache requires a `concept==...` filter to attach to; none found")
	}
	return nil
}

// stampConceptCacheHint walks expr and sets CacheHintSeconds on every
// `concept==X` equality comparison it finds, returning whether at least
// one was stamped. It descends through the directive wrappers and AND/OR
// chains the same way collectCacheHints reads them, so the hint lands on
// the node the engine later inspects.
func stampConceptCacheHint(expr ExpressionNode, seconds int) bool {
	switch n := expr.(type) {
	case nil:
		return false
	case *ComparisonExpression:
		if n.Operator == OpEq && n.Field.Raw == "concept" {
			s := seconds
			n.CacheHintSeconds = &s
			return true
		}
		return false
	case *LogicalExpression:
		left := stampConceptCacheHint(n.Left, seconds)
		right := stampConceptCacheHint(n.Right, seconds)
		return left || right
	case *RelationshipExpression:
		return stampConceptCacheHint(n.Target, seconds)
	case *SortExpression:
		return stampConceptCacheHint(n.Target, seconds)
	case *PaginateExpression:
		return stampConceptCacheHint(n.Target, seconds)
	case *SelectExpression:
		return stampConceptCacheHint(n.Target, seconds)
	case *TimestampExpression:
		return stampConceptCacheHint(n.Target, seconds)
	case *DepthExpression:
		return stampConceptCacheHint(n.Target, seconds)
	case *CountExpression:
		return stampConceptCacheHint(n.Target, seconds)
	case *ShapeExpression:
		return stampConceptCacheHint(n.Target, seconds)
	default:
		return false
	}
}

func ensureBoundConceptFilter(expr ExpressionNode, boundConcept string) ExpressionNode {
	switch n := expr.(type) {
	case *ShapeExpression:
		n.Target = ensureBoundConceptFilter(n.Target, boundConcept)
		return n
	case *SortExpression:
		n.Target = ensureBoundConceptFilter(n.Target, boundConcept)
		return n
	case *PaginateExpression:
		n.Target = ensureBoundConceptFilter(n.Target, boundConcept)
		return n
	case *SelectExpression:
		n.Target = ensureBoundConceptFilter(n.Target, boundConcept)
		return n
	case *TimestampExpression:
		n.Target = ensureBoundConceptFilter(n.Target, boundConcept)
		return n
	case *DepthExpression:
		n.Target = ensureBoundConceptFilter(n.Target, boundConcept)
		return n
	case *CountExpression:
		n.Target = ensureBoundConceptFilter(n.Target, boundConcept)
		return n
	}
	conceptCmp := &ComparisonExpression{
		Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
		Operator: OpEq,
		Value:    boundConcept,
	}
	if expr == nil {
		return conceptCmp
	}
	if containsBoundConceptEquality(expr, boundConcept) {
		return expr
	}
	return &LogicalExpression{Op: LogicalAnd, Left: expr, Right: conceptCmp}
}

// containsBoundConceptEquality reports whether expr (or any AND-leaf
// descendant) already binds `concept == boundConcept`. Only AND
// chains are walked: an OR branch can't be relied on to constrain
// the concept on every path, so we treat it as "not present" and
// add our own AND.
func containsBoundConceptEquality(expr ExpressionNode, boundConcept string) bool {
	switch n := expr.(type) {
	case nil:
		return false
	case *ComparisonExpression:
		if n == nil || n.Operator != OpEq || n.Field.Raw != "concept" {
			return false
		}
		v, ok := n.Value.(string)
		return ok && v == boundConcept
	case *LogicalExpression:
		if n == nil || n.Op != LogicalAnd {
			return false
		}
		return containsBoundConceptEquality(n.Left, boundConcept) ||
			containsBoundConceptEquality(n.Right, boundConcept)
	default:
		return false
	}
}

// resolveBareConcept walks an expression tree and replaces any bare
// `concept` reference (a SpecReferenceExpression with Name "concept")
// with a ComparisonExpression{concept == boundConcept}. This supports
// the implicit concept binding syntax where `use cluster.node` +
// `return concept, nil` is equivalent to `return concept==v1:cluster:node, nil`.
func resolveBareConcept(expr ExpressionNode, boundConcept string) ExpressionNode {
	if expr == nil {
		return nil
	}
	switch n := expr.(type) {
	case *SpecReferenceExpression:
		if n.Name == "concept" {
			return &ComparisonExpression{
				Field: FieldReference{
					Raw:   "concept",
					Parts: []string{"concept"},
				},
				Operator: OpEq,
				Value:    boundConcept,
			}
		}
		return n
	case *LogicalExpression:
		return &LogicalExpression{
			Op:    n.Op,
			Left:  resolveBareConcept(n.Left, boundConcept),
			Right: resolveBareConcept(n.Right, boundConcept),
		}
	case *RelationshipExpression:
		return &RelationshipExpression{
			Function: n.Function,
			Target:   resolveBareConcept(n.Target, boundConcept),
		}
	case *SortExpression:
		return &SortExpression{
			Fields: n.Fields,
			Target: resolveBareConcept(n.Target, boundConcept),
		}
	case *PaginateExpression:
		return &PaginateExpression{
			Limit:  n.Limit,
			Target: resolveBareConcept(n.Target, boundConcept),
		}
	case *ShapeExpression:
		return &ShapeExpression{
			Target:        resolveBareConcept(n.Target, boundConcept),
			Template:      n.Template,
			TemplateName:  n.TemplateName,
			IncludeBundle: n.IncludeBundle,
		}
	case *CountExpression:
		return &CountExpression{
			Target: resolveBareConcept(n.Target, boundConcept),
		}
	default:
		return expr
	}
}

// extractExpressionFromContent extracts the expression part from a function definition.
func extractExpressionFromContent(content string) string {
	// Find the content between { and the last }
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start >= 0 && end > start {
		return strings.TrimSpace(content[start+1 : end])
	}
	return content
}

func validateStrictFunctionContract(funcDef *languageParser.FunctionDef, expectedName, content string) error {
	if funcDef == nil {
		return fmt.Errorf("function %q: definition is nil", expectedName)
	}

	switch funcDef.Type {
	case languageParser.FunctionTypeQuery:
		if len(funcDef.Args) != 1 || !strings.EqualFold(strings.TrimSpace(funcDef.Args[0].Type), "any") {
			return fmt.Errorf("function %q: query signature must be (args any)", expectedName)
		}
		if len(funcDef.Returns) != 2 ||
			!strings.EqualFold(strings.TrimSpace(funcDef.Returns[0]), "any") ||
			!strings.EqualFold(strings.TrimSpace(funcDef.Returns[1]), "error") {
			return fmt.Errorf("function %q: query return signature must be (any, error)", expectedName)
		}
		if !functionBodyStartsWithReturn(content) {
			return fmt.Errorf("function %q: query body must start with 'return'", expectedName)
		}
	case languageParser.FunctionTypeMutation:
		if len(funcDef.Args) != 1 || !strings.EqualFold(strings.TrimSpace(funcDef.Args[0].Type), "any") {
			return fmt.Errorf("function %q: mutation signature must be (args any)", expectedName)
		}
		if len(funcDef.Returns) != 1 || !strings.EqualFold(strings.TrimSpace(funcDef.Returns[0]), "error") {
			return fmt.Errorf("function %q: mutation return signature must be error", expectedName)
		}
		if !functionBodyStartsWithReturn(content) {
			return fmt.Errorf("function %q: mutation body must start with 'return'", expectedName)
		}
	}

	return nil
}

func functionBodyStartsWithReturn(content string) bool {
	// Locate the procedural `func (...)` header on a comment-blanked
	// view so a `func (Query)` token inside a `//` / `/* */` comment in
	// the construct's leading docblock isn't mistaken for the real
	// header (memql#1074). Offsets are preserved, so the indices index
	// straight back into the original `content`.
	scan := languageParser.BlankComments(content)
	funcIdx := strings.Index(scan, "func ")
	if funcIdx < 0 {
		return false
	}
	start := strings.Index(scan[funcIdx:], "{")
	if start < 0 {
		return false
	}
	start += funcIdx
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return false
	}
	body := strings.TrimSpace(content[start+1 : end])

	// Skip leading comments and blank lines to find the first
	// statement. The canonical procedural body starts with `return
	// <expr>`. memql#302 retired the legacy `ctx.output = <expr>;
	// return ctx, nil` envelope shape; that branch is gone.
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.HasPrefix(trimmed, "return ") {
			return true
		}
		return false
	}

	return false
}

// collectFunctionDefsFromFile filters a parsed *File down to the
// function definitions it contains. Used by the per-file CQS
// validator hoist in tryParseNewFunctionSyntax.
// hasFlagAttribute reports whether a flag-style `@name` attribute is present on
// a parsed construct. Used for annotations the langparser surfaces only on the
// raw Attributes slice (no typed FunctionDef field) -- e.g. @mcp (Phase 4 #1534).
func hasFlagAttribute(attrs []*languageParser.Attribute, name string) bool {
	for _, a := range attrs {
		if a != nil && a.Name == name {
			return true
		}
	}
	return false
}

func collectFunctionDefsFromFile(file *languageParser.File) []*languageParser.FunctionDef {
	if file == nil {
		return nil
	}
	out := make([]*languageParser.FunctionDef, 0, len(file.Definitions))
	for _, def := range file.Definitions {
		if fn, ok := def.(*languageParser.FunctionDef); ok {
			out = append(out, fn)
		}
	}
	return out
}

// nonReturnStepCount returns the number of steps in the slice whose ID is
// not "_return". Used by the logic-body validator to flag multi-step
// bodies that the function executor doesn't yet run end-to-end.
func nonReturnStepCount(steps []languageParser.StepDef) int {
	n := 0
	for _, s := range steps {
		if s.ID != "_return" {
			n++
		}
	}
	return n
}

// extractLogicReturnExpression returns the expression the logic body
// produces. We look for the synthetic `_return` step that
// parseGoStyleAutomationBody appends for the trailing `return <expr>`
// terminator, and pull the wrapped QueryStepConfig.Query out of it.
func extractLogicReturnExpression(auto *languageParser.AutomationDef) (languageParser.ExpressionNode, error) {
	for _, s := range auto.Steps {
		if s.ID != "_return" {
			continue
		}
		cfg, ok := s.Config.(*languageParser.QueryStepConfig)
		if !ok || cfg == nil || cfg.Query == nil {
			return nil, fmt.Errorf("logic `_return` step is missing its expression")
		}
		return cfg.Query, nil
	}
	return nil, fmt.Errorf("logic body has no trailing `return <expr>` terminator")
}

// validateFunctions (cycle + unknown-reference validation) was dead code with
// zero call sites and was DELETED (S6, memql#2361): the strict-boot posture
// (#2357) + the callgraph contract gates cover the classes it targeted.

// collectFunctionReferences finds all FunctionCallExpression nodes in an expression tree.
func collectFunctionReferences(expr ExpressionNode) []string {
	var refs []string
	collectFunctionRefsRecursive(expr, &refs)
	return refs
}

func collectFunctionRefsRecursive(expr ExpressionNode, refs *[]string) {
	if expr == nil {
		return
	}

	switch node := expr.(type) {
	case *FunctionCallExpression:
		*refs = append(*refs, node.Name)
	case *BuiltinFunctionExpression:
		// Builtin functions are not included in function references
		// as they are handled directly by the executor
	case *LogicalExpression:
		collectFunctionRefsRecursive(node.Left, refs)
		collectFunctionRefsRecursive(node.Right, refs)
	case *RelationshipExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *SortExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *PaginateExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *SelectExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *TimestampExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *DepthExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *CountExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *ShapeExpression:
		collectFunctionRefsRecursive(node.Target, refs)
	case *ConditionalFilterExpression:
		collectFunctionRefsRecursive(node.Filter, refs)
	case *ArgRefExpression:
		// ArgRefExpression doesn't reference functions
	case *CollectionMethodExpression:
		// Story 4 (#2302): traverse the receiver + args so any function a
		// chain references is seen by unknown-ref + cycle validation.
		collectFunctionRefsRecursive(node.Receiver, refs)
		for _, a := range node.Args {
			collectFunctionRefsRecursive(a, refs)
		}
	case *DotAccessExpression:
		collectFunctionRefsRecursive(node.Object, refs)
	case *LambdaExpression:
		collectFunctionRefsRecursive(node.Body, refs)
	case *ArithmeticExpression:
		collectFunctionRefsRecursive(node.Left, refs)
		collectFunctionRefsRecursive(node.Right, refs)
	case *BinaryComparisonExpression:
		collectFunctionRefsRecursive(node.Left, refs)
		collectFunctionRefsRecursive(node.Right, refs)
	}
}

// enforceLambdaPurity walks every loaded function body for collection
// lambdas (Story 4 / #2302 / ADR §2.2) and rejects a lambda whose body
// calls a mutation or action. Lambda bodies must be pure: they only read
// fields and compute values, never mutate state. Runs in the post-load
// validation pass where the full registry is available so a call name can
// be classified.
func enforceLambdaPurity(functions map[string]*Function) error {
	for name, fn := range functions {
		if fn == nil || fn.Expr == nil {
			continue
		}
		if err := walkForImpureLambda(fn.Expr, functions); err != nil {
			return fmt.Errorf("logic %q: %w", name, err)
		}
	}
	return nil
}

// walkForImpureLambda finds LambdaExpression nodes and validates their
// bodies contain no mutation/action call.
func walkForImpureLambda(expr ExpressionNode, functions map[string]*Function) error {
	switch node := expr.(type) {
	case nil:
		return nil
	case *CollectionMethodExpression:
		if err := walkForImpureLambda(node.Receiver, functions); err != nil {
			return err
		}
		for _, a := range node.Args {
			if err := walkForImpureLambda(a, functions); err != nil {
				return err
			}
		}
	case *DotAccessExpression:
		return walkForImpureLambda(node.Object, functions)
	case *LambdaExpression:
		if bad := findImpureCall(node.Body, functions); bad != "" {
			return fmt.Errorf("lambda body must be pure: mutation/action %q is not allowed inside a collection lambda (ADR §2.2)", bad)
		}
		return walkForImpureLambda(node.Body, functions)
	case *LogicalExpression:
		if err := walkForImpureLambda(node.Left, functions); err != nil {
			return err
		}
		return walkForImpureLambda(node.Right, functions)
	case *BinaryComparisonExpression:
		// An expression-led comparison operand can itself carry a collection
		// chain with a lambda (`rows.any(r => mutate()) == true`); traverse
		// both sides so the purity check reaches a nested lambda.
		if err := walkForImpureLambda(node.Left, functions); err != nil {
			return err
		}
		return walkForImpureLambda(node.Right, functions)
	case *CountExpression:
		return walkForImpureLambda(node.Target, functions)
	case *ShapeExpression:
		return walkForImpureLambda(node.Target, functions)
	}
	return nil
}

// findImpureCall returns the name of the first mutation/action call found
// anywhere in expr, or "" when the body is pure.
func findImpureCall(expr ExpressionNode, functions map[string]*Function) string {
	switch node := expr.(type) {
	case nil:
		return ""
	case *FunctionCallExpression:
		if isMutationOrActionName(node.Name, functions) {
			return node.Name
		}
		return ""
	case *BuiltinFunctionExpression:
		if isMutationOrActionName(node.Name, functions) {
			return node.Name
		}
		return ""
	case *LogicalExpression:
		if bad := findImpureCall(node.Left, functions); bad != "" {
			return bad
		}
		return findImpureCall(node.Right, functions)
	case *CollectionMethodExpression:
		if bad := findImpureCall(node.Receiver, functions); bad != "" {
			return bad
		}
		for _, a := range node.Args {
			if bad := findImpureCall(a, functions); bad != "" {
				return bad
			}
		}
		return ""
	case *DotAccessExpression:
		return findImpureCall(node.Object, functions)
	case *LambdaExpression:
		return findImpureCall(node.Body, functions)
	case *ArithmeticExpression:
		if bad := findImpureCall(node.Left, functions); bad != "" {
			return bad
		}
		return findImpureCall(node.Right, functions)
	case *BinaryComparisonExpression:
		if bad := findImpureCall(node.Left, functions); bad != "" {
			return bad
		}
		return findImpureCall(node.Right, functions)
	case *ComparisonExpression:
		if v, ok := node.Value.(ExpressionNode); ok {
			return findImpureCall(v, functions)
		}
		return ""
	}
	return ""
}

// isMutationOrActionName reports whether a call name resolves to a mutation
// or action -- either by registry classification or by the conventional
// `mutation`/`mutate` name prefix (a backstop for names not yet in the
// passed registry).
func isMutationOrActionName(name string, functions map[string]*Function) bool {
	key := strings.TrimSpace(name)
	if fn, ok := functions[key]; ok && fn != nil {
		kind := strings.ToLower(strings.TrimSpace(fn.FunctionKind))
		if kind == "mutation" || kind == "action" {
			return true
		}
	}
	lower := strings.ToLower(key)
	return strings.HasPrefix(lower, "mutation") || strings.HasPrefix(lower, "mutate")
}

// detectFunctionCycle uses DFS to detect cycles in the function dependency graph.
func detectFunctionCycle(deps map[string][]string) []string {
	visited := make(map[string]bool)
	recStack := make(map[string]bool)
	path := make([]string, 0)

	var dfs func(name string) []string
	dfs = func(name string) []string {
		visited[name] = true
		recStack[name] = true
		path = append(path, name)

		for _, dep := range deps[name] {
			if !visited[dep] {
				if cycle := dfs(dep); cycle != nil {
					return cycle
				}
			} else if recStack[dep] {
				// Found cycle - find where it starts
				cycleStart := -1
				for i, p := range path {
					if p == dep {
						cycleStart = i
						break
					}
				}
				if cycleStart >= 0 {
					cycle := append(path[cycleStart:], dep)
					return cycle
				}
				return []string{dep, name, dep}
			}
		}

		path = path[:len(path)-1]
		recStack[name] = false
		return nil
	}

	for name := range deps {
		if !visited[name] {
			if cycle := dfs(name); cycle != nil {
				return cycle
			}
		}
	}

	return nil
}

func normalizeBuiltinArgContract(raw *builtinArgContractDefinition) (*BuiltinArgContract, error) {
	if raw == nil {
		return &BuiltinArgContract{Profile: BuiltinArgProfileNone}, nil
	}
	profile := BuiltinArgProfile(strings.TrimSpace(raw.Profile))
	if profile == "" {
		profile = BuiltinArgProfileNone
	}
	switch profile {
	case BuiltinArgProfileNone, BuiltinArgProfileObject, BuiltinArgProfileOptionalObject,
		BuiltinArgProfileStringOrObject, BuiltinArgProfileOptionalString, BuiltinArgProfileOptionalStringOrObject:
	default:
		return nil, fmt.Errorf("unsupported profile %q", raw.Profile)
	}

	contract := &BuiltinArgContract{
		Profile:              profile,
		StringKey:            strings.TrimSpace(raw.StringKey),
		Required:             append([]string(nil), raw.Required...),
		Properties:           nil,
		AdditionalProperties: raw.AdditionalProperties,
	}
	if raw.Properties != nil {
		contract.Properties = make(map[string]string, len(raw.Properties))
		for key, typ := range raw.Properties {
			key = strings.TrimSpace(key)
			typ = strings.TrimSpace(strings.ToLower(typ))
			if key == "" {
				continue
			}
			contract.Properties[key] = typ
		}
	}

	if profile == BuiltinArgProfileStringOrObject || profile == BuiltinArgProfileOptionalString || profile == BuiltinArgProfileOptionalStringOrObject {
		if contract.StringKey == "" {
			return nil, fmt.Errorf("profile %q requires stringKey", profile)
		}
	}
	return contract, nil
}

func reportFunctionError(logger *slog.Logger, origin string, err error) {
	if logger == nil || err == nil {
		return
	}
	logger.Error("function skipped",
		"component", ComponentName,
		"file", filepath.Base(origin),
		"error", err.Error(),
	)
}

// convertArgsSchema converts languageParser.ArgsSchema to memql.ArgsSchemaConfig.
// Returns an error when a field's regex @pattern fails to compile so
// malformed DSL fails loud at function-load time rather than per-call.
func convertArgsSchema(assertDef *languageParser.ArgsSchema) (*ArgsSchemaConfig, error) {
	if assertDef == nil || len(assertDef.Fields) == 0 {
		return nil, nil
	}

	config := &ArgsSchemaConfig{
		Fields:               make([]*FunctionArgsField, len(assertDef.Fields)),
		AdditionalProperties: assertDef.AdditionalProperties,
	}

	for i, field := range assertDef.Fields {
		converted, err := convertArgsField(field)
		if err != nil {
			return nil, err
		}
		config.Fields[i] = converted
	}

	return config, nil
}

// markSecretArgsFields stamps FunctionArgsField.Secret on every args field
// whose name matches a field the bound concept annotates `@secret`, so a
// rejected value on that argument is redacted from the validation error
// message instead of being quoted into it (memql#3036).
//
// Resolved HERE rather than in the validator for the same reason @pattern is
// compiled here: it needs the concept registry, which the loader has and the
// validator deliberately does not -- functionValidator holds only functions +
// specs and is ctx-free by design. Doing it once at load also means the
// per-call path stays a field read.
//
// Nested and array-item fields are stamped by name too. A concept's @secret
// fields are top-level, so an exact name match on a nested field is a
// heuristic -- but it errs toward redacting a message rather than leaking a
// credential, which is the right direction for a diagnostic. It never
// suppresses a validation FAILURE; only the value inside the message.
//
// Fails OPEN, deliberately and visibly: no bound concept, no registry, or a
// concept the registry cannot resolve leaves every field unstamped and the
// messages exactly as they were. The alternative -- refusing to load -- would
// let an unrelated registry gap take down boot over a diagnostic.
func markSecretArgsFields(schema *ArgsSchemaConfig, boundConcept string, registry memoryNodes.Registry) {
	if schema == nil || len(schema.Fields) == 0 || boundConcept == "" || registry == nil {
		return
	}
	concept, err := registry.Get(boundConcept)
	if err != nil || concept == nil {
		return
	}
	secret := concept.SecretFields()
	if len(secret) == 0 {
		return
	}
	names := make(map[string]struct{}, len(secret))
	for _, name := range secret {
		names[name] = struct{}{}
	}
	for _, field := range schema.Fields {
		markSecretArgsField(field, names)
	}
}

// markSecretArgsField stamps one field and its nested / item children.
func markSecretArgsField(field *FunctionArgsField, secretNames map[string]struct{}) {
	if field == nil {
		return
	}
	if _, ok := secretNames[field.Name]; ok {
		field.Secret = true
	}
	for _, nested := range field.Nested {
		markSecretArgsField(nested, secretNames)
	}
	// An array's Items carries the ELEMENT schema and usually has no name of
	// its own, so it inherits the array field's classification: a `[]string`
	// of secrets is a secret in every element.
	if field.Items != nil {
		if field.Secret {
			field.Items.Secret = true
		}
		markSecretArgsField(field.Items, secretNames)
	}
}

// convertArgsField converts languageParser.ArgsField to memql.FunctionArgsField.
// Compiles the @pattern regex once here so the validator can match
// against the cached *regexp.Regexp on every call.
func convertArgsField(field *languageParser.ArgsField) (*FunctionArgsField, error) {
	if field == nil {
		return nil, nil
	}

	result := &FunctionArgsField{
		Name:                 field.Name,
		Description:          field.DocComment,
		Type:                 field.Type,
		Optional:             field.Optional,
		Enum:                 append([]any(nil), field.Enum...),
		Minimum:              field.Minimum,
		Maximum:              field.Maximum,
		Format:               field.Format,
		MaxLength:            field.MaxLength,
		Pattern:              field.Pattern,
		AdditionalProperties: field.AdditionalProperties,
	}

	if field.Pattern != "" {
		compiled, err := regexp.Compile(field.Pattern)
		if err != nil {
			return nil, fmt.Errorf("args field %q: invalid @pattern %q: %w", field.Name, field.Pattern, err)
		}
		result.patternRegex = compiled
	}

	if len(field.Nested) > 0 {
		result.Nested = make([]*FunctionArgsField, len(field.Nested))
		for i, nested := range field.Nested {
			converted, err := convertArgsField(nested)
			if err != nil {
				return nil, err
			}
			result.Nested[i] = converted
		}
	}
	if field.Items != nil {
		converted, err := convertArgsField(field.Items)
		if err != nil {
			return nil, err
		}
		result.Items = converted
	}

	return result, nil
}

// valueReferencesCallerArg reports whether a parsed payload-template
// value is bound to caller-supplied input. The struct-form rewriter
// passes `args.X` references through verbatim; the object-literal
// parser also recognises the legacy `ctx.X` shorthand for the same
// thing. Non-string values (literals, nested objects) are never
// caller-args. Backs the C5 (memql#2035) sensitive-field gate.
func valueReferencesCallerArg(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "args.") || strings.HasPrefix(s, "ctx.")
}

// validateMutationCallerArgs enforces the C5 (memql#2035) invariant
// that a mutation may not write a concept field marked @internal or
// @serverSet from caller args -- those fields are server-only
// (@internal) or server-stamped (@serverSet) and must come from
// `now`/`actor.X`/a literal, never the caller. This makes the
// accept/stamp sugar safe (an `accept { f }` desugars to
// `f: args.f`, rejected here when f is sensitive) and also catches a
// hand-written `insert` that binds a sensitive field from args.
//
// No-op when the concept can't be resolved, declares no sensitive
// fields, or the payload binds none of them from args -- so the whole
// pre-C6 tree (no concept carries the annotations yet) passes
// untouched.
func validateMutationCallerArgs(registry memoryNodes.Registry, conceptName, mutationName string, payload map[string]any) error {
	if registry == nil || len(payload) == 0 {
		return nil
	}
	conceptName = strings.TrimSpace(conceptName)
	if conceptName == "" {
		return nil
	}
	concept, err := registry.Get(conceptName)
	if err != nil || concept == nil {
		return nil
	}
	sensitive := append(concept.InternalFields(), concept.ServerSetFields()...)
	for _, field := range sensitive {
		if valueReferencesCallerArg(payload[field]) {
			return fmt.Errorf("mutation %q writes field %q from caller args, but concept %q marks it @internal or @serverSet (server-only / server-stamped) -- stamp it in `stamp { ... }` from now/actor.X/a literal instead of accepting it from the caller", mutationName, field, conceptName)
		}
	}
	return nil
}
