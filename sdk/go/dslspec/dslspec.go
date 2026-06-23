// Package dslspec is the Go SDK surface for the memQL DSL spec export -- the
// portable, versioned JSON form of the DSL authoring surface (the top-level
// constructs, keywords, operators, field types, per-construct annotation
// legality, and "legal-next" completion rules). The cockpit (and a future
// web/Monaco editor) fetch it so their language intelligence is derived from
// the EXACT same source of truth memQL Sense is driven from
// (component/language/dslspec; epic memql#2122 / A4, issue #2125).
//
// The package mirrors sdk/go/pack + sdk/go/sense in shape -- a Client that
// wraps a client.Dispatcher, a typed method that returns an SDK-owned value
// (not raw protobuf), and errors that bubble up instead of being silently
// swallowed.
//
// There is no write path: the spec is assembled from the engine's own grammar
// tables + annotation registry. This surface is read-only by construction.
//
// Types are SDK-owned. No memqlv1.* import leaks out of this package's exported
// surface -- callers depend on dslspec.Spec, not on protobuf.
//
// See sdk/go/CLAUDE.md for the broader SDK rules (named-primitive contract,
// opaque types, generated-code-is-read-only).
package dslspec

import (
	"context"
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// Client exposes the single read-only DSL spec fetch over a Dispatcher. Safe
// to reuse across goroutines once constructed; the underlying Dispatcher is the
// multiplex point.
type Client struct {
	dispatcher *client.Dispatcher
}

// NewClient wires a dslspec client to the supplied dispatcher. The dispatcher
// must already be Run(); Client.* methods just dispatch SendAndWait calls
// through it.
func NewClient(dispatcher *client.Dispatcher) *Client {
	return &Client{dispatcher: dispatcher}
}

// Spec is the fetched DSL spec: the portable JSON document plus its schema
// version. JSON is the indented JSON produced by the engine's
// dslspec.(*Spec).JSON() -- the form a Monaco/CodeMirror grammar is generated
// from; its top-level shape is { version, constructs[], annotations[],
// keywords[], operators[], fieldTypes[], nextRules[] }. Version mirrors the
// embedded spec.version so a consumer can check the contract version without
// parsing the document.
type Spec struct {
	JSON    string
	Version string
}

// Fetch returns the node's DSL spec as portable JSON. The Go error return is
// reserved for wire-level / serialization failures.
func (c *Client) Fetch(ctx context.Context) (*Spec, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_DslSpec{
			DslSpec: &memqlv1.DslSpecMsg{},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("dslspec.fetch: %w", err)
	}
	result := resp.GetDslSpecResult()
	if result == nil {
		return nil, fmt.Errorf("dslspec.fetch: unexpected reply %T", resp.GetPayload())
	}
	return &Spec{
		JSON:    result.GetSpecJson(),
		Version: result.GetVersion(),
	}, nil
}
