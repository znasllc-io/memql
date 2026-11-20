package calls

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestInvitationIdentityIsTheIssuedMediaIdentityShape(t *testing.T) {
	identity, err := NewToken(bytes.NewReader(make([]byte, MediaIdentityBytes)), MediaIdentityBytes)
	if err != nil || !InvitationIdentityWellFormed(identity) {
		t.Fatalf("issued identity was refused: %v", err)
	}
	for _, value := range []string{"", identity + "a", identity[:len(identity)-1], strings.ToUpper(identity), " " + identity, identity + "\n", strings.Repeat("0", 16), strings.Repeat("é", 8), strings.Repeat("a", 32)} {
		if InvitationIdentityWellFormed(value) {
			t.Errorf("malformed identity accepted: %q", value)
		}
	}
}

func TestInvitationRecipientRequiresOneMailbox(t *testing.T) {
	for _, value := range []string{"", "Name <a@example.test>", "a@example.test,b@example.test", "a@example.test\r\nBcc: b@example.test", "a\nb@example.test", ".a@example.test", "a..b@example.test", "a.@example.test", "a@-example.test", "a@example-.test", "a@localhost", "a@éxample.test", strings.Repeat("a", 65) + "@example.test"} {
		if got, ok := InvitationRecipient(value); ok {
			t.Errorf("accepted %q as %q", value, got)
		}
	}
	if got, ok := InvitationRecipient("  Person+Call@EXAMPLE.Test  "); !ok || got != "person+call@example.test" {
		t.Fatalf("canonical recipient: %q, %v", got, ok)
	}
}

func TestInvitationLimitPreservesConflictAndActualWait(t *testing.T) {
	err := &InvitationLimit{RetryAfter: 23 * time.Hour}
	if !errors.Is(err, ErrConflict) || ReasonOf(err) != ReasonRate || err.RetryAfter != 23*time.Hour {
		t.Fatal("rate response lost its class, reason or daily wait")
	}
}
