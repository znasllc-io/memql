package memql

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/memql/baseregistry"
)

var (
	// toolNamePattern enforces camelCase naming for tools.
	// Must start with lowercase letter, followed by letters/digits.
	toolNamePattern = regexp.MustCompile(`^[a-z]+[A-Za-z0-9]*$`)
)

// Tool represents an MCP-compatible tool definition.
// Tools are callable functions that AI models can invoke to perform actions.
type Tool struct {
	// Name is the unique identifier (camelCase, e.g., "searchDocuments").
	Name string `json:"name"`

	// Description provides human-readable context for the tool.
	Description string `json:"description"`

	// InputSchema is the JSON Schema describing the tool's input parameters.
	InputSchema json.RawMessage `json:"inputSchema"`

	// Handler defines how the tool executes when called.
	Handler *ToolHandler `json:"handler,omitempty"`

	// Annotations provides hints about tool behavior.
	Annotations *ToolAnnotations `json:"annotations,omitempty"`

	// ClientExecution, when true, tells the server to emit a
	// ClientToolCall envelope on the stream and await a matching
	// ClientToolResult instead of executing the handler locally. Used
	// by tools that affect browser UI (CoPresent Operator primitives).
	ClientExecution bool `json:"clientExecution,omitempty"`

	// Scopes the tool requires ("read" | "navigate" | "highlight" |
	// "create" | "update" | "delete" | "identity" | "billing"). The
	// caller's granted scope set must be a superset or the dispatcher
	// rejects the call. Middle layer of defense-in-depth between the
	// global deny-list and the per-tool confirmation UI.
	Scopes []string `json:"scopes,omitempty"`

	// AllowedRoles restricts which agent roles may call the tool.
	// Empty = no restriction. Used to gate Operator tools to
	// assistant-role agents.
	AllowedRoles []string `json:"allowedRoles,omitempty"`

	// Origin tracks where this tool was loaded from (file path).
	Origin string `json:"-"`
}

// ToolHandler defines the execution behavior of a tool.
type ToolHandler struct {
	// Type specifies the handler type: "query", "webhook", or "function".
	Type string `json:"type"`

	// Query is the MemQL expression to execute (for type "query").
	Query string `json:"query,omitempty"`

	// Shape is an optional output shaping template (for type "query").
	Shape map[string]any `json:"shape,omitempty"`

	// Name is the function name to call (for type "function").
	FunctionName string `json:"name,omitempty"`

	// URL is the webhook endpoint (for type "webhook").
	URL string `json:"url,omitempty"`

	// Method is the HTTP method (for type "webhook", defaults to POST).
	Method string `json:"method,omitempty"`

	// Headers are HTTP headers to send (for type "webhook").
	Headers map[string]string `json:"headers,omitempty"`

	// Body is the request body template (for type "webhook").
	Body map[string]any `json:"body,omitempty"`
}

// ToolAnnotations provides hints about tool behavior for AI models.
type ToolAnnotations struct {
	// Destructive indicates if the tool modifies data.
	Destructive bool `json:"destructive,omitempty"`

	// RequiresConfirmation indicates if user confirmation is needed.
	RequiresConfirmation bool `json:"requiresConfirmation,omitempty"`

	// ExecutionTime hints at how long the tool takes: "fast", "medium", "slow".
	ExecutionTime string `json:"executionTime,omitempty"`

	// RateLimit provides rate limiting hints.
	RateLimit *ToolRateLimit `json:"rateLimit,omitempty"`
}

// ToolRateLimit defines rate limiting hints for a tool.
type ToolRateLimit struct {
	// MaxCalls is the maximum number of calls allowed in the period.
	MaxCalls int `json:"maxCalls,omitempty"`

	// PeriodSeconds is the rate limit period in seconds.
	PeriodSeconds int `json:"periodSeconds,omitempty"`
}

// ToolCallResult represents the result of executing a tool.
type ToolCallResult struct {
	// Content is the result content returned by the tool.
	Content []ToolResultContent `json:"content"`

	// IsError indicates if the result represents an error.
	IsError bool `json:"isError,omitempty"`
}

// ToolResultContent represents a single piece of content in a tool result.
type ToolResultContent struct {
	// Type is the content type: "text", "image", "resource".
	Type string `json:"type"`

	// Text is the text content (for type "text").
	Text string `json:"text,omitempty"`

	// MimeType is the MIME type (for type "image" or "resource").
	MimeType string `json:"mimeType,omitempty"`

	// Data is base64-encoded binary data (for type "image").
	Data string `json:"data,omitempty"`

	// URI is a resource URI (for type "resource").
	URI string `json:"uri,omitempty"`
}

// clone creates a deep copy of the Tool.
func (t *Tool) clone() *Tool {
	if t == nil {
		return nil
	}

	cloned := &Tool{
		Name:            t.Name,
		Description:     t.Description,
		ClientExecution: t.ClientExecution,
		Origin:          t.Origin,
	}

	if t.InputSchema != nil {
		cloned.InputSchema = make(json.RawMessage, len(t.InputSchema))
		copy(cloned.InputSchema, t.InputSchema)
	}

	if t.Handler != nil {
		cloned.Handler = t.Handler.clone()
	}

	if t.Annotations != nil {
		cloned.Annotations = t.Annotations.clone()
	}

	if len(t.Scopes) > 0 {
		cloned.Scopes = append([]string(nil), t.Scopes...)
	}
	if len(t.AllowedRoles) > 0 {
		cloned.AllowedRoles = append([]string(nil), t.AllowedRoles...)
	}

	return cloned
}

// IsAllowedForRole reports whether this tool may be called by an agent
// with the given role. Empty role string is treated as "specialist"
// (matches the Agent.role default). An empty AllowedRoles list means
// the tool has no role restriction.
func (t *Tool) IsAllowedForRole(role string) bool {
	if t == nil {
		return false
	}
	if len(t.AllowedRoles) == 0 {
		return true
	}
	effective := role
	if effective == "" {
		effective = "specialist"
	}
	for _, allowed := range t.AllowedRoles {
		if allowed == effective {
			return true
		}
	}
	return false
}

// clone creates a deep copy of the ToolHandler.
func (h *ToolHandler) clone() *ToolHandler {
	if h == nil {
		return nil
	}

	cloned := &ToolHandler{
		Type:         h.Type,
		Query:        h.Query,
		FunctionName: h.FunctionName,
		URL:          h.URL,
		Method:       h.Method,
	}

	if h.Shape != nil {
		cloned.Shape = cloneMapStringAny(h.Shape)
	}

	if h.Headers != nil {
		cloned.Headers = make(map[string]string, len(h.Headers))
		for k, v := range h.Headers {
			cloned.Headers[k] = v
		}
	}

	if h.Body != nil {
		cloned.Body = cloneMapStringAny(h.Body)
	}

	return cloned
}

// clone creates a deep copy of the ToolAnnotations.
func (a *ToolAnnotations) clone() *ToolAnnotations {
	if a == nil {
		return nil
	}

	cloned := &ToolAnnotations{
		Destructive:          a.Destructive,
		RequiresConfirmation: a.RequiresConfirmation,
		ExecutionTime:        a.ExecutionTime,
	}

	if a.RateLimit != nil {
		cloned.RateLimit = &ToolRateLimit{
			MaxCalls:      a.RateLimit.MaxCalls,
			PeriodSeconds: a.RateLimit.PeriodSeconds,
		}
	}

	return cloned
}

// cloneMapStringAny creates a deep copy of a map[string]any.
func cloneMapStringAny(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}

	cloned := make(map[string]any, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case map[string]any:
			cloned[k] = cloneMapStringAny(val)
		case []any:
			cloned[k] = cloneSliceAny(val)
		default:
			cloned[k] = v
		}
	}
	return cloned
}

// cloneSliceAny creates a deep copy of a []any.
func cloneSliceAny(s []any) []any {
	if s == nil {
		return nil
	}

	cloned := make([]any, len(s))
	for i, v := range s {
		switch val := v.(type) {
		case map[string]any:
			cloned[i] = cloneMapStringAny(val)
		case []any:
			cloned[i] = cloneSliceAny(val)
		default:
			cloned[i] = v
		}
	}
	return cloned
}

// ToolRegistry stores globally registered tools.
type ToolRegistry struct {
	*baseregistry.Registry[Tool]
}

// newToolRegistry creates a new empty ToolRegistry.
func newToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		Registry: baseregistry.New[Tool]("tool",
			func(t *Tool) *Tool { return t.clone() },
			validateToolName),
	}
}

// Upsert inserts or replaces a tool registration. Used by the
// unified loader (Pass 2) so it can register tools from the new
// tree on top of legacy registrations without erroring on
// duplicates.
func (r *ToolRegistry) Upsert(tool *Tool) error {
	if tool == nil {
		return fmt.Errorf("tool is nil")
	}
	return r.Registry.Upsert(tool.Name, tool)
}

// add inserts a tool into the registry. Errors when the name is
// already taken.
func (r *ToolRegistry) add(tool *Tool) error {
	if tool == nil {
		return fmt.Errorf("tool is nil")
	}
	return r.Registry.Add(tool.Name, tool)
}

// validateToolName ensures a tool name follows the camelCase convention.
func validateToolName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("tool name is required")
	}
	if !toolNamePattern.MatchString(trimmed) {
		return fmt.Errorf("tool name %q must be camelCase (letters/digits, starting lowercase)", name)
	}
	return nil
}

// ValidateTool performs comprehensive validation of a tool definition.
func ValidateTool(tool *Tool) error {
	if tool == nil {
		return fmt.Errorf("tool is nil")
	}

	if err := validateToolName(tool.Name); err != nil {
		return err
	}

	if strings.TrimSpace(tool.Description) == "" {
		return fmt.Errorf("tool %q: description is required", tool.Name)
	}

	if len(tool.InputSchema) == 0 {
		return fmt.Errorf("tool %q: inputSchema is required", tool.Name)
	}

	// Validate inputSchema is valid JSON
	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		return fmt.Errorf("tool %q: inputSchema is not valid JSON: %w", tool.Name, err)
	}

	// Validate handler if present
	if tool.Handler != nil {
		if err := validateToolHandler(tool.Name, tool.Handler); err != nil {
			return err
		}
	}

	return nil
}

// validateToolHandler validates a tool handler configuration.
func validateToolHandler(toolName string, handler *ToolHandler) error {
	if handler == nil {
		return nil
	}

	handlerType := strings.TrimSpace(strings.ToLower(handler.Type))
	switch handlerType {
	case "query":
		if strings.TrimSpace(handler.Query) == "" {
			return fmt.Errorf("tool %q: handler type 'query' requires a query", toolName)
		}
	case "function":
		if strings.TrimSpace(handler.FunctionName) == "" {
			return fmt.Errorf("tool %q: handler type 'function' requires a function name", toolName)
		}
	case "webhook":
		if strings.TrimSpace(handler.URL) == "" {
			return fmt.Errorf("tool %q: handler type 'webhook' requires a URL", toolName)
		}
	case "delegate":
		// "delegate" handlers take no additional config -- they
		// invoke delegate_takeover.go, which requires the caller to
		// pass a targetAgentId in args. args are validated by the
		// tool's inputSchema at call time.
	case "":
		return fmt.Errorf("tool %q: handler type is required", toolName)
	default:
		return fmt.Errorf("tool %q: unknown handler type %q", toolName, handler.Type)
	}

	return nil
}
