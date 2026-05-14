package memql

import (
	"encoding/json"

	concept "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/component/memql/sense"
)

// SenseAdapter implements sense.RegistryProvider by reading from the MemQLEngine registries.
type SenseAdapter struct {
	engine *MemQLEngine
}

// NewSenseAdapter creates a new adapter that bridges engine registries to the sense package.
func NewSenseAdapter(engine *MemQLEngine) *SenseAdapter {
	return &SenseAdapter{engine: engine}
}

func (a *SenseAdapter) FunctionNames() []string {
	if a.engine.functions == nil {
		return nil
	}
	return a.engine.functions.Names()
}

func (a *SenseAdapter) FunctionGet(name string) (*sense.FunctionInfo, bool) {
	if a.engine.functions == nil {
		return nil, false
	}
	fn, err := a.engine.functions.Get(name)
	if err != nil {
		return nil, false
	}
	return &sense.FunctionInfo{
		Name:        fn.Name,
		Description: fn.Description,
		Kind:        fn.FunctionKind,
		ArgsDoc:     fn.UsageDoc,
		Enabled:     fn.Enabled,
		Deprecated:  fn.Deprecated,
	}, true
}

func (a *SenseAdapter) ConceptNames() []string {
	if a.engine.concepts == nil {
		return nil
	}
	concepts := a.engine.concepts.List()
	names := make([]string, len(concepts))
	for i, c := range concepts {
		names[i] = c.Name
	}
	return names
}

func (a *SenseAdapter) ConceptGet(name string) (*sense.ConceptInfo, bool) {
	if a.engine.concepts == nil {
		return nil, false
	}
	c, err := a.engine.concepts.Get(name)
	if err != nil {
		return nil, false
	}
	return &sense.ConceptInfo{
		Name:        c.Name,
		Description: c.Description,
		Fields:      extractConceptFields(c),
	}, true
}

func (a *SenseAdapter) SpecNames() []string {
	if a.engine.specs == nil {
		return nil
	}
	return a.engine.specs.Names()
}

func (a *SenseAdapter) ToolNames() []string {
	if a.engine.tools == nil {
		return nil
	}
	return a.engine.tools.Names()
}

func (a *SenseAdapter) ToolGet(name string) (*sense.ToolInfo, bool) {
	if a.engine.tools == nil {
		return nil, false
	}
	t, err := a.engine.tools.Get(name)
	if err != nil {
		return nil, false
	}
	return &sense.ToolInfo{
		Name:        t.Name,
		Description: t.Description,
	}, true
}

func (a *SenseAdapter) PromptNames() []string {
	if a.engine.prompts == nil {
		return nil
	}
	// PromptRegistry doesn't have Names(), iterate via known prompts.
	// For now return nil -- prompts are loaded dynamically.
	return nil
}

func (a *SenseAdapter) PromptGet(name string) (*sense.PromptInfo, bool) {
	if a.engine.prompts == nil {
		return nil, false
	}
	p, ok := a.engine.prompts.Get(name)
	if !ok {
		return nil, false
	}
	return &sense.PromptInfo{
		Name:            p.Name,
		Description:     p.Description,
		DefaultProvider: p.DefaultProvider,
	}, true
}

func (a *SenseAdapter) ProviderNames() []string {
	if a.engine.providers == nil {
		return nil
	}
	a.engine.providers.mu.RLock()
	defer a.engine.providers.mu.RUnlock()
	names := make([]string, 0, len(a.engine.providers.byName))
	for name, entry := range a.engine.providers.byName {
		if entry != nil && entry.Available {
			names = append(names, name)
		}
	}
	return names
}

func (a *SenseAdapter) ProviderGet(name string) (*sense.ProviderInfo, bool) {
	if a.engine.providers == nil {
		return nil, false
	}
	entry, ok := a.engine.providers.Entry(name)
	if !ok {
		return nil, false
	}
	return &sense.ProviderInfo{
		Name:     entry.Config.Name,
		Type:     entry.Config.Type,
		Model:    entry.Config.Model,
		Modality: entry.Config.Modality,
	}, true
}

func (a *SenseAdapter) ShapeNames() []string {
	if a.engine.shapes == nil {
		return nil
	}
	shapes := a.engine.shapes.List()
	names := make([]string, len(shapes))
	for i, s := range shapes {
		names[i] = s.Name
	}
	return names
}

func (a *SenseAdapter) ShapeGet(name string) (*sense.ShapeInfo, bool) {
	if a.engine.shapes == nil {
		return nil, false
	}
	s, ok := a.engine.shapes.Get(name)
	if !ok {
		return nil, false
	}
	return &sense.ShapeInfo{
		Name:        s.Name,
		Description: s.Description,
	}, true
}

func (a *SenseAdapter) IntegrationCapabilities() []string {
	if a.engine.integrations == nil {
		return nil
	}
	return a.engine.integrations.List()
}

// extractConceptFields extracts field info from a concept's definition schema.
func extractConceptFields(c *concept.Concept) []sense.FieldInfo {
	schemaData, ok := c.Schemas["definition"]
	if !ok {
		return nil
	}

	var schema struct {
		Properties map[string]struct {
			Type        string   `json:"type"`
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schemaData, &schema); err != nil {
		return nil
	}

	requiredSet := make(map[string]bool)
	for _, r := range schema.Required {
		requiredSet[r] = true
	}

	var fields []sense.FieldInfo
	for name, prop := range schema.Properties {
		fields = append(fields, sense.FieldInfo{
			Name:        name,
			Type:        prop.Type,
			Description: prop.Description,
			Required:    requiredSet[name],
			Enum:        prop.Enum,
		})
	}
	return fields
}
