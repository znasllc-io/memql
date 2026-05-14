// Package sense provides the MemQL Sense language service -- syntax highlighting,
// autocompletion, diagnostics, hover info, and signature help for .memql files.
// The service is stateless and thread-safe. It reads engine registries via the
// RegistryProvider interface and uses the existing parser/lexer for analysis.
package sense

// RegistryProvider abstracts engine registries for the language service.
// Implemented by an adapter in the memql package to avoid circular imports.
type RegistryProvider interface {
	FunctionNames() []string
	FunctionGet(name string) (*FunctionInfo, bool)
	ConceptNames() []string
	ConceptGet(name string) (*ConceptInfo, bool)
	SpecNames() []string
	ToolNames() []string
	ToolGet(name string) (*ToolInfo, bool)
	PromptNames() []string
	PromptGet(name string) (*PromptInfo, bool)
	ProviderNames() []string
	ProviderGet(name string) (*ProviderInfo, bool)
	ShapeNames() []string
	ShapeGet(name string) (*ShapeInfo, bool)
	IntegrationCapabilities() []string
}

// FunctionInfo is a lightweight projection of an engine Function.
type FunctionInfo struct {
	Name        string
	Description string
	Kind        string // "query", "mutation", "automation", "spec", "tool", "builtin", "prompt", "provider", "shape"
	ArgsDoc     string // serialized arg assertions or description
	Enabled     bool
	Deprecated  string
}

// ConceptInfo is a lightweight projection of a concept definition.
type ConceptInfo struct {
	Name        string
	Description string
	Fields      []FieldInfo
}

// FieldInfo describes a single field in a concept.
type FieldInfo struct {
	Name        string
	Type        string
	Description string
	Required    bool
	Enum        []string
}

// ToolInfo is a lightweight projection of a tool definition.
type ToolInfo struct {
	Name        string
	Description string
}

// PromptInfo is a lightweight projection of a prompt definition.
type PromptInfo struct {
	Name            string
	Description     string
	DefaultProvider string
}

// ProviderInfo is a lightweight projection of a provider configuration.
type ProviderInfo struct {
	Name     string
	Type     string // "OpenAI", "Anthropic", etc.
	Model    string
	Modality string // "text", "tts", "stt", "embedding"
}

// ShapeInfo is a lightweight projection of a shape definition.
type ShapeInfo struct {
	Name        string
	Description string
}

// Token represents a semantic token for syntax highlighting.
type Token struct {
	Type    string // keyword, identifier, string, number, operator, annotation, comment, punctuation, concept
	Literal string
	Range   Range
}

// Position represents a cursor position (1-indexed).
type Position struct {
	Line   int
	Column int
}

// Range represents a span of text.
type Range struct {
	Start Position
	End   Position
}

// CompletionItem represents a single completion suggestion.
type CompletionItem struct {
	Label         string
	Kind          string // keyword, function, concept, annotation, builtin, spec, shape, provider, tool, field, snippet, receiver
	Detail        string // short type/signature
	Documentation string // longer description (markdown)
	InsertText    string // text to insert
	SortPriority  int    // lower = higher priority
}

// Diagnostic represents a single error or warning.
type Diagnostic struct {
	Range    Range
	Severity Severity
	Message  string
	Code     string // e.g., "unknown-function", "parse-error"
}

// Severity levels for diagnostics.
type Severity int

const (
	SeverityError   Severity = 1
	SeverityWarning Severity = 2
	SeverityInfo    Severity = 3
	SeverityHint    Severity = 4
)

// HoverResult contains information about a symbol at a position.
type HoverResult struct {
	Contents string // markdown formatted
	Range    Range
}

// SignatureResult contains function signature help.
type SignatureResult struct {
	Signatures      []Signature
	ActiveSignature int
	ActiveParameter int
}

// Signature describes a function signature.
type Signature struct {
	Label         string
	Documentation string
	Parameters    []Parameter
}

// Parameter describes a function parameter.
type Parameter struct {
	Label         string
	Documentation string
}

// Service is the MemQL Sense language service. Thread-safe and stateless.
type Service struct {
	registries RegistryProvider
}

// New creates a new Sense language service.
func New(rp RegistryProvider) *Service {
	return &Service{registries: rp}
}
