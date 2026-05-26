package memql

// unified_kinds_loader.go bundles the unified loaders for the five
// kinds with dedicated parsers (shape, provider, tool, builtin,
// prompt). Each loader is a thin wrapper around baseloader.LoadOne /
// LoadMany; the only construct-specific bits are the parser fn and
// the registry callback. The Prompt loader still owns its template-
// sidecar resolution + JSON-schema compilation inline since neither
// step is generic.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"strings"
	"text/template"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// extractAdapter bridges ExtractKeywordSlices to the
// baseloader.Slice contract.
func extractAdapter(content, keyword string) []baseloader.Slice {
	src := ExtractKeywordSlices(content, keyword)
	out := make([]baseloader.Slice, len(src))
	for i, s := range src {
		out[i] = baseloader.Slice{Name: s.Name, Source: s.Source}
	}
	return out
}

// LoadUnifiedShapes walks the new tree, parses every `shape NAME
// { ... }` block through the langparser's load-time path, and upserts
// the resulting ShapeDefinition into the supplied registry.
//
// memql#315 (sub-epic #309 / #306 child C) migrated the parsing
// half off the hand-rolled parseShapeMemQL onto langparser.ParseShapeDecl
// + the in-package shapeDeclToShapeDefinition converter. The
// hand-rolled parser is unreferenced from production code after this
// child; tests in shape_parser_test.go still exercise it pending the
// final deletion in #306 child D (#310).
func LoadUnifiedShapes(logger *slog.Logger, registry *ShapeRegistry) (int, error) {
	if registry == nil {
		return 0, fmt.Errorf("shape registry is nil")
	}
	return baseloader.LoadOne[ShapeDefinition](
		logger,
		"memql.unifiedShapeLoader",
		"shape",
		baseloader.ReadAll(logger),
		extractAdapter,
		func(origin string, raw []byte) (*ShapeDefinition, error) {
			decl, err := languageParser.ParseShapeDecl(string(raw))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", origin, err)
			}
			shape, err := shapeDeclToShapeDefinition(decl, origin)
			if err != nil {
				return nil, err
			}
			shape.Origin = strings.TrimSuffix(origin, ":"+shape.Name)
			return shape, nil
		},
		registry.Upsert,
	)
}

// LoadUnifiedProviders walks the new tree, parses every `provider
// NAME { ... }` block, and upserts into the registry. Provider files
// bundle a vendor base + all its models in one file.
//
// As of sub-epic #309 child #316, the langparser handles the
// `provider NAME { ... }` struct form natively
// (langparser.ParseProviderDecl). This loader slices each provider
// out of the per-file source, parses it through the langparser, and
// converts the resulting *ast.ProviderDecl to a *ProviderConfig via
// providerDeclToProviderConfig. The legacy hand-rolled
// parseProviderMemQL is unreferenced from this loader path and will
// be deleted in sub-epic #306 child #310.
//
// Each parsed config gets a CLIENT instantiated via newSIProvider
// before the registry entry is written. Without that step the
// registry holds the Config but Available stays false and the SI
// runtime rejects every lookup with "provider <name> not available"
// (see si_runtime.go's entry.Available check). The legacy
// loadSIProviders walked the providers and called newSIProvider;
// that step was lost when Pass 3 of the DSL restructure retired the
// legacy walk. Re-attaching it here keeps the unified-loader path
// self-contained.
//
// @base providers (vendor-level entries with no concrete model, used
// only for @extends inheritance) are registered with Available=false
// on purpose: they're metadata, not callable.
func LoadUnifiedProviders(logger *slog.Logger, registry *ProviderRegistry) (int, error) {
	if registry == nil {
		return 0, fmt.Errorf("provider registry is nil")
	}

	// Two-pass load. Pass 1 registers every @base provider so the
	// inheritance lookup in pass 2 can find them regardless of file
	// order. Pass 2 walks every non-base provider, fills in fields
	// the child left empty (Type / Auth) from its @extends parent,
	// then instantiates the SDK client via newSIProvider. The single-
	// pass loader fails when the file places bases after their
	// children (which dsl/providers/providers.memql does today --
	// `openai` is at line 379 but children like audio15 / chat54
	// land at lines 27..) because the resolver runs before the base
	// is in the registry.
	files := baseloader.ReadAll(logger)
	type parsedProvider struct {
		cfg    *ProviderConfig
		origin string
	}
	all := make([]parsedProvider, 0, 64)
	for _, raw := range files {
		for _, slice := range extractAdapter(raw.Content, "provider") {
			origin := raw.Path + ":" + slice.Name
			decl, err := languageParser.ParseProviderDecl(slice.Source)
			if err != nil {
				if logger != nil {
					logger.Debug("memql.unifiedProviderLoader: parse failed",
						"file", raw.Path, "provider", slice.Name, "error", err)
				}
				continue
			}
			cfg, err := providerDeclToProviderConfig(decl)
			if err != nil {
				if logger != nil {
					logger.Debug("memql.unifiedProviderLoader: convert failed",
						"file", raw.Path, "provider", slice.Name, "error", err)
				}
				continue
			}
			if cfg == nil {
				continue
			}
			all = append(all, parsedProvider{cfg: cfg, origin: origin})
		}
	}

	// Pass 1: register bases. Available=false on purpose; their only
	// role is to be the target of @extends inheritance.
	for _, p := range all {
		if !p.cfg.Base {
			continue
		}
		registry.setEntry(&ProviderConfigEntry{Config: *p.cfg})
	}

	// Pass 2: resolve extends + instantiate client + register child.
	total := 0
	for _, p := range all {
		if p.cfg.Base {
			total++ // bases count toward "registered" even though they're not callable
			continue
		}
		entry := &ProviderConfigEntry{Config: *p.cfg}
		resolveProviderExtends(&entry.Config, registry)
		resolvedAuth, authErr := resolveAuthPlaceholders(entry.Config.Auth)
		if authErr != nil {
			entry.err = authErr
			if logger != nil {
				logger.Warn("memql.unifiedProviderLoader: auth resolution failed; registered as unavailable",
					"provider", entry.Config.Name, "error", authErr)
			}
			registry.setEntry(entry)
			total++
			continue
		}
		entry.Config.Auth = resolvedAuth
		client, clientErr := newSIProvider(entry.Config)
		if clientErr != nil {
			entry.err = clientErr
			if logger != nil {
				logger.Warn("memql.unifiedProviderLoader: provider client construction failed; registered as unavailable",
					"provider", entry.Config.Name, "type", entry.Config.Type, "error", clientErr)
			}
		} else {
			entry.Client = client
			entry.Available = true
		}
		registry.setEntry(entry)
		total++
	}

	if logger != nil {
		logger.Info("memql.unifiedProviderLoader: registered",
			"keyword", "provider", "count", total)
	}
	return total, nil
}

// LoadUnifiedTools walks the new tree, parses every `tool NAME
// { ... }` block through the langparser's load-time path, and
// upserts each into the registry.
//
// memql#317 (sub-epic #309 sibling of #315 / #316) migrated the
// parsing half off the hand-rolled parseToolMemQL onto
// langparser.ParseToolDecl + the in-package toolDeclToTool
// converter. The hand-rolled parser is unreferenced from production
// code after this child; tests in tool_parser_test.go still
// exercise it pending the final deletion in #306 child D (#310).
func LoadUnifiedTools(logger *slog.Logger, registry *ToolRegistry) (int, error) {
	if registry == nil {
		return 0, fmt.Errorf("tool registry is nil")
	}
	return baseloader.LoadMany[Tool](
		logger,
		"memql.unifiedToolLoader",
		"tool",
		baseloader.ReadAll(logger),
		extractAdapter,
		func(origin string, raw []byte) ([]*Tool, error) {
			decl, err := languageParser.ParseToolDecl(string(raw))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", origin, err)
			}
			return toolDeclToTool(decl, origin)
		},
		registry.Upsert,
	)
}

// LoadUnifiedBuiltins walks the new tree, parses every `builtin NAME
// { ... }` block through the langparser's load-time path, and upserts
// into the FunctionRegistry (builtins are functions internally).
//
// memql#318 (sub-epic #309 / #306 child C) migrated the parsing half
// off the hand-rolled parseBuiltinMemQL onto
// languageParser.ParseBuiltinDecl + the in-package
// builtinDeclToFunction converter. The hand-rolled parser is
// unreferenced from production after this child; tests in
// builtin_parser_test.go still exercise it pending the final deletion
// in #306 child D (#310).
func LoadUnifiedBuiltins(logger *slog.Logger, registry *FunctionRegistry) (int, error) {
	if registry == nil {
		return 0, fmt.Errorf("function registry is nil")
	}
	return baseloader.LoadOne[Function](
		logger,
		"memql.unifiedBuiltinLoader",
		"builtin",
		baseloader.ReadAll(logger),
		extractAdapter,
		func(origin string, raw []byte) (*Function, error) {
			decl, err := languageParser.ParseBuiltinDecl(string(raw))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", origin, err)
			}
			return builtinDeclToFunction(decl, origin)
		},
		registry.Upsert,
	)
}

// LoadUnifiedPrompts walks the new tree, extracts every `prompt NAME
// { ... }` block through the langparser's load-time path, resolves
// its template (.tmpl sidecar) against the unified FS, compiles the
// input schema, and registers via PromptRegistry.set.
//
// memql#319 (sub-epic #309 / #306 child C) migrated the parsing half
// off the hand-rolled parsePromptMemQL onto
// languageParser.ParsePromptDecl + the in-package
// promptDeclToPromptDecl converter. The hand-rolled parser is
// unreferenced from production after this child; tests under
// prompt_parser_test.go still exercise it pending the final deletion
// in #306 child D (#310).
//
// Prompts can't use the generic helper because the per-slice work
// (template resolution + schema compilation) needs the raw file's
// directory path to resolve sidecar templates. Kept inline for that
// reason.
func LoadUnifiedPrompts(logger *slog.Logger, registry *PromptRegistry, partials *template.Template) (int, error) {
	if registry == nil {
		return 0, fmt.Errorf("prompt registry is nil")
	}
	tree := memqldsl.Tree()
	total := 0
	for _, raw := range baseloader.ReadAll(logger) {
		for _, slice := range ExtractKeywordSlices(raw.Content, "prompt") {
			origin := "unified:" + raw.Path + ":" + slice.Name
			astDecl, err := languageParser.ParsePromptDecl(slice.Source)
			if err != nil {
				if logger != nil {
					logger.Debug("memql.unifiedPromptLoader: parse failed",
						"file", raw.Path, "prompt", slice.Name, "error", err)
				}
				continue
			}
			decl, err := promptDeclToPromptDecl(astDecl, origin)
			if err != nil {
				if logger != nil {
					logger.Debug("memql.unifiedPromptLoader: convert failed",
						"file", raw.Path, "prompt", slice.Name, "error", err)
				}
				continue
			}
			source, err := resolveUnifiedPromptTemplate(decl, raw.Path, tree)
			if err != nil {
				if logger != nil {
					logger.Warn("memql.unifiedPromptLoader: template resolve failed",
						"file", raw.Path, "prompt", slice.Name, "error", err)
				}
				continue
			}
			tmpl, err := template.Must(partials.Clone()).New(decl.name).Parse(source)
			if err != nil {
				if logger != nil {
					logger.Warn("memql.unifiedPromptLoader: template parse failed",
						"prompt", slice.Name, "error", err)
				}
				continue
			}
			var compiledSchema *jsonschema.Schema
			if inputSchema, sErr := decl.toInputSchema(); sErr == nil && inputSchema != nil {
				schemaBytes, mErr := json.Marshal(inputSchema)
				if mErr == nil {
					compiler := jsonschema.NewCompiler()
					compiler.Draft = jsonschema.Draft2019
					if rErr := compiler.AddResource(decl.name, bytes.NewReader(schemaBytes)); rErr == nil {
						if compiled, cErr := compiler.Compile(decl.name); cErr == nil {
							compiledSchema = compiled
						}
					}
				}
			}
			registry.set(&PromptTemplate{
				Name:            decl.name,
				Description:     decl.description,
				TemplateSource:  source,
				DefaultProvider: decl.defaultProvider,
				tmpl:            tmpl,
				inputSchema:     compiledSchema,
			})
			total++
		}
	}
	if logger != nil {
		logger.Info("memql.unifiedPromptLoader: registered",
			"component", "memql.unifiedPromptLoader", "count", total)
	}
	return total, nil
}

// fileTopUseClauseRe matches a legacy single-line `use <ns>.<concept>`
// clause anchored at column 0. Form B `use <path>.{ names }` imports
// can span multiple lines and are extracted separately by
// extractFileTopPreamble below.
var fileTopUseClauseRe = regexp.MustCompile(`(?m)^[ \t]*use[ \t]+([A-Za-z_][A-Za-z0-9_]*)\.([A-Za-z_][A-Za-z0-9_]*)[ \t]*$`)

// extractFileTopPreamble returns every file-top `use ...` clause in
// the source -- both Form A (legacy single-binding) and Form B
// (`path.{ names }`) shapes -- joined into a single preamble string
// that the loader re-prepends to each per-seed slice. Form B clauses
// can span multiple lines, so we scan with a small state machine
// rather than a per-line regex.
//
// Stops at the first non-comment, non-blank, non-`use` line --
// typically the first `seed`, `@`, or other construct keyword.
func extractFileTopPreamble(source string) string {
	var out []string
	i := 0
	for i < len(source) {
		// Find start of next logical line.
		lineStart := i
		// Skip leading whitespace within the line.
		for lineStart < len(source) && (source[lineStart] == ' ' || source[lineStart] == '\t') {
			lineStart++
		}
		if lineStart >= len(source) {
			break
		}
		// Detect blank line -- skip and continue.
		if source[lineStart] == '\n' || source[lineStart] == '\r' {
			// advance i past the newline
			for i < len(source) && source[i] != '\n' {
				i++
			}
			if i < len(source) {
				i++
			}
			continue
		}
		// Detect line comment -- skip and continue.
		if lineStart+1 < len(source) && source[lineStart] == '/' && source[lineStart+1] == '/' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			if i < len(source) {
				i++
			}
			continue
		}
		// `use` keyword followed by whitespace?
		if lineStart+4 <= len(source) && source[lineStart:lineStart+3] == "use" &&
			(source[lineStart+3] == ' ' || source[lineStart+3] == '\t') {
			// Walk to end of this use clause: either end-of-line for
			// Form A, or balanced closing `}` for Form B.
			j := lineStart
			braceDepth := 0
			seenBrace := false
			for j < len(source) {
				ch := source[j]
				if ch == '{' {
					braceDepth++
					seenBrace = true
				} else if ch == '}' {
					braceDepth--
					if seenBrace && braceDepth == 0 {
						j++
						break
					}
				} else if ch == '\n' && !seenBrace {
					break
				}
				j++
			}
			out = append(out, source[lineStart:j])
			// Advance past the terminating newline if present.
			i = j
			if i < len(source) && source[i] == '\n' {
				i++
			}
			continue
		}
		// Hit body / construct keyword. Stop.
		break
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

// LoadUnifiedSeeds walks the DSL tree, parses every `seed NAME { ... }`
// block, compiles each into a SeedDefinition, and registers it. Row
// materialization is a separate pass owned by the SeedMaterializer
// (which runs after the database is up and can write rows). This
// loader is parse + collect only.
//
// File shape: each `.memql` file may contain a single file-top
// `use <namespace>.<concept>` clause that binds every `seed NAME { }`
// block in the file to the same target concept. This is the
// canonical authoring pattern -- the role catalog under
// dsl/agents/roles/ packs dozens of seeds per file sharing one use
// clause, and the platform agents (assistant.memql etc.)
// share the same one-use-many-seeds convention even when the count
// is one.
//
// Files that fail to parse or compile are logged + skipped so a
// single bad seed doesn't bring down the rest. A registry-upsert
// failure (duplicate name) is also logged + skipped -- the first
// definition wins, the second is dropped.
//
// Sidecar resolution: when a seed declares @templateFile, the file
// content is loaded into Body's "systemPrompt" field (if not already
// declared), mirroring the agent loader's behavior. The materializer
// passes the body through to the row insert without further sidecar
// handling.
func LoadUnifiedSeeds(logger *slog.Logger, registry *SeedRegistry) (int, error) {
	if registry == nil {
		return 0, fmt.Errorf("seed registry is nil")
	}
	tree := memqldsl.Tree()
	total := 0
	for _, raw := range baseloader.ReadAll(logger) {
		// Extract the file-top preamble (every `use ...` clause,
		// both Form A and Form B) once per file. Each per-seed slice
		// doesn't include the file-top preamble (the
		// ExtractKeywordSlices preamble walk-back stops at the blank
		// line before the seed's annotation cluster), so we re-inject
		// it into every slice we parse.
		preamble := extractFileTopPreamble(raw.Content)

		for _, slice := range ExtractKeywordSlices(raw.Content, "seed") {
			origin := "unified:" + raw.Path + ":" + slice.Name
			source := preamble + slice.Source
			decl, err := parseSeedMemQL(origin, []byte(source))
			if err != nil {
				if logger != nil {
					logger.Warn("memql.unifiedSeedLoader: parse failed",
						"file", raw.Path, "seed", slice.Name, "error", err)
				}
				continue
			}
			def, err := compileSeedDecl(decl)
			if err != nil {
				if logger != nil {
					logger.Warn("memql.unifiedSeedLoader: compile failed",
						"file", raw.Path, "seed", slice.Name, "error", err)
				}
				continue
			}
			def.Origin = origin

			// Resolve @templateFile sidecar -- contents land in the
			// body under "systemPrompt" (when not already set). This
			// matches the agent-loader convention; the materializer
			// just writes whatever's in the body to the row.
			if def.TemplateFile != "" {
				if _, alreadySet := def.Body.fields["systemPrompt"]; !alreadySet {
					dir := path.Dir(raw.Path)
					tmplPath := path.Join(dir, def.TemplateFile)
					data, rErr := fs.ReadFile(tree, tmplPath)
					if rErr != nil {
						if logger != nil {
							logger.Warn("memql.unifiedSeedLoader: templateFile resolve failed",
								"file", raw.Path, "seed", slice.Name, "templateFile", def.TemplateFile,
								"resolvedTo", tmplPath, "error", rErr)
						}
						continue
					}
					source := strings.TrimSpace(string(data))
					if source == "" {
						if logger != nil {
							logger.Warn("memql.unifiedSeedLoader: templateFile is empty",
								"file", raw.Path, "seed", slice.Name, "templateFile", tmplPath)
						}
						continue
					}
					// Inject systemPrompt at the body's top level so
					// the materializer's downcast into row payload
					// includes it like any other field.
					def.Body.keys = append(def.Body.keys, "systemPrompt")
					def.Body.fields["systemPrompt"] = seedValue{kind: seedString, str: source}
				}
			}

			if err := registry.Upsert(def); err != nil {
				if logger != nil {
					logger.Warn("memql.unifiedSeedLoader: registry upsert failed",
						"file", raw.Path, "seed", slice.Name, "error", err)
				}
				continue
			}
			total++
		}
	}
	if logger != nil {
		logger.Info("memql.unifiedSeedLoader: registered",
			"component", "memql.unifiedSeedLoader", "count", total)
	}
	return total, nil
}

// resolveUnifiedPromptTemplate mirrors resolvePromptDeclTemplate but
// reads .tmpl sidecars from dsl.Tree() at the prompt's relative
// path.
func resolveUnifiedPromptTemplate(decl *promptDecl, memqlPath string, tree fs.FS) (string, error) {
	if decl.templateFile != "" {
		dir := path.Dir(memqlPath)
		tmplPath := path.Join(dir, decl.templateFile)
		data, err := fs.ReadFile(tree, tmplPath)
		if err != nil {
			return "", fmt.Errorf("read templateFile %q (resolved to %q): %w", decl.templateFile, tmplPath, err)
		}
		source := strings.TrimSpace(string(data))
		if source == "" {
			return "", fmt.Errorf("templateFile %q is empty", tmplPath)
		}
		return source, nil
	}
	if decl.templateSource != "" {
		return decl.templateSource, nil
	}
	return "", fmt.Errorf("prompt %q: template or templateFile is required", decl.name)
}

// providerDeclToProviderConfig translates the langparser-produced
// AST node into the *ProviderConfig the loader registers. The
// validation rules mirror what the legacy parseProviderMemQL applied
// at the end of its parse: @type is required unless the provider is
// @base (vendor-level metadata) or @extends another provider (in
// which case the parent supplies the type); @model is required
// unless the provider is @base. Auth and Params maps are aliased
// straight through -- the env() resolution + base-extends merge
// happens later in this loader's pass 2.
func providerDeclToProviderConfig(decl *ast.ProviderDecl) (*ProviderConfig, error) {
	if decl == nil {
		return nil, fmt.Errorf("provider declaration is nil")
	}
	if decl.Type == "" && decl.Extends == "" && !decl.IsBase {
		return nil, fmt.Errorf("provider %q: @type is required", decl.Name)
	}
	if decl.Model == "" && !decl.IsBase {
		return nil, fmt.Errorf("provider %q: @model is required", decl.Name)
	}
	return &ProviderConfig{
		Name:        decl.Name,
		Type:        decl.Type,
		Model:       decl.Model,
		Auth:        decl.Auth,
		Params:      decl.Params,
		Default:     decl.IsDefault,
		Modality:    decl.Modality,
		Description: decl.Description,
		Base:        decl.IsBase,
		Extends:     decl.Extends,
	}, nil
}

// resolveProviderExtends fills in fields a child provider didn't
// declare explicitly by copying from the @extends base. The DSL
// convention is "base carries vendor-wide auth + the canonical
// @type; children pin a model and override params." Without this
// step, children that omit @type (most of them) boot with Type=""
// and newSIProvider rejects with "unsupported provider type" --
// and children that omit auth (every one of them) fail with
// "missing auth.apiKey".
//
// Only fields the child left empty get filled. An explicit
// declaration on the child always wins -- @extends is inheritance,
// not override.
//
// File-order assumption: the base must be registered before the
// child. providers.memql keeps bases at the top of each vendor
// block, so the linear loader walk satisfies this naturally. A
// future split that disturbs the ordering would surface as
// "missing auth.apiKey" on the first call after rebuild; in that
// case the fix is either re-ordering the file or doing a two-pass
// resolve (bases first, then children).
func resolveProviderExtends(cfg *ProviderConfig, registry *ProviderRegistry) {
	if cfg == nil || registry == nil {
		return
	}
	parentName := strings.TrimSpace(cfg.Extends)
	if parentName == "" {
		return
	}
	parent, ok := registry.Entry(parentName)
	if !ok || parent == nil {
		return
	}
	if cfg.Type == "" {
		cfg.Type = parent.Config.Type
	}
	if len(cfg.Auth) == 0 && len(parent.Config.Auth) > 0 {
		cfg.Auth = make(map[string]string, len(parent.Config.Auth))
		for k, v := range parent.Config.Auth {
			cfg.Auth[k] = v
		}
	} else if len(parent.Config.Auth) > 0 {
		// Merge: parent fields the child didn't supply.
		for k, v := range parent.Config.Auth {
			if _, exists := cfg.Auth[k]; !exists {
				cfg.Auth[k] = v
			}
		}
	}
}
