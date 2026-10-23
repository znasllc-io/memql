package access

import "testing"

// ===========================================================================
// PRESENCE COMES FROM THE RESPONSE, NEVER FROM THE BINARY
// ===========================================================================
// The descriptor is a property of THIS BUILD, not of the cluster that answered.
// Once the pin moves past memql#5181 every cockpit's descriptor carries `role`,
// so a descriptor-only check would report every response as "reported" --
// including from a node one release behind, which sends nothing. That renders a
// cluster that said nothing as a person who holds nothing, everywhere: the
// exact inversion this package exists to prevent.

// A message that DECLARES every field and SETS none is what an older cluster
// looks like to a newer cockpit.
func TestDecodeReportsNothingWhenTheFieldsAreDeclaredButUnset(t *testing.T) {
	m := futureResult(t) // declares role, role_name, rank, groups, account_ids, every_account

	var got Summary
	decodePending(m, &got)

	if got.Role.Reported {
		t.Errorf("Role.Reported = true for a response that set no role: %+v", got.Role)
	}
	if got.Role.HasRank {
		t.Errorf("Role.HasRank = true for a response that set no role: %+v", got.Role)
	}
	if got.Groups.Reported {
		t.Errorf("Groups.Reported = true for a response that sent no groups")
	}
	if got.Scope.Reported {
		t.Errorf("Scope.Reported = true for a response that sent no scope")
	}
}

// The cockpit CAN tell one thing apart, and only one: whether its own wire
// carries the field at all. That is the difference between "this contract
// predates the field" and "the cluster sent no value", and they get different
// sentences.
func TestDecodeSeparatesAnUnknownFieldFromAnUnsentOne(t *testing.T) {
	unset := futureResult(t)
	var declared Summary
	decodePending(unset, &declared)
	if !declared.Role.OnTheWire {
		t.Error("Role.OnTheWire = false for a message whose descriptor carries `role`")
	}

	// The pinned message, which has no `role` field at all.
	var pinned Summary
	decodePending(pinnedResult(t), &pinned)
	if pinned.Role.OnTheWire {
		t.Error("Role.OnTheWire = true for the pinned wire, which has no `role` field")
	}
}
