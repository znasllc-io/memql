package auth

import (
	"errors"
	"testing"
)

// memql#3620. ActorEnvelopeBind is the value-binding surface: what a filter
// term compares against and what a `stamp { }` writes. It refuses
// `actor.userId` when the envelope names nobody, where ActorEnvelopeValue
// answers "".
//
// The two must NOT be collapsed. ActorEnvelopeValue's empty string is what the
// row gate reads in order to DENY (component/memql rowauthz_enforce.go), so
// making it error would take the gate's answer away; this function's refusal is
// what stops the same emptiness from being compiled into SQL or written into a
// row. One question is "what does the envelope say", the other is "may this be
// used as a value".
func TestActorEnvelopeBind_RefusesUserIdWithNoCaller(t *testing.T) {
	cases := []struct {
		name string
		ac   *AccessContext
	}{
		{"nil context", nil},
		{"empty user id", &AccessContext{Role: RoleWriter}},
		{"whitespace user id", &AccessContext{UserId: "  \t ", Role: RoleWriter}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, known, err := ActorEnvelopeBind(tc.ac, "userId")
			if !known {
				t.Fatal("userId must stay a KNOWN envelope path -- an unknown path is a " +
					"different error, and the caller's wording depends on telling them apart")
			}
			if !errors.Is(err, ErrActorEnvelopeNoCaller) {
				t.Fatalf("bind returned (%v, err=%v); an absent caller must refuse so the value "+
					"surfaces agree with the gate surfaces that already deny it", v, err)
			}
		})
	}
}

func TestActorEnvelopeBind_BindsARealCaller(t *testing.T) {
	v, known, err := ActorEnvelopeBind(&AccessContext{UserId: "u-1", Role: RoleWriter}, "userId")
	if err != nil || !known {
		t.Fatalf("a real caller must bind: (%v, known=%v, err=%v)", v, known, err)
	}
	if v != "u-1" {
		t.Errorf("bound %v, want %q", v, "u-1")
	}
}

// The refusal is scoped to userId ON PURPOSE. Widening it would refuse the
// normal case: identityId is never populated by LoadFromClaims, primaryEmail is
// legitimately blank for machine credentials, and role / isClusterOwner already
// FAIL a gate when absent rather than passing one (memql#2801). Pinned so a
// later "make it symmetric" edit has to argue with this.
func TestActorEnvelopeBind_OtherPathsStillResolveWithNoCaller(t *testing.T) {
	for _, path := range []string{"role", "identityId", "primaryEmail", "isClusterOwner", "now", "isOwner"} {
		t.Run(path, func(t *testing.T) {
			_, known, err := ActorEnvelopeBind(nil, path)
			if !known {
				t.Fatalf("%s must resolve (it is in the closed envelope set)", path)
			}
			if err != nil {
				t.Fatalf("%s must not refuse an absent caller: %v", path, err)
			}
		})
	}
}

// A path outside the envelope is UNKNOWN, not a refusal -- the caller owns that
// message because each surface names its own construct in it.
func TestActorEnvelopeBind_UnknownPathIsNotARefusal(t *testing.T) {
	_, known, err := ActorEnvelopeBind(nil, "partitions")
	if known {
		t.Fatal("`partitions` was dropped from the envelope in #2623 and must report unknown")
	}
	if err != nil {
		t.Fatalf("an unknown path must not masquerade as a no-caller refusal: %v", err)
	}
}

// ActorEnvelopeValue must keep answering "" -- the gate surfaces depend on
// seeing the emptiness. If this ever starts erroring, rowAuthzActorUserId
// stops being able to tell "no caller" from "unknown path" and the owned-tier
// row gate loses its deny.
func TestActorEnvelopeValue_StillReportsTheEmptinessForTheGates(t *testing.T) {
	v, ok := ActorEnvelopeValue(nil, "userId")
	if !ok || v != "" {
		t.Fatalf("ActorEnvelopeValue(nil, \"userId\") = (%v, %v), want (\"\", true) -- "+
			"memql#3172's gates read this emptiness in order to deny", v, ok)
	}
}
