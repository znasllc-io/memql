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
	"strings"
	"text/template"

	"github.com/visionarys-io/memql/component/memql/baseloader"
	memqldsl "github.com/visionarys-io/memql/dsl"
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
			cfgs, err := parseProviderConfigs(origin, raw)
			if err != nil {
				return nil, err
			}
			out := make([]*ProviderConfig, len(cfgs))
			for i := range cfgs {
				out[i] = &cfgs[i]
			}
			return out, nil
		},
		func(cfg *ProviderConfig) error {
			registry.setEntry(&ProviderConfigEntry{Config: *cfg})
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
