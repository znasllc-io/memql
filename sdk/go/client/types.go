package client

import (
	"encoding/json"
	"fmt"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/encoding/protojson"
)

// =============================================================================
// SDK-owned wire types
//
// Every type returned by a public SDK method is defined here, not in
// memqlv1. The translators at the bottom of this file do the
// proto<->SDK conversion in one place so the rest of the package never
// hands a raw proto to a consumer.
//
// Rationale: the opaque-type rule in sdk/go/CLAUDE.md. Consumers must
// not import `github.com/znasllc-io/memql/component/grpc/gen`; the
// SDK is the buffer that lets the wire evolve without recompiling
// every downstream binary.
//
// Surfaced by memql#115 (the four leaks under audit) and memql#118
// (the rule itself).
// =============================================================================

// =============================================================================
// Role
// =============================================================================

// Role is the cluster-wide authorization role assigned to a user: the SLUG
// their v1:identity:user row carries.
//
// IT IS A PLAIN STRING AND NOT A CLOSED SET (epic memql#5166). It mirrored the
// memqlv1.UserRole enum, which is deleted: the set of roles is cluster state,
// so an enum could only name the five this repo shipped, and a role a cluster
// authored for itself arrived as USER_ROLE_UNSPECIFIED -- the same value an
// unauthenticated caller gets.
//
// The constants below stay because the five they name are the roles every
// cluster seeds, and comparing against one is the ordinary thing a consumer
// does. A consumer that needs to ORDER roles reads the ladder from
// `activeRoles` and resolves through it; there is deliberately no rank table
// here, for the reason MemQL OS ships none: two hand-maintained ladders
// disagree, and nothing notices.
type Role string

const (
	// RoleUnspecified is the zero value -- a user with no role yet, or one
	// whose role this SDK was handed as an empty string. Treated as "no access"
	// by every gate.
	RoleUnspecified Role = ""
	// RoleOwner has full cluster-wide privileges including the
	// admin / cluster-management surfaces.
	RoleOwner Role = "owner"
	// RoleAdmin can administer non-owner users / partitions but
	// cannot transfer cluster ownership.
	RoleAdmin Role = "admin"
	// RoleDeveloper is engineering power (authoring + inline DSL +
	// deploy/cut-version) without user management (#1532 / #1876). Sits
	// in the privileged tier alongside admin (different powers, not a
	// strict ordering): a developer may cut + deploy a version forward
	// but may not roll back (owner-only).
	RoleDeveloper Role = "developer"
	// RoleWriter can read + write rows the user is granted access to.
	RoleWriter Role = "writer"
	// RoleReader can read rows the user is granted access to but
	// cannot mutate.
	RoleReader Role = "reader"
)

// =============================================================================
// SubscriptionKind
// =============================================================================

// SubscriptionKind names the family of events a subscription receives.
// Mirrors memqlv1.SubscriptionKind but stays inside the SDK so
// SubscribeMsg callers don't import the proto enum.
type SubscriptionKind string

const (
	SubscriptionKindUnspecified      SubscriptionKind = ""
	SubscriptionKindTelemetry        SubscriptionKind = "telemetry"
	SubscriptionKindMessage          SubscriptionKind = "message"
	SubscriptionKindQuerySpec        SubscriptionKind = "query_spec"
	SubscriptionKindAIStream         SubscriptionKind = "ai_stream"
	SubscriptionKindGraphEvents      SubscriptionKind = "graph_events"
	SubscriptionKindDomainEvents     SubscriptionKind = "domain_events"
	SubscriptionKindAutomationEvents SubscriptionKind = "automation_events"
	SubscriptionKindAll              SubscriptionKind = "all"
)

// toProto converts an SDK SubscriptionKind to its memqlv1 equivalent.
// Unknown / unspecified values map to the proto's zero value.
func (k SubscriptionKind) toProto() memqlv1.SubscriptionKind {
	switch k {
	case SubscriptionKindTelemetry:
		return memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_TELEMETRY
	case SubscriptionKindMessage:
		return memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_MESSAGE
	case SubscriptionKindQuerySpec:
		return memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_QUERY_SPEC
	case SubscriptionKindAIStream:
		return memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_AI_STREAM
	case SubscriptionKindGraphEvents:
		return memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS
	case SubscriptionKindDomainEvents:
		return memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_DOMAIN_EVENTS
	case SubscriptionKindAutomationEvents:
		return memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_AUTOMATION_EVENTS
	case SubscriptionKindAll:
		return memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_ALL
	default:
		return memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_UNSPECIFIED
	}
}

// GraphAction is a CDC verb a structured graph subscription filters on
// (memql#2460). It mirrors memqlv1.GraphNodeAction but stays inside the
// SDK so callers don't import the proto enum.
type GraphAction string

const (
	GraphActionCreated GraphAction = "created"
	GraphActionUpdated GraphAction = "updated"
	GraphActionDeleted GraphAction = "deleted"
)

// toProto converts an SDK GraphAction to its memqlv1 equivalent. An
// unknown value maps to UNSPECIFIED, which the server rejects inside a
// populated actions list.
func (a GraphAction) toProto() memqlv1.GraphNodeAction {
	switch a {
	case GraphActionCreated:
		return memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_CREATED
	case GraphActionUpdated:
		return memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_UPDATED
	case GraphActionDeleted:
		return memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_DELETED
	default:
		return memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_UNSPECIFIED
	}
}

// =============================================================================
// Event
// =============================================================================

// Event is the SDK-owned envelope SubscriptionManager.Subscribe
// delivers to its caller. Mirrors memqlv1.EventNotification but
// presents payload as a decoded `map[string]any` (callers don't
// touch structpb / protojson) and Kind as a string (the proto enum's
// String() form, stripped of the "EVENT_KIND_" prefix).
type Event struct {
	SubscriptionId string
	Kind           string
	Timestamp      time.Time
	Payload        map[string]any
	// PayloadOmitted marks an ID-ONLY notification: Payload carries the
	// row's identity (concept / id / createdAt, plus the topic and kind
	// naming the action) and NOT the row (memql#4309).
	//
	// It happens when the row's concept declares the `granted` row-authz
	// tier, whose predicate is a relationship spec: deciding it needs a
	// join the fan-out cannot perform against one row in isolation.
	// A row too large to broadcast, written on another node, arrives the same
	// way whatever its tier.
	// RE-READ the row through the normal authorized read path and use what
	// that returns; if the read refuses, the caller was not entitled to
	// the row and the event should be dropped.
	//
	// A consumer that ignores this sees a row whose fields are absent, so
	// it degrades to rendering nothing rather than to leaking anything.
	// False on every ordinary event.
	PayloadOmitted bool

	// Seq is this notification's position in the CONNECTION's delivery
	// sequence, from 1 (memql#4536). The server numbers every notification
	// it writes on the stream, so a delivery whose Seq is not the previous
	// one plus one means something never arrived.
	//
	// 0 against a server that predates the field: read that as "this
	// connection carries no sequence", not as "the first event".
	//
	// PER STREAM. A reconnect starts a new counter at 1, so stream
	// establishment is an implicit gap rather than a comparison across
	// connections.
	Seq uint64

	// GapBefore is true when one or more deliveries were DROPPED between
	// the previous notification on this stream and this one -- the engine's
	// per-stream event channel overflowed (memql#4536).
	//
	// The correct response is to RE-SEED: re-run the read that produced the
	// current state. There is no replay to ask for.
	GapBefore bool
}

// eventFromProto translates memqlv1.EventNotification -> Event.
// Returns a zero-value Event + error if the proto's payload struct
// can't be decoded; callers can drop / log the event in that case.
func eventFromProto(ev *memqlv1.EventNotification) (Event, error) {
	if ev == nil {
		return Event{}, nil
	}
	out := Event{
		SubscriptionId: ev.GetSubscriptionId(),
		Kind:           eventKindString(ev.GetKind()),
		PayloadOmitted: ev.GetPayloadOmitted(),
		Seq:            ev.GetSeq(),
		GapBefore:      ev.GetGapBefore(),
	}
	if ts := ev.GetTs(); ts != nil {
		out.Timestamp = ts.AsTime()
	}
	if payload := ev.GetPayload(); payload != nil {
		jsonBytes, err := protojson.Marshal(payload)
		if err != nil {
			return Event{}, fmt.Errorf("marshal event payload: %w", err)
		}
		out.Payload = make(map[string]any)
		if err := json.Unmarshal(jsonBytes, &out.Payload); err != nil {
			return Event{}, fmt.Errorf("decode event payload: %w", err)
		}
	}
	return out, nil
}

// eventKindString returns the proto's String() with the
// "EVENT_KIND_" prefix stripped, so callers see e.g. "NODE_CREATED"
// instead of "EVENT_KIND_NODE_CREATED". Unknown values fall back to
// the full String() output so the caller has something to log.
func eventKindString(k memqlv1.EventKind) string {
	const prefix = "EVENT_KIND_"
	s := k.String()
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

// =============================================================================
// AccessSummary
// =============================================================================

// AccessSummary is the SDK-owned shape of QueryClient.GetMyAccess.
//
// `ClusterRole Role` became three fields in epic memql#5166, and the shape of
// the change is the point: the role is a SLUG the cluster wrote, its NAME is
// what a person reads, and its RANK is where it sits on the ladder. One enum
// could carry none of those for a role the cluster authored itself.
type AccessSummary struct {
	RequestId    string
	UserId       string
	PrimaryEmail string
	// Role is the caller's role slug.
	Role Role
	// RoleName is the role's display name off the catalog ("Owner", "Support
	// Lead"). EMPTY when the slug resolves to no active rung, which is a real
	// answer: render the slug rather than inventing a title for a role the
	// cluster does not recognise.
	RoleName string
	// Rank is the role's rung, HIGHER == more privileged. Zero when the slug
	// ranks nowhere -- and zero is the answer rather than "unknown", because an
	// unrankable role admits nothing.
	Rank int
}

// accessSummaryFromProto translates memqlv1.MyAccessResult ->
// AccessSummary.
func accessSummaryFromProto(p *memqlv1.MyAccessResult) *AccessSummary {
	if p == nil {
		return nil
	}
	return &AccessSummary{
		RequestId:    p.GetRequestId(),
		UserId:       p.GetUserId(),
		PrimaryEmail: p.GetPrimaryEmail(),
		Role:         Role(p.GetRole()),
		RoleName:     p.GetRoleName(),
		Rank:         int(p.GetRank()),
	}
}

// =============================================================================
// Concept
// =============================================================================

// DisplayCard carries the per-concept rendering hints declared via
// the `@displayCard(...)` DSL annotation. Concept-agnostic clients
// (the cockpit's Concepts tab, future generic browsers) project
// rows through these slot names instead of carrying per-concept
// rendering code. Nil when the concept didn't declare the
// annotation. See memql#160.
type DisplayCard struct {
	// Primary is the payload field that names the row (e.g. "name"
	// for agents, "title" for spaces, "goal" for plans). Always set
	// when DisplayCard is non-nil; the loader rejects annotations
	// missing the primary slot.
	Primary string
	// Secondary is contextual (role, kind, type discriminator).
	// Optional.
	Secondary string
	// Tertiary is extra context (owner, parent space). Optional.
	Tertiary string
	// Status is a boolean or short enum that drives a colored badge
	// in the row chrome. Optional.
	Status string
}

// Concept is the SDK-owned projection of memqlv1.ConceptInfo.
// Used by QueryClient.ListConcepts.
type Concept struct {
	Id          string
	Version     string
	Domain      string
	Entity      string
	Description string
	Type        string
	// DisplayCard is the per-concept rendering hint set. Nil when
	// the concept's DSL declaration didn't carry `@displayCard(...)`.
	// See memql#160.
	DisplayCard *DisplayCard

	// DataState is MemQL's relationship to this concept's data (epic
	// memql#4378): "mirror", "origin" or "native". Three values, no
	// fourth.
	//
	// "mirror" is the one that changes what a caller may DO: an
	// external system owns the data and the engine refuses every write
	// that does not come from that system's connector. A client
	// offering an edit over a mirror concept is offering an action the
	// server will refuse -- read DataState before rendering one.
	//
	// Empty only when talking to a server that predates the field.
	DataState string
	// DataOrigin names the system where changes to this concept are
	// made. Never empty on a server that carries the field: a concept
	// that declared nothing reports "memql", so no client re-derives
	// the default.
	//
	// NOT to be confused with a construct's `Origin`, which is a
	// different question -- where the SOURCE FILE lives.
	DataOrigin string
	// DataMirroredTo names the external systems MemQL pushes this
	// concept's changes out to. Empty unless DataState is "origin".
	DataMirroredTo []string
}

// conceptsFromProto translates a []*memqlv1.ConceptInfo slice into
// []Concept. Returns an empty slice (never nil) so callers can
// `range` without a nil check.
func conceptsFromProto(in []*memqlv1.ConceptInfo) []Concept {
	out := make([]Concept, 0, len(in))
	for _, c := range in {
		if c == nil {
			continue
		}
		concept := Concept{
			Id:             c.GetId(),
			Version:        c.GetVersion(),
			Domain:         c.GetDomain(),
			Entity:         c.GetEntity(),
			Description:    c.GetDescription(),
			Type:           c.GetType(),
			DataState:      c.GetDataState(),
			DataOrigin:     c.GetDataOrigin(),
			DataMirroredTo: c.GetDataMirroredTo(),
		}
		if dc := c.GetDisplayCard(); dc != nil {
			concept.DisplayCard = &DisplayCard{
				Primary:   dc.GetPrimary(),
				Secondary: dc.GetSecondary(),
				Tertiary:  dc.GetTertiary(),
				Status:    dc.GetStatus(),
			}
		}
		out = append(out, concept)
	}
	return out
}
