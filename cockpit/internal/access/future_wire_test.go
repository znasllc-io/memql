package access

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// ===========================================================================
// THE MESSAGE THE ENGINE HAS NOT LANDED YET
// ===========================================================================
// memql#5181 (roles) and memql#5165 (groups) both edit MyAccessResult. Neither
// has merged, so this file builds the message they describe and runs the real
// decode against it -- the only way to prove the cockpit reads a wire that does
// not exist here yet.
//
// THE FIELD NUMBERS BELOW ARE DELIBERATELY WRONG. The records illustrate role
// at 11, role_name at 12 and rank at 13, and then say the numbers are the
// implementer's to choose. Using 41/42/43 here is the assertion: if anything in
// the decode path ever reads a number instead of a name, these tests fail.

func strField(name string, num int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(num),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
	}
}

func int32Field(name string, num int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(num),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
	}
}

func boolField(name string, num int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(num),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum(),
	}
}

func repeatedStrField(name string, num int32) *descriptorpb.FieldDescriptorProto {
	f := strField(name, num)
	f.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	return f
}

func repeatedMsgField(name string, num int32, typeName string) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:     proto.String(name),
		Number:   proto.Int32(num),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: proto.String(typeName),
	}
}

// futureResult builds a MyAccessResult carrying every field the two records
// add, at numbers nobody has agreed to.
func futureResult(t *testing.T) protoreflect.Message {
	t.Helper()
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("cockpit_future_my_access.proto"),
		Package: proto.String("cockpit.future"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("MyAccessGroup"),
			Field: []*descriptorpb.FieldDescriptorProto{
				strField("id", 1),
				strField("name", 2),
				strField("kind", 3),
				strField("account_id", 4),
				strField("account_name", 5),
			},
		}, {
			Name: proto.String("MyAccessResult"),
			Field: []*descriptorpb.FieldDescriptorProto{
				strField("request_id", 1),
				strField("user_id", 2),
				strField("primary_email", 3),
				strField("session_id", 6),
				strField("display_name", 7),
				// Record B's three, at numbers of this test's choosing.
				strField("role", 41),
				strField("role_name", 42),
				int32Field("rank", 43),
				// Record A's three, likewise.
				repeatedMsgField("groups", 44, ".cockpit.future.MyAccessGroup"),
				repeatedStrField("account_ids", 45),
				boolField("every_account", 46),
			},
		}},
	}
	fd, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatalf("build future descriptor: %v", err)
	}
	return dynamicpb.NewMessage(fd.Messages().ByName("MyAccessResult"))
}

func setStr(t *testing.T, m protoreflect.Message, name, v string) {
	t.Helper()
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil {
		t.Fatalf("future message has no field %q", name)
	}
	m.Set(fd, protoreflect.ValueOfString(v))
}

func setInt32(t *testing.T, m protoreflect.Message, name string, v int32) {
	t.Helper()
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil {
		t.Fatalf("future message has no field %q", name)
	}
	m.Set(fd, protoreflect.ValueOfInt32(v))
}

// roleOnlyResult carries `role` but no `rank`, which is what a split landing of
// memql#5181 would look like.
func roleOnlyResult(t *testing.T) protoreflect.Message {
	t.Helper()
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("cockpit_role_only_my_access.proto"),
		Package: proto.String("cockpit.roleonly"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("MyAccessResult"),
			Field: []*descriptorpb.FieldDescriptorProto{
				strField("user_id", 2),
				strField("role", 41),
			},
		}},
	}
	fd, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatalf("build role-only descriptor: %v", err)
	}
	return dynamicpb.NewMessage(fd.Messages().ByName("MyAccessResult"))
}

// A CUSTOM slug is none of the five legacy names, and its rank is what says
// where it stands. It must arrive intact, with no fallback to "reader".
func TestDecodeReadsRoleSlugNameAndRankByNameNotByNumber(t *testing.T) {
	m := futureResult(t)
	setStr(t, m, "role", "release-manager")
	setStr(t, m, "role_name", "Release Manager")
	setInt32(t, m, "rank", 150)

	var got Summary
	decodePending(m, &got)

	if !got.Role.Reported {
		t.Fatal("Role.Reported = false, want true: this wire carries `role`")
	}
	if got.Role.Slug != "release-manager" {
		t.Errorf("Role.Slug = %q, want %q", got.Role.Slug, "release-manager")
	}
	if got.Role.Name != "Release Manager" {
		t.Errorf("Role.Name = %q, want %q", got.Role.Name, "Release Manager")
	}
	if !got.Role.HasRank {
		t.Error("Role.HasRank = false, want true")
	}
	if got.Role.Rank != 150 {
		t.Errorf("Role.Rank = %d, want 150", got.Role.Rank)
	}
}

// Rank 0 is a REAL rank: the record gives an unknown slug rankOf 0, meaning
// "holds nothing". It must not read as "the cluster sent no rank".
//
// The comparison is against a message built WITHOUT the field, which is the
// only thing that makes the claim mean anything. An earlier version of this
// test asserted HasRank against a message that always declared `rank`, so it
// passed with its own setup deleted -- it was measuring the descriptor, not
// the decode.
func TestDecodeTellsRankZeroApartFromNoRank(t *testing.T) {
	withRank := futureResult(t)
	setStr(t, withRank, "role", "orphaned-slug")
	setInt32(t, withRank, "rank", 0)

	var got Summary
	decodePending(withRank, &got)
	if !got.Role.HasRank {
		t.Fatal("Role.HasRank = false for an explicit rank of 0: zero is a rank, not an absence")
	}
	if got.Role.Rank != 0 {
		t.Errorf("Role.Rank = %d, want 0", got.Role.Rank)
	}

	// The same slug on a wire that carries no rank at all.
	noRank := roleOnlyResult(t)
	setStr(t, noRank, "role", "orphaned-slug")

	var got2 Summary
	decodePending(noRank, &got2)
	if !got2.Role.Reported {
		t.Fatal("Role.Reported = false; the role itself should still arrive")
	}
	if got2.Role.HasRank {
		t.Error("Role.HasRank = true for a wire with no `rank` field: absence and zero must not collapse")
	}
}

// Record A's groups and account scope ride the same message and the same
// name-not-number rule.
func TestDecodeReadsGroupsAndAccountScopeByName(t *testing.T) {
	m := futureResult(t)
	appendGroup(t, m, "g_1", "Platform", "team", "acct_9", "Acme")
	appendStr(t, m, "account_ids", "acct_9")

	var got Summary
	decodePending(m, &got)

	if !got.Groups.Reported {
		t.Fatal("Groups.Reported = false, want true")
	}
	if len(got.Groups.Items) != 1 {
		t.Fatalf("len(Groups.Items) = %d, want 1", len(got.Groups.Items))
	}
	g := got.Groups.Items[0]
	if g.ID != "g_1" || g.Name != "Platform" || g.Kind != "team" || g.AccountID != "acct_9" || g.AccountName != "Acme" {
		t.Errorf("group = %+v, want every field carried through", g)
	}
	if !got.Scope.Reported {
		t.Error("Scope.Reported = false, want true")
	}
	if len(got.Scope.AccountIDs) != 1 || got.Scope.AccountIDs[0] != "acct_9" {
		t.Errorf("Scope.AccountIDs = %v, want [acct_9]", got.Scope.AccountIDs)
	}
	if got.Scope.EveryAccount {
		t.Error("Scope.EveryAccount = true, want false: this caller is scoped to one account")
	}
}

// every_account is record A's D6 staff case: account_ids is EMPTY and the bool
// is set. An empty list plus a false bool is the opposite fact -- no scope at
// all -- so the two must not collapse.
func TestDecodeReadsEveryAccountWithAnEmptyAccountList(t *testing.T) {
	m := futureResult(t)
	setBool(t, m, "every_account", true)

	var got Summary
	decodePending(m, &got)

	if !got.Scope.Reported {
		t.Fatal("Scope.Reported = false, want true")
	}
	if !got.Scope.EveryAccount {
		t.Error("Scope.EveryAccount = false, want true")
	}
	if len(got.Scope.AccountIDs) != 0 {
		t.Errorf("Scope.AccountIDs = %v, want empty", got.Scope.AccountIDs)
	}
	// This message declares `role` too and sets nothing in it. The role must
	// stay absent -- the field existing is not the cluster having answered.
	if got.Role.Reported {
		t.Error("Role.Reported = true from a declared-but-unset field")
	}
}

func setBool(t *testing.T, m protoreflect.Message, name string, v bool) {
	t.Helper()
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil {
		t.Fatalf("future message has no field %q", name)
	}
	m.Set(fd, protoreflect.ValueOfBool(v))
}

func appendStr(t *testing.T, m protoreflect.Message, name, v string) {
	t.Helper()
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil {
		t.Fatalf("future message has no field %q", name)
	}
	m.Mutable(fd).List().Append(protoreflect.ValueOfString(v))
}

func appendGroup(t *testing.T, m protoreflect.Message, id, name, kind, acctID, acctName string) {
	t.Helper()
	fd := m.Descriptor().Fields().ByName("groups")
	if fd == nil {
		t.Fatal("future message has no field \"groups\"")
	}
	list := m.Mutable(fd).List()
	item := list.NewElement().Message()
	for field, value := range map[string]string{
		"id": id, "name": name, "kind": kind,
		"account_id": acctID, "account_name": acctName,
	} {
		item.Set(item.Descriptor().Fields().ByName(protoreflect.Name(field)), protoreflect.ValueOfString(value))
	}
	list.Append(protoreflect.ValueOfMessage(item))
}

// pinnedResult is MyAccessResult as it exists at the CURRENT pin: no role, no
// groups, no scope. Built the same way as futureResult so the two differ only
// in the fields under test.
func pinnedResult(t *testing.T) protoreflect.Message {
	t.Helper()
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("cockpit_pinned_my_access.proto"),
		Package: proto.String("cockpit.pinned"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("MyAccessResult"),
			Field: []*descriptorpb.FieldDescriptorProto{
				strField("request_id", 1),
				strField("user_id", 2),
				strField("primary_email", 3),
				strField("session_id", 6),
				strField("display_name", 7),
			},
		}},
	}
	fd, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatalf("build pinned descriptor: %v", err)
	}
	return dynamicpb.NewMessage(fd.Messages().ByName("MyAccessResult"))
}
