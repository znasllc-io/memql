package memql

import (
	"testing"

	"github.com/visionarys-io/memql/component/auth"
)

func TestHasElevatedWriteRole(t *testing.T) {
	if !hasElevatedWriteRole("developer") {
		t.Fatalf("expected developer to be elevated")
	}
	if hasElevatedWriteRole("reader") {
		t.Fatalf("expected reader to not be elevated")
	}
}

func TestMatchesAuthenticatedIdentity(t *testing.T) {
	identity := auth.UserIdentity{
		Subject: "subject-1",
		Email:   "email@example.com",
	}

	if !matchesAuthenticatedIdentity("subject-1", "actor@example.com", identity) {
		t.Fatalf("expected subject match to authorize")
	}
	if !matchesAuthenticatedIdentity("email@example.com", "actor@example.com", identity) {
		t.Fatalf("expected email match to authorize")
	}
	if matchesAuthenticatedIdentity("different", "actor@example.com", identity) {
		t.Fatalf("expected unrelated identity to be rejected")
	}
}
