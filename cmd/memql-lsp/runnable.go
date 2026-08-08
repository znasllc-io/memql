package main

// runnable.go serves `memql/runnableConstructs`, the custom LSP request behind
// the VS Code runtime panel's Run affordances (memql#3309).
//
// The whole point of the request is that THE LANGUAGE SERVER OWNS ALL PARSING.
// The extension must never parse .memql itself: two parsers means the generated
// argument form can silently disagree with the compiler about what a construct
// accepts, and the developer finds out by running the wrong thing. So the
// analysis lives in Sense (component/memql/sense/runnable.go) next to the real
// parser, and this file is a transport adapter -- coordinate conversion and
// JSON shape, nothing more.
//
// The wire shape below is a FIXED contract shared with the TypeScript consumer.
// Field names, the six-value `type` set, and the "empty array, never null"
// rule for `constructs` and `args` are all load-bearing; changing any of them
// means changing both sides in the same commit.

import (
	"encoding/json"
	"errors"

	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/cmd/memql-lsp/internal/position"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// methodRunnableConstructs is the custom request's method name.
const methodRunnableConstructs = "memql/runnableConstructs"

// capabilityRunnableConstructs is the key the server advertises under
// `capabilities.experimental` so a client can feature-detect the request
// instead of calling it blind and handling MethodNotFound.
const capabilityRunnableConstructs = "memqlRunnableConstructs"

type runnableConstructsParams struct {
	TextDocument protocol.TextDocumentIdentifier `json:"textDocument"`
}

type runnableConstructsResult struct {
	Constructs []runnableConstruct `json:"constructs"`
}

type runnableConstruct struct {
	Kind           string           `json:"kind"`
	Name           string           `json:"name"`
	Concept        string           `json:"concept,omitempty"`
	SignatureRange protocol.Range   `json:"signatureRange"`
	Args           []runnableArg    `json:"args"`
	Trigger        *runnableTrigger `json:"trigger,omitempty"`
}

type runnableArg struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Required    bool     `json:"required"`
	Enum        []string `json:"enum,omitempty"`
	Description string   `json:"description,omitempty"`
}

type runnableTrigger struct {
	Concept  string `json:"concept,omitempty"`
	Event    string `json:"event,omitempty"`
	Schedule string `json:"schedule,omitempty"`
}

// customHandler wraps the generated protocol.Handler so the server can serve a
// method glsp has no slot for. protocol.Handler dispatches off a fixed switch
// over the standard LSP methods and exposes no extension point, so the only
// way to add one is to sit in front of it.
//
// Everything that is not the custom method delegates untouched, preserving the
// (result, validMethod, validParams, err) contract exactly -- glsp's server
// turns validMethod=false into MethodNotFound and validParams=false into
// InvalidParams, so collapsing any of those four values into an error would
// change how every standard request reports failure.
type customHandler struct {
	*protocol.Handler
	srv *server
}

// newCustomHandler builds the wrapped handler the server is run with.
func newCustomHandler(s *server) *customHandler {
	return &customHandler{Handler: s.handler(), srv: s}
}

func (h *customHandler) Handle(ctx *glsp.Context) (any, bool, bool, error) {
	if ctx == nil || ctx.Method != methodRunnableConstructs {
		return h.Handler.Handle(ctx)
	}
	// Mirror protocol.Handler's own precondition: nothing but `initialize` is
	// answerable before initialization, and the Sense service does not exist
	// until then either.
	if !h.Handler.IsInitialized() {
		return nil, true, true, errors.New("server not initialized")
	}
	var params runnableConstructsParams
	if err := json.Unmarshal(ctx.Params, &params); err != nil {
		return nil, true, false, err
	}
	return h.srv.runnableConstructs(params), true, true, nil
}

// runnableConstructs answers the request for one document.
//
// It never fails. An unknown or unopened URI, a Sense service that is not built
// yet, and a buffer that does not parse all produce the same empty result: the
// editor issues this on every keystroke and on every CodeLens refresh, so a
// half-typed buffer is the ordinary case, not an error condition. An error
// would surface to the user as a popup on a document they are simply still
// typing.
func (s *server) runnableConstructs(params runnableConstructsParams) runnableConstructsResult {
	// Allocated up front so the field marshals as [] and never as null: the
	// TypeScript consumer iterates it directly.
	out := runnableConstructsResult{Constructs: []runnableConstruct{}}

	text, ok := s.docs.get(params.TextDocument.URI)
	if !ok {
		return out
	}
	svc := s.getSense()
	if svc == nil {
		return out
	}

	for _, rc := range svc.RunnableConstructs(text) {
		out.Constructs = append(out.Constructs, toWireConstruct(text, rc))
	}
	return out
}

// toWireConstruct converts one Sense result to its wire form. The coordinate
// conversion is the part worth watching: Sense reports 1-based lines and
// RUNE columns, LSP wants 0-based lines and UTF-16 code-unit characters, and
// the two only agree below U+10000 -- so it goes through
// internal/position.ToLSPPosition, which resolves the line's text rather than
// doing arithmetic.
func toWireConstruct(text string, rc sense.RunnableConstruct) runnableConstruct {
	out := runnableConstruct{
		Kind:    rc.Kind,
		Name:    rc.Name,
		Concept: rc.Concept,
		SignatureRange: protocol.Range{
			Start: position.ToLSPPosition(text, rc.SignatureRange.Start.Line, rc.SignatureRange.Start.Column),
			End:   position.ToLSPPosition(text, rc.SignatureRange.End.Line, rc.SignatureRange.End.Column),
		},
		Args: make([]runnableArg, 0, len(rc.Args)),
	}
	for _, a := range rc.Args {
		out.Args = append(out.Args, runnableArg{
			Name:        a.Name,
			Type:        a.Type,
			Required:    a.Required,
			Enum:        a.Enum,
			Description: a.Description,
		})
	}
	if rc.Trigger != nil {
		out.Trigger = &runnableTrigger{
			Concept:  rc.Trigger.Concept,
			Event:    rc.Trigger.Event,
			Schedule: rc.Trigger.Schedule,
		}
	}
	return out
}
