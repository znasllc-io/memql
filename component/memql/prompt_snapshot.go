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

// InvokeCapturedPromptStructured executes one tool-free call from trusted saved
// execution data. It never resolves a prompt or partial from the current
// installation. Routing, context ceilings, attribution and journaling remain on
// the ordinary model seam; no response cache can substitute another run's review.
func (e *MemQLEngine) InvokeCapturedPromptStructured(ctx context.Context, snapshot map[string]any, data map[string]any, schemaName, schema string, strict bool) (StructuredAIResult, error) {
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
	rendered, err := p.Render(data)
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
