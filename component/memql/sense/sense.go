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
	Kind        string    // "query", "mutation", "automation", "spec", "tool", "builtin", "prompt", "provider", "shape"
	ArgsDoc     string    // leading documentation comment block
	Args        []ArgInfo // declared args from the function's `args { ... }` block
	Enabled     bool
	Deprecated  string
}

// ArgInfo is one declared argument of a function, projected for signature help.
type ArgInfo struct {
	Name     string
	Type     string // "string", "number", "bool", "object", "array", "any"
	Required bool
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
	// IsSnippet marks InsertText as LSP snippet syntax ($1, ${1:name},
	// $0 tabstops). The LSP layer maps it to InsertTextFormat=Snippet;
	// consumers that do not support snippets render the plain-text
	// degradation via PlainInsertText (#2629). Snippet syntax sent
	// WITHOUT this flag inserts literally -- dollar signs visible in
	// the buffer -- which is why the flag exists rather than sniffing.
	IsSnippet bool
}

// PlainInsertText renders an item's insert text for a consumer without
// snippet support: tabstops and placeholders are stripped
// (`${1:name}` -> `name`, `$0` -> “), and escaped literals are
// unescaped. Non-snippet items return InsertText unchanged.
func (c CompletionItem) PlainInsertText() string {
	if !c.IsSnippet {
		return c.InsertText
	}
	return plainFromSnippet(c.InsertText)
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
