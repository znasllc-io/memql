package calls

import (
	"bytes"
	"testing"
)

func TestInvitationLinkDerivationSeparatesEveryScope(t *testing.T) {
	pepper := bytes.Repeat([]byte{7}, 32)
	token := InvitationLinkToken(pepper, "owner", "site", "room")
	// Changing framing, domain or encoding would silently mint another alias for
	// every existing room and orphan recovery records. This vector pins the wire.
	if token != "sobceuaioyttstvp5muiymahlqnsdf3n" || !TokenWellFormed(token) {
		t.Fatalf("guest alias wire format changed: %q", token)
	}
	if token != InvitationLinkToken(pepper, "owner", "site", "room") {
		t.Fatal("the same scope produced different aliases")
	}
	for _, other := range []string{
		InvitationLinkToken(pepper, "another", "site", "room"),
		InvitationLinkToken(pepper, "owner", "another", "room"),
		InvitationLinkToken(pepper, "owner", "site", "another"),
		InvitationLinkToken(bytes.Repeat([]byte{8}, 32), "owner", "site", "room"),
	} {
		if token == other {
			t.Fatal("a different scope or pepper produced the same alias")
		}
	}
	if InvitationLinkToken(pepper, "a", "bc", "d") == InvitationLinkToken(pepper, "ab", "c", "d") ||
		InvitationLinkToken(pepper, "a\x00b", "c", "d") == InvitationLinkToken(pepper, "a", "b\x00c", "d") {
		t.Fatal("scope boundaries are ambiguous")
	}
}

func TestInvitationFingerprintPreservesReceiptWireFormat(t *testing.T) {
	const want = "3a307b076c5447efce63485af35c5d5c146086650e43f9ebeed2ab7ffd301dbf"
	if got := InvitationFingerprint("Call invitation", "Text", "<p>HTML</p>"); got != want {
		t.Fatalf("invitation receipt fingerprint changed: %q", got)
	}
	for _, fields := range [][3]string{
		{"Changed", "Text", "<p>HTML</p>"},
		{"Call invitation", "Changed", "<p>HTML</p>"},
		{"Call invitation", "Text", "Changed"},
		{"Text", "Call invitation", "<p>HTML</p>"},
	} {
		if InvitationFingerprint(fields[0], fields[1], fields[2]) == want {
			t.Fatal("changed message content matched the original receipt")
		}
	}
}
