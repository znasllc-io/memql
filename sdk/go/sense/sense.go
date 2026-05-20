// Package sense is the Go SDK surface for memQL Sense -- the
// language-intelligence service that powers editor affordances over
// .memql source: syntax tokens, diagnostics, autocompletion, hover
// docs, and signature help.
//
// The package mirrors `sdk/go/voice` in shape -- a `Client` that
// wraps a `client.Dispatcher`, typed methods that take and return
// SDK-owned values (not raw protobuf), and errors that bubble up
// instead of being silently swallowed.
//
// Why a dedicated SDK module: consumers (the memQL Cockpit editor,
// future thin clients) used to hand-build the five Sense gRPC
// envelopes (SenseTokenizeMsg / SenseDiagnoseMsg / SenseCompleteMsg /
// SenseHoverMsg / SenseSignatureHelpMsg) and translate the proto
// results into editor-friendly structs at the call site. Five
// hand-rolled envelopes per consumer; consumers reached past the SDK
// into `memqlv1` directly. This package owns that translation once.
//
// Types are SDK-owned. No `memqlv1.*` import leaks out of this
// package's exported surface -- callers depend on
// `sense.Token` / `sense.Diagnostic` / etc., not on protobuf.
//
// See sdk/go/CLAUDE.md for the broader SDK rules (named-primitive
// contract, opaque types, generated-code-is-read-only).
package sense

import (
	"context"
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// Client exposes the five Sense operations over a Dispatcher. Safe to
// reuse across goroutines once constructed; the underlying Dispatcher
// is the multiplex point.
type Client struct {
	dispatcher *client.Dispatcher
}

// NewClient wires a Sense client to the supplied dispatcher. The
// dispatcher must already be Run(); Client.* methods just dispatch
// SendAndWait calls through it.
func NewClient(dispatcher *client.Dispatcher) *Client {
	return &Client{dispatcher: dispatcher}
}

// Position is a 1-indexed (line, column) cursor location, matching
// the wire convention every Sense message uses for SensePosition.
// Lifted to the SDK surface so callers don't need to know proto
// field names.
type Position struct {
	Line   int
	Column int
}

// Range is a half-open [Start, End) span. End is exclusive on the
// column axis (`StartCol == EndCol` means a zero-width insertion
// point); inclusive on the line axis.
type Range struct {
	Start Position
	End   Position
}

// Token is one entry in the Tokenize result. Type names match the
// engine-side categorisation (keyword / identifier / string / number
// / operator / annotation / comment / punctuation / concept).
type Token struct {
	Type    string
	Literal string
	Range   Range
}

// Diagnostic is one entry in the Diagnose result. Severity values:
// 1=Error, 2=Warning, 3=Info, 4=Hint.
type Diagnostic struct {
	Severity int
	Message  string
	Code     string
	Range    Range
}

// CompletionItem is one entry in the Complete result. SortPriority
// lets the engine express ordering preference without forcing a
// stable sort on the consumer.
type CompletionItem struct {
	Label         string
	Kind          string
	Detail        string
	Documentation string
	InsertText    string
	SortPriority  int
}

// Hover is the structured response from a Hover request. Contents
// is markdown-formatted; the renderer side decides how to present
// it (the cockpit's hover popup is one consumer).
type Hover struct {
	Contents string
	Range    Range
}

// Parameter is one entry in a Signature's parameter list. Label is
// the parameter name as it appears in the signature string;
// Documentation is the per-parameter description if Sense surfaced
// one.
type Parameter struct {
	Label         string
	Documentation string
}

// Signature is one overload in a SignatureHelp response. Most memQL
// functions have a single signature; the slice exists to mirror the
// gRPC envelope (which exists so the engine can grow into overload
// resolution without a wire break).
type Signature struct {
	Label         string
	Documentation string
	Parameters    []Parameter
}

// SignatureHelp is the structured response from a Signature request.
// ActiveSignature / ActiveParameter point into Signatures + the
// active signature's Parameters respectively, so the cockpit can
// highlight which one the cursor is currently inside.
type SignatureHelp struct {
	Signatures      []Signature
	ActiveSignature int
	ActiveParameter int
}

// Tokenize asks Sense for syntax tokens over the supplied source.
// Returns the tokens in source order. An error here is the
// dispatcher's error -- the engine itself reports per-source issues
// via Diagnose, not as gRPC errors.
func (c *Client) Tokenize(ctx context.Context, source string) ([]Token, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_SenseTokenize{
			SenseTokenize: &memqlv1.SenseTokenizeMsg{Source: source},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("sense.tokenize: %w", err)
	}
	result := resp.GetSenseTokenizeResult()
	if result == nil {
		return nil, nil
	}
	out := make([]Token, 0, len(result.GetTokens()))
	for _, t := range result.GetTokens() {
		out = append(out, Token{
			Type:    t.GetType(),
			Literal: t.GetLiteral(),
			Range:   protoRange(t.GetRange()),
		})
	}
	return out, nil
}

// Diagnose asks Sense for lint / parse / semantic diagnostics over
// the supplied source. Returned diagnostics are NOT errors -- they're
// regular language warnings + errors the editor should surface in
// the gutter. The Go error return is reserved for wire-level
// failures (dispatcher closed, context cancelled, etc.).
func (c *Client) Diagnose(ctx context.Context, source string) ([]Diagnostic, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_SenseDiagnose{
			SenseDiagnose: &memqlv1.SenseDiagnoseMsg{Source: source},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("sense.diagnose: %w", err)
	}
	result := resp.GetSenseDiagnoseResult()
	if result == nil {
		return nil, nil
	}
	out := make([]Diagnostic, 0, len(result.GetDiagnostics()))
	for _, d := range result.GetDiagnostics() {
		out = append(out, Diagnostic{
			Severity: int(d.GetSeverity()),
			Message:  d.GetMessage(),
			Code:     d.GetCode(),
			Range:    protoRange(d.GetRange()),
		})
	}
	return out, nil
}

// Complete asks Sense for completion candidates at the supplied
// cursor position. Items arrive in the engine's preferred sort
// order; SortPriority is preserved so the caller can re-sort if it
// has different priorities.
func (c *Client) Complete(ctx context.Context, source string, cursor Position) ([]CompletionItem, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_SenseComplete{
			SenseComplete: &memqlv1.SenseCompleteMsg{
				Source: source,
				Cursor: protoPosition(cursor),
			},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("sense.complete: %w", err)
	}
	result := resp.GetSenseCompleteResult()
	if result == nil {
		return nil, nil
	}
	out := make([]CompletionItem, 0, len(result.GetItems()))
	for _, item := range result.GetItems() {
		out = append(out, CompletionItem{
			Label:         item.GetLabel(),
			Kind:          item.GetKind(),
			Detail:        item.GetDetail(),
			Documentation: item.GetDocumentation(),
			InsertText:    item.GetInsertText(),
			SortPriority:  int(item.GetSortPriority()),
		})
	}
	return out, nil
}

// Hover asks Sense for the symbol-info markdown at the supplied
// cursor position. Returns nil (not an error) when Sense has nothing
// to say at that position -- that's the normal case for whitespace,
// punctuation, etc., and shouldn't produce a "no hover" error path
// in the consumer.
func (c *Client) Hover(ctx context.Context, source string, cursor Position) (*Hover, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_SenseHover{
			SenseHover: &memqlv1.SenseHoverMsg{
				Source:   source,
				Position: protoPosition(cursor),
			},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("sense.hover: %w", err)
	}
	result := resp.GetSenseHoverResult()
	if result == nil || result.GetContents() == "" {
		return nil, nil
	}
	return &Hover{
		Contents: result.GetContents(),
		Range:    protoRange(result.GetRange()),
	}, nil
}

// SignatureHelp asks Sense for function-call signature help at the
// supplied cursor position. Returns nil when the cursor is not
// inside a function-call's argument list -- consumers don't need to
// distinguish "no signatures" from an error.
func (c *Client) SignatureHelp(ctx context.Context, source string, cursor Position) (*SignatureHelp, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_SenseSignatureHelp{
			SenseSignatureHelp: &memqlv1.SenseSignatureHelpMsg{
				Source:   source,
				Position: protoPosition(cursor),
			},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("sense.signatureHelp: %w", err)
	}
	result := resp.GetSenseSignatureHelpResult()
	if result == nil || len(result.GetSignatures()) == 0 {
		return nil, nil
	}
	sigs := make([]Signature, 0, len(result.GetSignatures()))
	for _, s := range result.GetSignatures() {
		params := make([]Parameter, 0, len(s.GetParameters()))
		for _, p := range s.GetParameters() {
			params = append(params, Parameter{
				Label:         p.GetLabel(),
				Documentation: p.GetDocumentation(),
			})
		}
		sigs = append(sigs, Signature{
			Label:         s.GetLabel(),
			Documentation: s.GetDocumentation(),
			Parameters:    params,
		})
	}
	return &SignatureHelp{
		Signatures:      sigs,
		ActiveSignature: int(result.GetActiveSignature()),
		ActiveParameter: int(result.GetActiveParameter()),
	}, nil
}

// protoPosition adapts an SDK Position into the wire form. Lives
// here (not exported) because every caller into the package boundary
// should only see the SDK type.
func protoPosition(p Position) *memqlv1.SensePosition {
	return &memqlv1.SensePosition{
		Line:   int32(p.Line),
		Column: int32(p.Column),
	}
}

// protoRange adapts a wire SenseRange into the SDK Range. Returns
// the zero Range when the wire side has no range (Sense omits the
// field for whole-source diagnostics).
func protoRange(r *memqlv1.SenseRange) Range {
	if r == nil {
		return Range{}
	}
	out := Range{}
	if s := r.GetStart(); s != nil {
		out.Start = Position{Line: int(s.GetLine()), Column: int(s.GetColumn())}
	}
	if e := r.GetEnd(); e != nil {
		out.End = Position{Line: int(e.GetLine()), Column: int(e.GetColumn())}
	}
	return out
}
