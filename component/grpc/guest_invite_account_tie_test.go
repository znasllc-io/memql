package memql

import (
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// The wire test for the account tie on the guest-invite path (epic
// memql#4800, task memql#4797).
//
// WHAT IS ACTUALLY AT RISK. `SendGuestInviteMsg` gained one optional field.
// A new request field is a wire-contract change, and the claim being made
// about it is compatibility: a client built before the field existed must
// produce exactly the invitation it produced before. That claim is a property
// of the argument map -- specifically of what is NOT in it -- and it is
// silently breakable, because createGuestInvitation ACCEPTS `accountId`, so
// an empty string put in the map is an empty string written to the row.
//
// A row carrying `accountId: ""` is not obviously wrong at a glance: it reads
// back as "we asked and there is no client" rather than "nobody has said", and
// every surface would render it identically to an absent key. It would take a
// schema audit to notice. That is what this test is for.

func sendGuestInviteMsg(accountId string) *memqlv1.SendGuestInviteMsg {
	return &memqlv1.SendGuestInviteMsg{
		RequestId:   "req-1",
		SpaceId:     "v1:cognition:space:s1",
		SpaceName:   "Acme workspace",
		InviterName: "Owner",
		Email:       "guest@acme.test",
		JoinUrlBase: "https://app.example.com",
		GuestName:   "Dana",
		AccountId:   accountId,
	}
}

func buildArgs(accountId string) map[string]any {
	return guestInvitationArgs(
		sendGuestInviteMsg(accountId),
		"v1:identity:invitation:i1",
		"p1",
		"v1:identity:user:me",
		"guest@acme.test",
		"deadbeef",
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	)
}

// An omitting client is UNAFFECTED: the key is absent, not empty.
func TestGuestInviteOmitsTheAccountTieWhenUnset(t *testing.T) {
	args := buildArgs("")
	if _, present := args["accountId"]; present {
		t.Fatalf("accountId must be ABSENT from the args when the client sent none, got %#v.\n\n"+
			"createGuestInvitation accepts this arg, so a key present in the map is a key present in "+
			"the delta -- an empty string would be WRITTEN to the row. The invitation a pre-field "+
			"client creates has to be byte-identical to the one it created before the field existed; "+
			"that is what makes this addition wire-compatible rather than merely backward-parsing.",
			args)
	}
}

// Whitespace is not a value. A client that sends " " has named no account.
func TestGuestInviteTreatsBlankAccountTieAsUnset(t *testing.T) {
	for _, blank := range []string{" ", "\t", "\n  "} {
		if _, present := buildArgs(blank)["accountId"]; present {
			t.Fatalf("a whitespace-only accountId (%q) must be treated as unset", blank)
		}
	}
}

// A supplying client lands the value, trimmed.
func TestGuestInviteForwardsTheAccountTieWhenSet(t *testing.T) {
	args := buildArgs("  v1:accounts:account:a1  ")
	got, present := args["accountId"]
	if !present {
		t.Fatal("accountId must reach createGuestInvitation when the client sent one")
	}
	if got != "v1:accounts:account:a1" {
		t.Fatalf("accountId = %q, want the trimmed id", got)
	}
}

// EVERY OTHER ARGUMENT IS UNCHANGED BY THE ADDITION -- the half of
// compatibility a test of the new key alone would not cover. A field the
// addition perturbed would be a behaviour change wearing a tie's clothes.
func TestGuestInviteAccountTieChangesNothingElse(t *testing.T) {
	without := buildArgs("")
	with := buildArgs("v1:accounts:account:a1")

	for key, want := range without {
		if got := with[key]; got != want {
			t.Errorf("%s = %#v with a tie, %#v without -- the tie must not perturb any other argument", key, got, want)
		}
	}
	// And the tie is the ONLY key it adds.
	if len(with) != len(without)+1 {
		t.Errorf("supplying a tie changed the argument count by %d, want exactly 1", len(with)-len(without))
	}
}

// The reachable positive: the field is on the generated message at all. A
// proto regeneration that dropped it would otherwise make every assertion
// above pass against a getter that returns the zero value forever.
func TestSendGuestInviteMsgCarriesTheAccountField(t *testing.T) {
	msg := &memqlv1.SendGuestInviteMsg{AccountId: "v1:accounts:account:a1"}
	if msg.GetAccountId() != "v1:accounts:account:a1" {
		t.Fatal("SendGuestInviteMsg.account_id does not round-trip; the proto and the handler disagree")
	}
}
