// Package access reads the caller's own access record off a MemQL cluster
// and renders it: who the cluster thinks you are, what role you hold, and
// what that role's rank says about where it stands.
package access

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// Summary is what the cluster says about the caller.
type Summary struct {
	UserID       string
	PrimaryEmail string
	DisplayName  string
	SessionID    string
	Role         Role
	Groups       Groups
	Scope        Scope
}

// Group is one membership the cluster reports (memql#5165).
type Group struct {
	ID          string
	Name        string
	Kind        string
	AccountID   string
	AccountName string
}

// Groups is the caller's memberships.
type Groups struct {
	Items []Group
	// Reported is true when at least one group arrived. proto3 cannot tell an
	// EMPTY repeated field from an absent one -- they are the same bytes --
	// which is the same limitation the worker's `apps_present` exists for. So
	// "in no groups" and "sent no groups" collapse here, and they collapse
	// toward the safe reading: a person told "not reported" looks further,
	// where one told "none" believes they hold nothing.
	Reported bool
	// OnTheWire is true when THIS BUILD's descriptor carries `groups` at all.
	OnTheWire bool
}

// Scope is the account scope the caller's memberships resolve to.
//
// EveryAccount is record A's D6 staff case: `account_ids` is EMPTY and the
// bool is set. An empty list with the bool clear is the opposite fact -- no
// scope at all -- so Reported keeps them apart.
type Scope struct {
	AccountIDs   []string
	EveryAccount bool
	// Reported is true when a scope actually arrived: a non-empty account list,
	// or every_account set. A false bool is wire-identical to an absent one in
	// proto3, so it is never itself evidence that the cluster answered.
	Reported bool
	// OnTheWire is true when THIS BUILD's descriptor carries the scope fields.
	OnTheWire bool
}

// Role is the caller's cluster-wide role as a CATALOG SLUG, the role's
// display name, and its rank.
type Role struct {
	Slug string
	Name string
	Rank int32
	// HasRank is true when a rank travelled WITH a role. It is not read on its
	// own: proto3 sends no bytes for an int32 of 0, so "rank 0" and "no rank"
	// are indistinguishable in isolation. Tied to the role's arrival they stop
	// being ambiguous -- a role that arrived carries whatever rank came with
	// it, and 0 then means what the record says it means: holds nothing.
	HasRank bool
	// Reported is true when a role slug actually ARRIVED in this response.
	Reported bool
	// OnTheWire is true when THIS BUILD's descriptor carries `role` at all.
	//
	// It is the ONE thing the cockpit can tell apart, and the reason it is
	// separate from Reported: a descriptor is a property of this binary, not
	// of the cluster that answered. False means the contract predates the
	// field, so no cluster could have sent one. True with Reported false means
	// a cluster on this contract sent nothing -- an older node, or a
	// credential with no role row. Those get different sentences.
	OnTheWire bool
}

// Decode reads a MyAccessResult into a Summary.
func Decode(m *memqlv1.MyAccessResult) Summary {
	if m == nil {
		return Summary{}
	}
	s := Summary{
		UserID:       m.GetUserId(),
		PrimaryEmail: m.GetPrimaryEmail(),
		DisplayName:  m.GetDisplayName(),
		SessionID:    m.GetSessionId(),
	}
	decodePending(m.ProtoReflect(), &s)
	return s
}

// decodePending reads the fields the engine has not landed yet.
//
// PRESENCE COMES FROM THE RESPONSE, NOT FROM THE DESCRIPTOR. Reading it off
// the descriptor would mean reading it off this BINARY: the moment the pin
// moves past memql#5181, every build carries `role` and every response would
// claim to have reported one -- including from a node a release behind, which
// sends nothing. A cluster that said nothing would render as a person who
// holds nothing everywhere, which is the inversion this package exists to
// prevent. So the descriptor decides only whether the field COULD arrive
// (OnTheWire); the value decides whether it DID.
func decodePending(m protoreflect.Message, s *Summary) {
	decodeRole(m, &s.Role)
	decodeGroups(m, &s.Groups)
	decodeScope(m, &s.Scope)
}

func decodeRole(m protoreflect.Message, r *Role) {
	fd := lookupByName(m, fieldRole, protoreflect.StringKind)
	if fd == nil {
		return
	}
	r.OnTheWire = true
	slug := m.Get(fd).String()
	if slug == "" {
		// An empty slug is wire-identical to an unsent one, and every user row
		// carries a role -- so empty means the cluster did not send it.
		return
	}
	r.Reported = true
	r.Slug = slug

	// The name and the rank are only meaningful ALONGSIDE a slug, which is
	// also what makes rank 0 readable: on its own it is indistinguishable
	// from silence, and next to an arrived role it is the record's "holds
	// nothing".
	if nameFd := lookupByName(m, fieldRoleName, protoreflect.StringKind); nameFd != nil {
		r.Name = m.Get(nameFd).String()
	}
	if rankFd := lookupByName(m, fieldRank, protoreflect.Int32Kind); rankFd != nil {
		r.HasRank = true
		r.Rank = int32(m.Get(rankFd).Int())
	}
}

func decodeGroups(m protoreflect.Message, g *Groups) {
	fd := lookupListByName(m, fieldGroups, protoreflect.MessageKind)
	if fd == nil {
		return
	}
	g.OnTheWire = true
	list := m.Get(fd).List()
	if list.Len() == 0 {
		return
	}
	g.Reported = true
	g.Items = make([]Group, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		item := list.Get(i).Message()
		var one Group
		one.ID, _ = stringByName(item, fieldID)
		one.Name, _ = stringByName(item, fieldName)
		one.Kind, _ = stringByName(item, fieldKind)
		one.AccountID, _ = stringByName(item, fieldAccountID)
		one.AccountName, _ = stringByName(item, fieldAccountName)
		g.Items = append(g.Items, one)
	}
}

func decodeScope(m protoreflect.Message, sc *Scope) {
	idsFd := lookupListByName(m, fieldAccountIDs, protoreflect.StringKind)
	everyFd := lookupByName(m, fieldEveryAccount, protoreflect.BoolKind)
	if idsFd == nil && everyFd == nil {
		return
	}
	sc.OnTheWire = true
	if idsFd != nil {
		list := m.Get(idsFd).List()
		for i := 0; i < list.Len(); i++ {
			sc.AccountIDs = append(sc.AccountIDs, list.Get(i).String())
		}
	}
	if everyFd != nil {
		sc.EveryAccount = m.Get(everyFd).Bool()
	}
	// A false bool and an empty list are both wire-identical to silence, so
	// neither is evidence on its own that the cluster answered.
	sc.Reported = len(sc.AccountIDs) > 0 || sc.EveryAccount
}

// The field NAMES are the contract; see the comment on lookupByName.
const (
	fieldRole     = "role"
	fieldRoleName = "role_name"
	fieldRank     = "rank"

	fieldGroups       = "groups"
	fieldAccountIDs   = "account_ids"
	fieldEveryAccount = "every_account"

	// Inside MyAccessGroup.
	fieldID          = "id"
	fieldName        = "name"
	fieldKind        = "kind"
	fieldAccountID   = "account_id"
	fieldAccountName = "account_name"
)

// stringByName reads a string field BY NAME. The bool reports whether the
// DESCRIPTOR carries it, which is all a nested group field needs -- an empty
// name inside a group that did arrive is just an empty name.
func stringByName(m protoreflect.Message, name string) (string, bool) {
	fd := lookupByName(m, name, protoreflect.StringKind)
	if fd == nil {
		return "", false
	}
	return m.Get(fd).String(), true
}

// lookupListByName is lookupByName for a REPEATED field.
func lookupListByName(m protoreflect.Message, name string, kind protoreflect.Kind) protoreflect.FieldDescriptor {
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil || !fd.IsList() || fd.IsMap() || fd.Kind() != kind {
		return nil
	}
	return fd
}

// lookupByName finds a field by NAME AND KIND, or returns nil.
//
// ===========================================================================
// BY NAME, NEVER BY NUMBER -- AND THAT IS NOT A STYLE CHOICE
// ===========================================================================
// Both design records that add fields to this message settle the NAMES and
// explicitly leave the NUMBERS to whoever writes the engine change:
//
//	"Field numbers are chosen by the implementer against the current
//	 message; the names are the contract."
//	     -- 2026-09-07-groups-and-grants-design.md, section on the wire
//
//	"gains `string role = 11`, `string role_name = 12`, `int32 rank = 13`
//	 (numbers chosen against the message by the implementer ...)"
//	     -- 2026-09-07-roles-as-data-design.md, section G
//
// So a number written down here is a GUESS the engine is free to contradict,
// and the failure would be silent in the worst way: field 11 on the landed
// message might be a different string, and this would render it as the
// operator's role.
//
// It also rules out the obvious alternative. Proto keeps unrecognised fields
// as `unknownFields` bytes, but those bytes are keyed BY NUMBER ONLY -- the
// name never travels on the wire. There is therefore no way to pull `role`
// out of an unknown-field blob without already knowing its number, which is
// the thing the records decline to settle. Reading the DESCRIPTOR by name is
// the only correct option, and it needs no cockpit change when the engine
// lands: the field simply starts existing.
//
// The KIND is checked too, so a future field that reuses one of these names
// for a different type is treated as absent rather than coerced.
func lookupByName(m protoreflect.Message, name string, kind protoreflect.Kind) protoreflect.FieldDescriptor {
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil || fd.Kind() != kind || fd.IsList() || fd.IsMap() {
		return nil
	}
	return fd
}
