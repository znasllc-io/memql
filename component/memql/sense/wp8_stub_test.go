package sense

// stubRegistry is a configurable RegistryProvider for the WP8 tests (signature
// help + hover). It reads from the maps/slices the test populates and returns
// zero values elsewhere. (construct_concept_test.go's fakeRegistry is fixed;
// this one is driven by the test.)
type stubRegistry struct {
	functions map[string]*FunctionInfo
	concepts  map[string]*ConceptInfo
	providers []string
	shapes    []string
}

func (r *stubRegistry) FunctionNames() []string {
	names := make([]string, 0, len(r.functions))
	for n := range r.functions {
		names = append(names, n)
	}
	return names
}
func (r *stubRegistry) FunctionGet(name string) (*FunctionInfo, bool) {
	f, ok := r.functions[name]
	return f, ok
}
func (r *stubRegistry) ConceptNames() []string {
	names := make([]string, 0, len(r.concepts))
	for n := range r.concepts {
		names = append(names, n)
	}
	return names
}
func (r *stubRegistry) ConceptGet(name string) (*ConceptInfo, bool) {
	c, ok := r.concepts[name]
	return c, ok
}
func (r *stubRegistry) SpecNames() []string                      { return nil }
func (r *stubRegistry) ToolNames() []string                      { return nil }
func (r *stubRegistry) ToolGet(string) (*ToolInfo, bool)         { return nil, false }
func (r *stubRegistry) PromptNames() []string                    { return nil }
func (r *stubRegistry) PromptGet(string) (*PromptInfo, bool)     { return nil, false }
func (r *stubRegistry) ProviderNames() []string                  { return r.providers }
func (r *stubRegistry) ProviderGet(string) (*ProviderInfo, bool) { return nil, false }
func (r *stubRegistry) ShapeNames() []string                     { return r.shapes }
func (r *stubRegistry) ShapeGet(string) (*ShapeInfo, bool)       { return nil, false }
func (r *stubRegistry) IntegrationCapabilities() []string        { return nil }
