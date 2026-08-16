// Package constructs is the Go SDK surface for the construct CATALOG -- what a
// cluster has actually loaded, at registry grain (memql#3749).
//
// It is the Go mirror of sdk/ts/src/constructs/constructs.ts, field for field,
// because sdk/go/CLAUDE.md rule 4 is that every Go method has a TS equivalent.
// It arrived later than its TS twin (epic memql#3928) for the ordinary reason:
// the editor consumed the catalog first, and nothing on the Go side had asked
// the question until the staged tier needed a client that could tell an
// OWNER-SCOPED registration from a shared one.
//
// # It is not a file walk, and that is the point
//
// The pack browser answers "show me this file". This answers "what do you
// have", and the two differ in both directions: a PROMOTED or STAGED construct
// lives in a database row and in no file at all, and a construct the loader
// SKIPPED is in a file and not in the engine. Every judgement -- which
// constructs exist, which kind each is, where it came from, whether it is
// runnable, what arguments it takes -- is made once, server-side, and read
// here. This package converts structs; it decides nothing.
//
// # What a caller sees
//
// The shared catalog, plus the STAGED constructs this caller may see: their
// own, and -- for a cluster owner -- every author's, attributed on Owner. The
// scoping is the server's; there is no filter to pass and none to get wrong.
package constructs

import (
	"context"
	"fmt"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/sdk/go/client"
)

// Construct origins. A CLOSED SET the engine derives in one place; nothing here
// re-derives it, because a second derivation is a second definition.
//
// Compare against these constants rather than the string literals: they are the
// SDK's own copy of the engine vocabulary, which is what keeps a consumer off
// the wire types.
const (
	// OriginCore: the engine's own embedded DSL tree, or a construct registered
	// from Go. Sealed -- it can never be redefined by an authored one.
	OriginCore = "core"
	// OriginBundle: a product's DSL, mounted at MEMQL_DSL_PATH. Changing it
	// needs a rollout, not a promote.
	OriginBundle = "bundle"
	// OriginPromoted: trained into the shared registry. Persisted, callable by
	// every session, replayed at boot.
	OriginPromoted = "promoted"
	// OriginStaged: durable and OWNER-SCOPED (epic memql#3928). Persisted and
	// replayed at boot like a promoted construct, and callable by its author and
	// by nobody else. A sibling of promoted rather than a qualifier on it: the
	// two differ in WHO can call the construct.
	OriginStaged = "staged"
)

// Client exposes the catalog over a Dispatcher. Safe to reuse across goroutines
// once constructed; the underlying Dispatcher is the multiplex point.
type Client struct {
	dispatcher *client.Dispatcher
}

// NewClient wires a constructs client to the supplied dispatcher. The
// dispatcher must already be Run().
func NewClient(dispatcher *client.Dispatcher) *Client {
	return &Client{dispatcher: dispatcher}
}

// Arg is one declared input of a runnable construct.
//
// Field for field the language server's RunnableArg, from the same analysis over
// the same source, so one argument-form model serves both paths: a form
// generated from this cannot disagree with the compiler about what a construct
// accepts. `Enum` is spelled `enum_values` on the wire only because `enum` is
// reserved in the proto language.
type Arg struct {
	Name string
	// Type is one of string | number | boolean | object | array | any.
	Type     string
	Required bool
	// Enum is the closed value set from @enum(...); empty when unconstrained.
	Enum        []string
	Description string
	// AutoInjected marks a tool field the engine stamps server-side, DROPPING
	// whatever the caller sent. Only a tool field can carry it.
	AutoInjected bool
}

// Trigger is what fires an AUTOMATION.
//
// The run form is decided ENTIRELY by these three: a Schedule with no Event
// offers "fire it now with an empty event"; an Event WITH a Concept offers a row
// picker over that concept's rows; an Event with no Concept offers a pasted
// payload alone, because there is no row set to pick from.
type Trigger struct {
	Concept  string
	Event    string
	Schedule string
}

// Construct is one construct the cluster has loaded.
type Construct struct {
	// Name is the registry key: a concept's canonical id
	// ("v1:cognition:space"), or the declared name for every other kind.
	Name string
	// Kind is the KIND, not the authored keyword -- "mutation" for what a file
	// spells `mutate`. Do not derive Runnable from it; read Runnable.
	Kind string
	// Namespace is the DSL domain it was authored in. Empty for a construct
	// that lives in a database row rather than a file.
	Namespace string
	// Origin is one of the Origin* constants above.
	Origin string
	// OriginPath is the construct's file, relative to the DSL tree root. Empty
	// for a promoted or staged construct, and for the handful the engine
	// registers from Go.
	OriginPath  string
	Description string
	// Runnable reports membership of the five-kind runnable set. Server-derived.
	Runnable bool
	// Args is the declared input schema in authored order. Always non-nil;
	// empty for every view-only kind, which has no argument form because it has
	// no run.
	Args []Arg
	// BoundConcept is the signature binding for query / mutation / shape / spec
	// / seed. Empty otherwise.
	BoundConcept string
	// SourceHash is the canonical content hash of the authored source, which is
	// what drift detection diffs against.
	//
	// EMPTY MEANS "NOT AVAILABLE", never "hashes to nothing" -- a construct
	// registered from Go has no authored source to hash -- so it must never be
	// compared equal to another empty hash.
	SourceHash string
	// Source is the authored text, carried ONLY when OriginPath is empty: a
	// construct that lives in a database row has no file to read it out of, and
	// this is the only copy a client can reach. Empty WITH a non-empty
	// OriginPath means "read the file"; both empty means the source is
	// genuinely unavailable.
	Source string
	// Trigger is what fires an automation, and is nil for every other kind.
	//
	// NIL IS NOT AN EMPTY TRIGGER. An automation with no trigger is manual-run
	// and its form says so from the absence; an empty value would be a claim
	// that it fires on nothing.
	Trigger *Trigger
	// Owner is the user a STAGED construct belongs to, and is empty for every
	// other origin. It exists for the cluster-owner view, the only caller that
	// sees more than one author's staged constructs and therefore the only one
	// for which the name alone is ambiguous.
	Owner string
}

// List returns every construct this caller can see: the shared catalog, plus
// the staged constructs scoped to them by the server.
//
// Sorted by (kind, namespace, name), so two calls against an unchanged cluster
// return an identical list and a client can diff two reads. Always non-nil.
func (c *Client) List(ctx context.Context) ([]Construct, error) {
	msg := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_ListConstructs{
			ListConstructs: &memqlv1.ListConstructsMsg{},
		},
	}
	resp, err := c.dispatcher.SendAndWait(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("constructs.list: %w", err)
	}
	result := resp.GetListConstructsResult()
	if result == nil {
		return nil, fmt.Errorf("constructs.list: empty response")
	}
	out := make([]Construct, 0, len(result.GetConstructs()))
	for _, info := range result.GetConstructs() {
		out = append(out, protoConstruct(info))
	}
	return out, nil
}

// Find returns the construct matching (kind, name), and whether one was found.
//
// A convenience over List for the common single-construct question, kept here
// rather than left to every caller because "not found" and "found with an empty
// origin" are different answers and a hand-rolled loop tends to conflate them.
func (c *Client) Find(ctx context.Context, kind, name string) (Construct, bool, error) {
	all, err := c.List(ctx)
	if err != nil {
		return Construct{}, false, err
	}
	for _, entry := range all {
		if entry.Kind == kind && entry.Name == name {
			return entry, true, nil
		}
	}
	return Construct{}, false, nil
}

// --- wire -> SDK -------------------------------------------------------------
//
// Unexported, so no memqlv1 type leaks across the package boundary
// (sdk_proto_leak_test.go).

func protoConstruct(info *memqlv1.ConstructInfo) Construct {
	out := Construct{
		Name:         info.GetName(),
		Kind:         info.GetKind(),
		Namespace:    info.GetNamespace(),
		Origin:       info.GetOrigin(),
		OriginPath:   info.GetOriginPath(),
		Description:  info.GetDescription(),
		Runnable:     info.GetRunnable(),
		Args:         protoArgs(info.GetArgs()),
		BoundConcept: info.GetBoundConcept(),
		SourceHash:   info.GetSourceHash(),
		Source:       info.GetSource(),
		Owner:        info.GetOwner(),
	}
	// ABSENT STAYS ABSENT. GetTrigger() returns a zero value for a message that
	// was not sent, so the presence check has to be on the pointer -- mapping
	// unconditionally would give every one of ~900 constructs a trigger that
	// fires on nothing.
	if t := info.GetTrigger(); t != nil {
		out.Trigger = &Trigger{
			Concept:  t.GetConcept(),
			Event:    t.GetEvent(),
			Schedule: t.GetSchedule(),
		}
	}
	return out
}

func protoArgs(in []*memqlv1.ConstructArg) []Arg {
	out := make([]Arg, 0, len(in))
	for _, a := range in {
		out = append(out, Arg{
			Name:         a.GetName(),
			Type:         a.GetType(),
			Required:     a.GetRequired(),
			Enum:         a.GetEnumValues(),
			Description:  a.GetDescription(),
			AutoInjected: a.GetAutoInjected(),
		})
	}
	return out
}
