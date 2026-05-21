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

// Role is the cluster-wide authorization role assigned to a user.
// Mirrors memqlv1.UserRole but stays inside the SDK so consumers
// don't pull in the proto enum constants.
type Role string

const (
	// RoleUnspecified is the zero value -- a user with no role yet.
	// Treated as "no access" by every gate.
	RoleUnspecified Role = ""
	// RoleOwner has full cluster-wide privileges including the
	// admin / cluster-management surfaces.
	RoleOwner Role = "owner"
	// RoleAdmin can administer non-owner users / partitions but
	// cannot transfer cluster ownership.
	RoleAdmin Role = "admin"
	// RoleWriter can read + write rows the user is granted access to.
	RoleWriter Role = "writer"
	// RoleReader can read rows the user is granted access to but
	// cannot mutate.
	RoleReader Role = "reader"
)

// roleFromProto converts memqlv1.UserRole -> Role.
func roleFromProto(r memqlv1.UserRole) Role {
	switch r {
	case memqlv1.UserRole_USER_ROLE_OWNER:
		return RoleOwner
	case memqlv1.UserRole_USER_ROLE_ADMIN:
		return RoleAdmin
	case memqlv1.UserRole_USER_ROLE_WRITER:
		return RoleWriter
	case memqlv1.UserRole_USER_ROLE_READER:
		return RoleReader
	default:
		return RoleUnspecified
	}
}

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
// Mirrors memqlv1.MyAccessResult with the cluster-wide role typed as
// Role instead of the proto enum.
type AccessSummary struct {
	RequestId    string
	UserId       string
	PrimaryEmail string
	ClusterRole  Role
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
		ClusterRole:  roleFromProto(p.GetClusterRole()),
	}
}

// =============================================================================
// Concept
// =============================================================================

// Concept is the SDK-owned projection of memqlv1.ConceptInfo.
// Used by QueryClient.ListConcepts.
type Concept struct {
	Id          string
	Version     string
	Domain      string
	Entity      string
	Description string
	Type        string
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
		out = append(out, Concept{
			Id:          c.GetId(),
			Version:     c.GetVersion(),
			Domain:      c.GetDomain(),
			Entity:      c.GetEntity(),
			Description: c.GetDescription(),
			Type:        c.GetType(),
		})
	}
	return out
}
