package access

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Field kinds the contract uses. The probe builds one field of each, because a
// target that only ever built STRINGS could assert nothing positive about
// `rank`, `groups`, `account_ids` or `every_account` -- four of the six slots
// -- and case-insensitive matching in their lookups would sail straight past
// it. (It did: an earlier version of this target passed a million executions
// with three of the four lookups deliberately broken.)
const (
	kindString = iota
	kindInt32
	kindBool
	kindRepeatedString
	kindRepeatedMessage
	kindCount
)

// FuzzDecodeMatchesFieldNamesExactly asserts the property the whole seam rests
// on: a field populates a Summary slot ONLY when its name is character-for-
// character one of the contract names AND its kind and cardinality match.
//
// The failure this catches is not a panic -- nothing here panics. It is the
// helpful-looking change: matching case-insensitively, trimming, accepting
// `clusterRole` as a synonym for `role`, or coercing a string into `rank`.
// Any of those would make the cockpit render a value the cluster never claimed
// as the operator's role, and it would look completely plausible on screen.
// The engine is free to add fields this cockpit has never heard of, so "some
// other field was present" must stay indistinguishable from "no field".
//
// It also pins the OTHER half of the contract: presence comes from the VALUE,
// never from the descriptor. `role` declared and empty is not a reported role,
// and neither is `role_name` or `rank` arriving without a slug to attach to.
func FuzzDecodeMatchesFieldNamesExactly(f *testing.F) {
	for _, seed := range []struct {
		name  string
		kind  int
		value string
	}{
		{"role", kindString, "owner"},
		{"role", kindString, ""},
		{"Role", kindString, "owner"},
		{"ROLE", kindString, "owner"},
		{"role_", kindString, "owner"},
		{"cluster_role", kindString, "owner"},
		{"clusterRole", kindString, "owner"},
		{"role_name", kindString, "Owner"},
		{"role", kindInt32, "1"},
		{"rank", kindInt32, "150"},
		{"rank", kindInt32, "0"},
		{"Rank", kindInt32, "150"},
		{"every_account", kindBool, "true"},
		{"every_account", kindBool, ""},
		{"EveryAccount", kindBool, "true"},
		{"account_ids", kindRepeatedString, "acct_1"},
		{"account_ids", kindString, "acct_1"},
		{"AccountIds", kindRepeatedString, "acct_1"},
		{"groups", kindRepeatedMessage, "g_1"},
		{"groups", kindRepeatedString, "g_1"},
		{"Groups", kindRepeatedMessage, "g_1"},
	} {
		f.Add(seed.name, seed.kind, seed.value)
	}

	f.Fuzz(func(t *testing.T, name string, kind int, value string) {
		if kind < 0 || kind >= kindCount {
			t.Skip("not a kind the contract uses")
		}
		m, ok := singleField(name, kind, value)
		if !ok {
			t.Skip("not a valid proto3 field name")
		}

		var got Summary
		decodePending(m, &got)

		// --- the role block -------------------------------------------------
		wantRoleWire := name == "role" && kind == kindString
		if got.Role.OnTheWire != wantRoleWire {
			t.Fatalf("%q/%d: Role.OnTheWire = %v, want %v", name, kind, got.Role.OnTheWire, wantRoleWire)
		}
		wantRole := wantRoleWire && value != ""
		if got.Role.Reported != wantRole {
			t.Fatalf("%q/%d: Role.Reported = %v, want %v", name, kind, got.Role.Reported, wantRole)
		}
		wantSlug := ""
		if wantRole {
			wantSlug = value
		}
		if got.Role.Slug != wantSlug {
			t.Fatalf("%q/%d: Role.Slug = %q, want %q", name, kind, got.Role.Slug, wantSlug)
		}
		// A lone role_name or rank attaches to nothing: without a slug there is
		// no role for them to describe, and reporting them would invent one.
		if got.Role.Name != "" {
			t.Fatalf("%q/%d: Role.Name = %q with no slug present", name, kind, got.Role.Name)
		}
		if got.Role.HasRank {
			t.Fatalf("%q/%d: Role.HasRank = true with no slug present", name, kind)
		}

		// --- groups ---------------------------------------------------------
		wantGroupsWire := name == "groups" && kind == kindRepeatedMessage
		if got.Groups.OnTheWire != wantGroupsWire {
			t.Fatalf("%q/%d: Groups.OnTheWire = %v, want %v", name, kind, got.Groups.OnTheWire, wantGroupsWire)
		}
		if got.Groups.Reported != wantGroupsWire {
			// The probe always appends exactly one element when it builds the
			// repeated message field.
			t.Fatalf("%q/%d: Groups.Reported = %v, want %v", name, kind, got.Groups.Reported, wantGroupsWire)
		}

		// --- account scope --------------------------------------------------
		wantIDs := name == "account_ids" && kind == kindRepeatedString
		wantEvery := name == "every_account" && kind == kindBool
		if want := wantIDs || wantEvery; got.Scope.OnTheWire != want {
			t.Fatalf("%q/%d: Scope.OnTheWire = %v, want %v", name, kind, got.Scope.OnTheWire, want)
		}
		// A false bool is wire-identical to silence, so only a true one counts
		// as a scope having been reported.
		wantScope := wantIDs || (wantEvery && value != "")
		if got.Scope.Reported != wantScope {
			t.Fatalf("%q/%d: Scope.Reported = %v, want %v", name, kind, got.Scope.Reported, wantScope)
		}
		if got.Scope.EveryAccount != (wantEvery && value != "") {
			t.Fatalf("%q/%d: Scope.EveryAccount = %v", name, kind, got.Scope.EveryAccount)
		}
	})
}

// singleField builds a message carrying exactly one field of the given name and
// kind, populated from value. Reports false when proto itself will not accept
// the name.
func singleField(name string, kind int, value string) (protoreflect.Message, bool) {
	var field *descriptorpb.FieldDescriptorProto
	switch kind {
	case kindString:
		field = strField(name, 1)
	case kindInt32:
		field = int32Field(name, 1)
	case kindBool:
		field = boolField(name, 1)
	case kindRepeatedString:
		field = repeatedStrField(name, 1)
	case kindRepeatedMessage:
		field = repeatedMsgField(name, 1, ".cockpit.fuzz.Item")
	default:
		return nil, false
	}

	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("cockpit_fuzz_one_field.proto"),
		Package: proto.String("cockpit.fuzz"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name:  proto.String("Item"),
				Field: []*descriptorpb.FieldDescriptorProto{strField("id", 1)},
			},
			{
				Name:  proto.String("Probe"),
				Field: []*descriptorpb.FieldDescriptorProto{field},
			},
		},
	}
	fd, err := protodesc.NewFile(file, nil)
	if err != nil {
		return nil, false
	}
	md := fd.Messages().ByName("Probe")
	if md == nil || md.Fields().Len() != 1 {
		return nil, false
	}
	m := dynamicpb.NewMessage(md)
	f := md.Fields().Get(0)

	switch kind {
	case kindString:
		m.Set(f, protoreflect.ValueOfString(value))
	case kindInt32:
		m.Set(f, protoreflect.ValueOfInt32(int32(len(value))))
	case kindBool:
		m.Set(f, protoreflect.ValueOfBool(value != ""))
	case kindRepeatedString:
		m.Mutable(f).List().Append(protoreflect.ValueOfString(value))
	case kindRepeatedMessage:
		list := m.Mutable(f).List()
		item := list.NewElement().Message()
		item.Set(item.Descriptor().Fields().ByName("id"), protoreflect.ValueOfString(value))
		list.Append(protoreflect.ValueOfMessage(item))
	}
	return m, true
}
