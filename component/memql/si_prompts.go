package memql

import (
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"text/template"

	"github.com/santhosh-tekuri/jsonschema/v5"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// PromptTemplate represents a compiled prompt definition.
type PromptTemplate struct {
	Name            string
	Description     string
	TemplateSource  string
	DefaultProvider string

	tmpl        *template.Template
	inputSchema *jsonschema.Schema
}

// PromptRegistry stores prompt templates by name.
type PromptRegistry struct {
	mu     sync.RWMutex
	byName map[string]*PromptTemplate
}

func newPromptRegistry() *PromptRegistry {
	return &PromptRegistry{byName: make(map[string]*PromptTemplate)}
}

// Get retrieves a prompt template by name.
func (r *PromptRegistry) Get(name string) (*PromptTemplate, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byName[name]
	return t, ok
}

// List returns every registered prompt template, sorted by name. Used by the
// MCP prompts/list surface (Phase 6 #1536).
func (r *PromptRegistry) List() []*PromptTemplate {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*PromptTemplate, 0, len(r.byName))
	for _, t := range r.byName {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Arguments returns the prompt's input-schema fields as MCP prompt argument
// descriptors ({name, required}), sorted by name. Empty when the prompt declares
// no input schema. (Phase 6 #1536)
func (p *PromptTemplate) Arguments() []map[string]any {
	if p == nil || p.inputSchema == nil {
		return nil
	}
	required := make(map[string]bool, len(p.inputSchema.Required))
	for _, r := range p.inputSchema.Required {
		required[r] = true
	}
	names := make([]string, 0, len(p.inputSchema.Properties))
	for n := range p.inputSchema.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"name": n, "required": required[n]})
	}
	return out
}

// set registers a prompt template, replacing any existing entry.
func (r *PromptRegistry) set(prompt *PromptTemplate) {
	if r == nil || prompt == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName[prompt.Name] = prompt
}

// loadPartials reads shared .tmpl snippets from the new tree at
// dsl/common/_partials/ and returns a template pre-loaded with their
// {{define}} blocks. Used by LoadUnifiedPrompts to give every prompt
// access to {{template "partial_name"}}.
func loadPartials() (*template.Template, error) {
	base := template.New("__base__").Funcs(template.FuncMap{})
	tree := memqldsl.Tree()
	entries, err := fs.ReadDir(tree, "common/_partials")
	if err != nil {
		// No partials directory -- partials are optional.
		return base, nil
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".tmpl") {
			continue
		}
		data, readErr := fs.ReadFile(tree, "common/_partials/"+entry.Name())
		if readErr != nil {
			return nil, fmt.Errorf("read partial %s: %w", entry.Name(), readErr)
		}
		if _, parseErr := base.Parse(string(data)); parseErr != nil {
			return nil, fmt.Errorf("parse partial %s: %w", entry.Name(), parseErr)
		}
	}
	return base, nil
}

// loadPromptRegistry returns an empty registry. Pass 3 of the DSL
// restructure migration retired the legacy walk over
// dsl/v1/prompts/. Prompts now load via LoadUnifiedPrompts (called
// from engine.go) which walks dsl/<domain>/prompts/.
func loadPromptRegistry(logger *slog.Logger) (*PromptRegistry, error) {
	_ = logger
	return newPromptRegistry(), nil
}

// Render renders the compiled template with the provided data object.
func (p *PromptTemplate) Render(data any) (string, error) {
	if p == nil || p.tmpl == nil {
		return "", fmt.Errorf("prompt template not compiled")
	}
	var buf bytes.Buffer
	if err := p.tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ValidateData validates input data against the optional schema.
func (p *PromptTemplate) ValidateData(data any) error {
	if p == nil || p.inputSchema == nil {
		return nil
	}
	if err := p.inputSchema.Validate(data); err != nil {
		return fmt.Errorf("data validation failed: %w", err)
	}
	return nil
}
