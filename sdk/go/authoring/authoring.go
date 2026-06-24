// Package authoring is the Go SDK surface for memQL's authoring operations --
// the cockpit-facing way to VALIDATE and SESSION-DEFINE a user "bundle" (a set
// of .memql sources) over the engine's gRPC stream (issue memql#2128 / C1).
//
// Two operations, both reusing the engine's existing authoring machinery
// (component/memql/authoring_sandbox.go + authoring_session.go):
//
//   - ValidateBundle runs the Gate-1 isolated compile-and-bind sandbox and
//     returns per-construct diagnostics + an overall OK. It NEVER mutates engine
//     state -- safe to call against a running engine.
//   - SessionDefineBundle validates, then registers the bundle into the caller's
//     owner-scoped, STREAM-scoped authored registry. Defined function-family
//     constructs become callable BY NAME for the lifetime of the stream, never
//     shadowing core, and are dropped when the stream (session) ends.
//
// A third operation adds the owner-gated DURABLE promotion (issue
// znasllc-io/memql-cockpit#232):
//
//   - DurablePromoteBundle validates the bundle, then durably promotes every
//     plain construct (query / mutation / logic / spec / trait) into the SHARED
//     engine registry: persisted (reviewable + restart-durable), callable by
//     every session, and -- via a live cross-node broadcast -- callable on every
//     node within seconds with no restart. OWNER-only.
//
// The package mirrors sdk/go/sense in shape -- a Client wrapping a
// client.Dispatcher, typed SDK-owned values (no memqlv1.* leaks into the public
// surface), and errors that bubble up. See sdk/go/CLAUDE.md for the broader SDK
// rules (opaque types, named-primitive contract, generated-code-is-read-only).
package authoring

import (
	"context"
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// Client exposes the authoring operations over a Dispatcher. Safe to reuse
// across goroutines once constructed; the underlying Dispatcher is the
// multiplex point.
type Client struct {
	dispatcher *client.Dispatcher
}

// NewClient wires an authoring client to the supplied dispatcher. The
// dispatcher must already be Run(); Client.* methods just dispatch SendAndWait
// calls through it.
func NewClient(dispatcher *client.Dispatcher) *Client {
	return &Client{dispatcher: dispatcher}
}

// Diagnostic is one entry in a validate / session-define result -- the
// per-construct compile-and-bind outcome. OK is true when the construct
// compiled + bound cleanly. Skipped is true when the sandbox does not yet
// compile the kind (the construct was neither validated nor rejected and does
// NOT fail the bundle). Error carries the compile/bind failure or the skip
// reason.
type Diagnostic struct {
	Name    string
	Kind    string
	OK      bool
	Skipped bool
	Error   string
}

// Construct names one construct that a session-define registered (now callable
// by name within the session).
type Construct struct {
	Kind string
	Name string
}

// ValidateResult is the response from ValidateBundle: an overall OK plus the
// per-construct diagnostics. OK is true only when every non-skipped construct
// compiled cleanly. No engine state was mutated.
type ValidateResult struct {
	OK          bool
	Diagnostics []Diagnostic
}

// SessionDefineResult is the response from SessionDefineBundle. On success OK is
// true and Defined lists the constructs that became callable by name for the
// session. On failure OK is false, Error explains the rejection (validation or
// register), and Diagnostics carries the per-construct detail; nothing was
// registered.
type SessionDefineResult struct {
	OK          bool
	Defined     []Construct
	Diagnostics []Diagnostic
	Error       string
}

// ValidateBundle runs the Gate-1 sandbox over the supplied .memql bundle and
// returns the per-construct diagnostics + overall OK. It NEVER mutates engine
// state. The Go error return is reserved for wire-level failures (dispatcher
// closed, context cancelled, permission denied) -- a bundle that fails to
// compile comes back as OK=false with diagnostics, not as an error.
func (c *Client) ValidateBundle(ctx context.Context, sources string) (*ValidateResult, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_AuthoringValidateBundle{
			AuthoringValidateBundle: &memqlv1.AuthoringValidateBundleMsg{Sources: sources},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("authoring.validateBundle: %w", err)
	}
	result := resp.GetAuthoringValidateBundleResult()
	if result == nil {
		return nil, fmt.Errorf("authoring.validateBundle: empty response")
	}
	return &ValidateResult{
		OK:          result.GetOk(),
		Diagnostics: protoDiagnostics(result.GetDiagnostics()),
	}, nil
}

// SessionDefineBundle validates the bundle, then session-defines it into the
// caller's owner-scoped, stream-scoped authored registry. Defined
// function-family constructs become callable by name within the session, never
// shadowing core, dropped when the stream ends. The Go error return is reserved
// for wire-level failures; a bundle the engine rejected comes back as OK=false
// with a populated Error + diagnostics.
func (c *Client) SessionDefineBundle(ctx context.Context, sources string) (*SessionDefineResult, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_AuthoringSessionDefineBundle{
			AuthoringSessionDefineBundle: &memqlv1.AuthoringSessionDefineBundleMsg{Sources: sources},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("authoring.sessionDefineBundle: %w", err)
	}
	result := resp.GetAuthoringSessionDefineBundleResult()
	if result == nil {
		return nil, fmt.Errorf("authoring.sessionDefineBundle: empty response")
	}
	defined := make([]Construct, 0, len(result.GetDefined()))
	for _, d := range result.GetDefined() {
		defined = append(defined, Construct{Kind: d.GetKind(), Name: d.GetName()})
	}
	return &SessionDefineResult{
		OK:          result.GetOk(),
		Defined:     defined,
		Diagnostics: protoDiagnostics(result.GetDiagnostics()),
		Error:       result.GetError(),
	}, nil
}

// PromoteResult is the response from DurablePromoteBundle. On success OK is true
// and Promoted lists the constructs durably promoted into the shared registry
// (persisted, restart-durable, and -- via the live broadcast -- callable on every
// node within seconds). On failure OK is false, Error explains the rejection
// (validation or a per-construct promote failure), and Diagnostics carries the
// per-construct compile/bind detail.
type PromoteResult struct {
	OK          bool
	Promoted    []Construct
	Diagnostics []Diagnostic
	Error       string
}

// DurablePromoteBundle validates the bundle, then durably promotes every plain
// construct (query / mutation / logic / spec / trait) into the SHARED engine
// registry. The promoted constructs are persisted (reviewable + restart-durable),
// callable by every session, and -- via a live cross-node broadcast -- callable
// on every node within seconds with no restart. OWNER-only: the engine rejects a
// non-owner caller with a permission-denied wire error.
//
// The Go error return is reserved for wire-level failures (dispatcher closed,
// context cancelled, permission denied); a bundle the engine rejected on
// validation comes back as OK=false with a populated Error + diagnostics.
func (c *Client) DurablePromoteBundle(ctx context.Context, sources string) (*PromoteResult, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_DurablePromoteBundle{
			DurablePromoteBundle: &memqlv1.DurablePromoteBundleMsg{Sources: sources},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("authoring.durablePromoteBundle: %w", err)
	}
	result := resp.GetDurablePromoteBundleResult()
	if result == nil {
		return nil, fmt.Errorf("authoring.durablePromoteBundle: empty response")
	}
	promoted := make([]Construct, 0, len(result.GetPromoted()))
	for _, p := range result.GetPromoted() {
		promoted = append(promoted, Construct{Kind: p.GetKind(), Name: p.GetName()})
	}
	return &PromoteResult{
		OK:          result.GetOk(),
		Promoted:    promoted,
		Diagnostics: protoDiagnostics(result.GetDiagnostics()),
		Error:       result.GetError(),
	}, nil
}

// DemoteResult is the response from DurableDemoteBundle, the inverse of
// PromoteResult. On success OK is true and Demoted lists the constructs durably
// demoted out of the shared registry (persisted as retired so a restart never
// re-hydrates them, and -- via the live broadcast -- removed on every node within
// seconds). On failure OK is false and Error explains the rejection (no demotable
// constructs in the source, or a per-construct demote failure).
type DemoteResult struct {
	OK          bool
	Demoted     []Construct
	Diagnostics []Diagnostic
	Error       string
}

// DurableDemoteBundle durably demotes every plain construct (query / mutation /
// logic / spec / trait) named in the bundle source OUT of the SHARED engine
// registry -- the inverse of DurablePromoteBundle (memql#2163). The demoted
// constructs are persisted as retired (so a restart never re-hydrates them),
// removed from the shared registry on this node (author-promoted-only safety
// gate -- a core construct can never be removed), and -- via a live cross-node
// broadcast -- removed on every node within seconds with no restart. OWNER-only:
// the engine rejects a non-owner caller with a permission-denied wire error.
//
// A demote needs only the names + kinds the source declares; it never compiles
// the construct, so a construct whose source no longer compiles can still be
// demoted. The Go error return is reserved for wire-level failures (dispatcher
// closed, context cancelled, permission denied); a demote the engine rejected
// comes back as OK=false with a populated Error.
func (c *Client) DurableDemoteBundle(ctx context.Context, sources string) (*DemoteResult, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_DurableDemoteBundle{
			DurableDemoteBundle: &memqlv1.DurableDemoteBundleMsg{Sources: sources},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("authoring.durableDemoteBundle: %w", err)
	}
	result := resp.GetDurableDemoteBundleResult()
	if result == nil {
		return nil, fmt.Errorf("authoring.durableDemoteBundle: empty response")
	}
	demoted := make([]Construct, 0, len(result.GetDemoted()))
	for _, d := range result.GetDemoted() {
		demoted = append(demoted, Construct{Kind: d.GetKind(), Name: d.GetName()})
	}
	return &DemoteResult{
		OK:          result.GetOk(),
		Demoted:     demoted,
		Diagnostics: protoDiagnostics(result.GetDiagnostics()),
		Error:       result.GetError(),
	}, nil
}

// protoDiagnostics adapts the wire diagnostics into the SDK form. Lives here
// (unexported) so no memqlv1 type leaks across the package boundary.
func protoDiagnostics(in []*memqlv1.AuthoringDiagnostic) []Diagnostic {
	out := make([]Diagnostic, 0, len(in))
	for _, d := range in {
		out = append(out, Diagnostic{
			Name:    d.GetName(),
			Kind:    d.GetKind(),
			OK:      d.GetOk(),
			Skipped: d.GetSkipped(),
			Error:   d.GetError(),
		})
	}
	return out
}
