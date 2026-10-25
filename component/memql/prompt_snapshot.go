package memql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

const capturedPromptLimit = 512 << 10

// A snapshot is native execution data, not a caller-supplied prompt override.
// Its owner must bind the entire map into the approved execution identity.
type capturedPrompt struct {
	Version         int               `json:"version"`
	Name            string            `json:"name"`
	Description     string            `json:"description"`
	Level           string            `json:"level"`
	DefaultProvider string            `json:"defaultProvider"`
	Source          string            `json:"source"`
	Templates       map[string]string `json:"templates"`
	InputSchema     map[string]any    `json:"inputSchema"`
}

// CapturePrompt freezes a prompt's compiled template closure and input schema.
// Only referenced partials are captured, so an unrelated installed partial does
// not change the approval identity. No registry pointers survive serialization.
func (e *MemQLEngine) CapturePrompt(name string) (map[string]any, error) {
	if e == nil {
		return nil, errors.New("prompt registry is unavailable")
	}
	p, ok := e.prompts.Get(name)
	if !ok || p == nil || p.tmpl == nil || p.tmpl.Name() != p.Name {
		return nil, fmt.Errorf("unknown or uncompiled prompt %q", name)
	}
	if (p.inputSchema != nil) != (p.inputSchemaSource != nil) {
		return nil, errors.New("prompt has no complete compiled input contract")
	}
	if _, err := airoute.ParseLevel(p.Level); err != nil {
		return nil, err
	}
	s := capturedPrompt{Version: 1, Name: p.Name, Description: p.Description, Level: p.Level,
		DefaultProvider: p.DefaultProvider, Source: p.TemplateSource, InputSchema: p.inputSchemaSource,
		Templates: map[string]string{}}
	var capture func(string) error
	capture = func(name string) error {
		if _, seen := s.Templates[name]; seen {
			return nil
		}
		t := p.tmpl.Lookup(name)
		if t == nil || t.Tree == nil || t.Tree.Root == nil {
			return fmt.Errorf("captured prompt references missing template %q", name)
		}
		if len(s.Templates) >= 64 {
			return errors.New("captured prompt exceeds its template closure bound")
		}
		s.Templates[name] = t.Tree.Root.String()
		return visitPromptTemplates(t.Tree.Root, capture)
	}
	if err := capture(p.Name); err != nil {
		return nil, err
	}
	b, err := json.Marshal(s)
	if err != nil || len(b) > capturedPromptLimit {
		return nil, errors.New("captured prompt exceeds its bounded snapshot")
	}
	var out map[string]any
	err = json.Unmarshal(b, &out)
	return out, err
}

func visitPromptTemplates(n parse.Node, visit func(string) error) error {
	switch n := n.(type) {
	case *parse.ListNode:
		if n != nil {
			for _, child := range n.Nodes {
				if err := visitPromptTemplates(child, visit); err != nil {
					return err
				}
			}
		}
	case *parse.TemplateNode:
		return visit(n.Name)
	case *parse.IfNode:
		if err := visitPromptTemplates(n.List, visit); err != nil {
			return err
		}
		return visitPromptTemplates(n.ElseList, visit)
	case *parse.RangeNode:
		if err := visitPromptTemplates(n.List, visit); err != nil {
			return err
		}
		return visitPromptTemplates(n.ElseList, visit)
	case *parse.WithNode:
		if err := visitPromptTemplates(n.List, visit); err != nil {
			return err
		}
		return visitPromptTemplates(n.ElseList, visit)
	}
	return nil
}

func restoreCapturedPrompt(snapshot map[string]any) (*PromptTemplate, error) {
	b, err := json.Marshal(snapshot)
	if err != nil || len(b) > capturedPromptLimit {
		return nil, errors.New("invalid or oversized captured prompt")
	}
	var s capturedPrompt
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return nil, fmt.Errorf("invalid captured prompt: %w", err)
	}
	if s.Version != 1 || strings.TrimSpace(s.Name) == "" || len(s.Templates) == 0 || len(s.Templates) > 64 {
		return nil, errors.New("captured prompt has no supported template closure")
	}
	if _, ok := s.Templates[s.Name]; !ok {
		return nil, errors.New("captured prompt is missing its entry template")
	}
	if _, err := airoute.ParseLevel(s.Level); err != nil {
		return nil, err
	}
	t := template.New(s.Name)
	names := make([]string, 0, len(s.Templates))
	for name := range s.Templates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// Parse separately before joining: a body may not redefine another
		// captured template through a hidden {{define}} block.
		part, err := template.New(name).Parse(s.Templates[name])
		if err != nil {
			return nil, err
		}
		if len(part.Templates()) != 1 || part.Tree == nil {
			return nil, errors.New("captured template contains an undeclared definition")
		}
		if _, err = t.AddParseTree(name, part.Tree); err != nil {
			return nil, err
		}
	}
	for _, part := range t.Templates() {
		if err := visitPromptTemplates(part.Tree.Root, func(name string) error {
			if t.Lookup(name) == nil {
				return fmt.Errorf("captured prompt is missing template %q", name)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	p := &PromptTemplate{Name: s.Name, Description: s.Description, Level: s.Level,
		DefaultProvider: s.DefaultProvider, TemplateSource: s.Source, tmpl: t.Lookup(s.Name), inputSchemaSource: s.InputSchema}
	if s.InputSchema != nil {
		b, err := json.Marshal(s.InputSchema)
		if err != nil {
			return nil, err
		}
		compiler := jsonschema.NewCompiler()
		compiler.Draft = jsonschema.Draft2019
		compiler.LoadURL = func(string) (io.ReadCloser, error) {
			return nil, errors.New("captured input schema cannot load external resources")
		}
		const resource = "memql-captured-prompt.json"
		if err = compiler.AddResource(resource, bytes.NewReader(b)); err != nil {
			return nil, err
		}
		p.inputSchema, err = compiler.Compile(resource)
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

// RenderCapturedPromptBounded validates and renders a captured template without
// a model/provider call. The writer stops before retaining an oversized result;
// callers can preflight immutable input before recording a model attempt.
func RenderCapturedPromptBounded(snapshot map[string]any, data map[string]any, maxBytes int) (string, error) {
	if maxBytes <= 0 || maxBytes > 1<<20 {
		return "", errors.New("invalid captured prompt render bound")
	}
	p, err := restoreCapturedPrompt(snapshot)
	if err != nil {
		return "", err
	}
	if err = p.ValidateData(data); err != nil {
		return "", err
	}
	return renderCapturedPromptBounded(p, data, maxBytes)
}

type capturedPromptBoundedWriter struct {
	bytes.Buffer
	remaining int
}

func (w *capturedPromptBoundedWriter) Write(b []byte) (int, error) {
	if len(b) > w.remaining {
		return 0, errors.New("captured prompt exceeds its model-input shape bound")
	}
	n, err := w.Buffer.Write(b)
	w.remaining -= n
	return n, err
}
func renderCapturedPromptBounded(p *PromptTemplate, data map[string]any, maxBytes int) (string, error) {
	// Bound intermediate template work as well as bytes reaching the writer.
	// The projected-review contract is a small string map, not executable data.
	if len(data) > 16 {
		return "", errors.New("bounded prompt input has too many fields")
	}
	total := 0
	for k, v := range data {
		text, ok := v.(string)
		if !ok {
			return "", errors.New("bounded prompt input requires strings")
		}
		total += len(k) + len(text)
		if total > 1<<20 {
			return "", errors.New("bounded prompt input exceeds byte bound")
		}
	}
	if err := validateBoundedPromptTemplate(p.tmpl); err != nil {
		return "", err
	}
	w := &capturedPromptBoundedWriter{remaining: maxBytes}
	if err := p.tmpl.Execute(w, data); err != nil {
		return "", err
	}
	return w.String(), nil
}

// The opted-in bounded path supports text/field lookups, conditionals, with
// and acyclic captured partials. It deliberately refuses loops, assignments and
// functions (e.g. an enormous printf width) before evaluation. Legacy prompts
// still use their unchanged full template language and rendering path.
func validateBoundedPromptTemplate(entry *template.Template) error {
	nodes := 0
	active := map[string]bool{}
	simple := func(p *parse.PipeNode) bool {
		if p == nil || len(p.Decl) != 0 || len(p.Cmds) != 1 || len(p.Cmds[0].Args) != 1 {
			return false
		}
		switch n := p.Cmds[0].Args[0].(type) {
		case *parse.DotNode:
			return true
		case *parse.FieldNode:
			return len(n.Ident) == 1
		default:
			return false
		}
	}
	var walk func(parse.Node, int) error
	var partial func(string, int) error
	partial = func(name string, depth int) error {
		if depth > 32 || active[name] {
			return errors.New("bounded prompt partial cycle or depth exceeded")
		}
		t := entry.Lookup(name)
		if t == nil || t.Tree == nil {
			return errors.New("bounded prompt partial is missing")
		}
		active[name] = true
		err := walk(t.Tree.Root, depth+1)
		delete(active, name)
		return err
	}
	walk = func(n parse.Node, depth int) error {
		if n == nil {
			return nil
		}
		nodes++
		if nodes > 4096 || depth > 32 {
			return errors.New("bounded prompt template work limit exceeded")
		}
		switch n := n.(type) {
		case *parse.ListNode:
			if n != nil {
				for _, child := range n.Nodes {
					if err := walk(child, depth+1); err != nil {
						return err
					}
				}
			}
		case *parse.TextNode:
		case *parse.ActionNode:
			if !simple(n.Pipe) {
				return errors.New("bounded prompt action is not a simple field lookup")
			}
		case *parse.IfNode:
			if !simple(n.Pipe) {
				return errors.New("bounded prompt condition is not a simple field lookup")
			}
			if err := walk(n.List, depth+1); err != nil {
				return err
			}
			return walk(n.ElseList, depth+1)
		case *parse.WithNode:
			if !simple(n.Pipe) {
				return errors.New("bounded prompt with is not a simple field lookup")
			}
			if err := walk(n.List, depth+1); err != nil {
				return err
			}
			return walk(n.ElseList, depth+1)
		case *parse.TemplateNode:
			if !simple(n.Pipe) {
				return errors.New("bounded prompt partial input is not a simple field lookup")
			}
			return partial(n.Name, depth+1)
		default:
			return errors.New("bounded prompt construct is unsupported")
		}
		return nil
	}
	return partial(entry.Name(), 0)
}

// InvokeCapturedPromptStructured executes one tool-free call from trusted saved
// execution data. It never resolves a prompt or partial from the current
// installation. Routing, context ceilings, attribution and journaling remain on
// the ordinary model seam; no response cache can substitute another run's review.
func (e *MemQLEngine) InvokeCapturedPromptStructured(ctx context.Context, snapshot map[string]any, data map[string]any, schemaName, schema string, strict bool) (StructuredAIResult, error) {
	return e.invokeCapturedPromptStructured(ctx, snapshot, data, schemaName, schema, strict, 0)
}

// InvokeCapturedPromptStructuredBounded uses the same ordinary routing and spend
// seam with a native render-size refusal. A byte bound is not provider capacity.
func (e *MemQLEngine) InvokeCapturedPromptStructuredBounded(ctx context.Context, snapshot map[string]any, data map[string]any, schemaName, schema string, strict bool, maxBytes int) (StructuredAIResult, error) {
	if maxBytes <= 0 || maxBytes > 1<<20 {
		return StructuredAIResult{}, errors.New("invalid captured prompt render bound")
	}
	return e.invokeCapturedPromptStructured(ctx, snapshot, data, schemaName, schema, strict, maxBytes)
}
func (e *MemQLEngine) invokeCapturedPromptStructured(ctx context.Context, snapshot map[string]any, data map[string]any, schemaName, schema string, strict bool, maxBytes int) (StructuredAIResult, error) {
	if e == nil || e.modelSeam == nil {
		return StructuredAIResult{}, errors.New("AI runtime is not configured")
	}
	if err := ctx.Err(); err != nil {
		return StructuredAIResult{}, err
	}
	p, err := restoreCapturedPrompt(snapshot)
	if err != nil {
		return StructuredAIResult{}, err
	}
	if err = p.ValidateData(data); err != nil {
		return StructuredAIResult{}, err
	}
	var rendered string
	if maxBytes > 0 {
		rendered, err = renderCapturedPromptBounded(p, data, maxBytes)
	} else {
		rendered, err = p.Render(data)
	}
	if err != nil {
		return StructuredAIResult{}, err
	}
	req, err := requestForPrompt(ctx, p, nil, airoute.ModalityStructured, rendered)
	if err != nil {
		return StructuredAIResult{}, err
	}
	return e.CallAIStructured(ctx, req, []common.ChatMessage{
		{Role: "system", Content: rendered},
		{Role: "user", Content: "Return the structured result requested above."},
	}, common.StructuredSchema{Name: schemaName, Description: p.Name + " output", Schema: json.RawMessage(schema), Strict: strict})
}
