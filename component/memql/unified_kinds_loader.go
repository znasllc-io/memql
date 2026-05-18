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

	"github.com/znasllc-io/memql/component/memql/baseloader"
	memqldsl "github.com/znasllc-io/memql/dsl"
	"github.com/santhosh-tekuri/jsonschema/v5"
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
// { ... }` block, and upserts the resulting ShapeDefinition into the
// supplied registry.
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
			shape, err := parseShapeMemQL(origin, raw)
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
// bundle a vendor base + all its models in one file, so the parser
// returns a slice (handled by baseloader.LoadMany).
//
// Providers ride the MemQL-DSL parser (parseProviderMemQL), not the
// legacy JSON path (parseProviderConfigs). Until 2026-05-17 this
// loader called parseProviderConfigs by mistake, which tried to
// json.Unmarshal the MemQL provider syntax and silently failed every
// slice. Result: every node booted with providers=0 and every SI
// call returned `provider "<name>" not available` -- including the
// planner agent loop's plannerAgent invocation.
//
// Each parsed config also gets a CLIENT instantiated via newSIProvider
// before the registry entry is written. Without that step the
// registry holds the Config but Available stays false and the SI
// runtime rejects every lookup with "provider <name> not available"
// (see si_runtime.go's entry.Available check). The legacy
// loadSIProviders walked the providers and called newSIProvider; that
// step was lost when Pass 3 of the DSL restructure retired the
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
	return baseloader.LoadMany[ProviderConfig](
		logger,
		"memql.unifiedProviderLoader",
		"provider",
		baseloader.ReadAll(logger),
		extractAdapter,
		func(origin string, raw []byte) ([]*ProviderConfig, error) {
			cfg, err := parseProviderMemQL(origin, raw)
			if err != nil {
				return nil, err
			}
			if cfg == nil {
				return nil, nil
			}
			return []*ProviderConfig{cfg}, nil
		},
		func(cfg *ProviderConfig) error {
			entry := &ProviderConfigEntry{Config: *cfg}
			// @base entries (vendor-level inheritance roots) are
			// declared without a model + only used as extension
			// targets. They have no callable client; register them
			// with Available=false so name-lookup still resolves but
			// no SI runtime path tries to invoke them.
			if cfg.Base {
				registry.setEntry(entry)
				return nil
			}
			client, clientErr := newSIProvider(*cfg)
			if clientErr != nil {
				// Construction failure (missing auth, unsupported
				// type, etc.) -- stash the error on the entry so
				// debug tooling can surface it, but don't break the
				// load. Other providers should still register.
				entry.err = clientErr
				if logger != nil {
					logger.Warn("memql.unifiedProviderLoader: provider client construction failed; registered as unavailable",
						"provider", cfg.Name, "type", cfg.Type, "error", clientErr)
				}
			} else {
				entry.Client = client
				entry.Available = true
			}
			registry.setEntry(entry)
			return nil
		},
	)
}

// LoadUnifiedTools walks the new tree, parses every `tool NAME
// { ... }` block, and upserts each into the registry.
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
		parseToolMemQL,
		registry.Upsert,
	)
}

// LoadUnifiedBuiltins walks the new tree, parses every `builtin NAME
// { ... }` block, and upserts into the FunctionRegistry (builtins
// are functions internally).
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
		parseBuiltinMemQL,
		registry.Upsert,
	)
}

// LoadUnifiedPrompts walks the new tree, extracts every `prompt NAME
// { ... }` block, resolves its template (.tmpl sidecar or inline
// source) against the unified FS, compiles the input schema, and
// registers via PromptRegistry.set.
//
// Prompts can't use the generic helper directly because the per-
// slice work (template resolution + schema compilation) needs the
// raw file's directory path to resolve sidecar templates. Kept
// inline for that reason.
func LoadUnifiedPrompts(logger *slog.Logger, registry *PromptRegistry, partials *template.Template) (int, error) {
	if registry == nil {
		return 0, fmt.Errorf("prompt registry is nil")
	}
	tree := memqldsl.Tree()
	total := 0
	for _, raw := range baseloader.ReadAll(logger) {
		for _, slice := range ExtractKeywordSlices(raw.Content, "prompt") {
			origin := "unified:" + raw.Path + ":" + slice.Name
			decl, err := parsePromptMemQL(origin, []byte(slice.Source))
			if err != nil {
				if logger != nil {
					logger.Debug("memql.unifiedPromptLoader: parse failed",
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

// fileTopUseClauseRe matches a `use <namespace>.<concept>` line
// anchored at column 0 (with optional leading whitespace). Each seed
// file is expected to declare its target concept once at the top;
// the loader extracts it and re-injects into every per-seed slice
// before parsing (ExtractKeywordSlices walks the slice preamble back
// from the seed line through annotations + comments, which stops
// short of the `use` line -- so we have to carry it forward
// ourselves).
var fileTopUseClauseRe = regexp.MustCompile(`(?m)^[ \t]*use[ \t]+([A-Za-z_][A-Za-z0-9_]*)\.([A-Za-z_][A-Za-z0-9_]*)[ \t]*$`)

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
// clause, and the platform agents (generalAssistant.memql etc.)
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
		// Extract the file-top `use <ns>.<concept>` clause once per
		// file. Each per-seed slice doesn't include it (the
		// ExtractKeywordSlices preamble walk-back stops at the
		// blank line before the seed's annotation cluster), so we
		// re-inject it into every slice we parse.
		useLine := ""
		if m := fileTopUseClauseRe.FindStringSubmatch(raw.Content); len(m) == 3 {
			useLine = "use " + m[1] + "." + m[2] + "\n"
		}

		for _, slice := range ExtractKeywordSlices(raw.Content, "seed") {
			origin := "unified:" + raw.Path + ":" + slice.Name
			source := useLine + slice.Source
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
