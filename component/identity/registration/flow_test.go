package registration

import (
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

// The policy that decides who may register into a cluster had NO tests, which
// is how memql#4282 survived: Decide took `hasInvitation bool`, the caller
// computed it as `strings.TrimSpace(x) != ""`, and an invitation branch that
// returned before every other check meant any non-empty string opened a closed
// cluster.
//
// The two cases the issue names -- garbage under invite_only, and a valid
// invitation for somebody ELSE -- are the first two below. They are impossible
// to express against a boolean, which is the point of the type change.

func cfg(mode identity.RegistrationMode, domains ...string) identity.Config {
	return identity.Config{RegistrationMode: mode, RegistrationDomains: domains}
}

func TestInviteOnlyRefusesAnInvitationThatDoesNotResolve(t *testing.T) {
	// A caller who pasted nonsense, or guessed the field exists, resolves to
	// no invitation at all -- and invite_only then decides on its own terms.
	d, err := Decide(cfg(identity.RegistrationModeInviteOnly), "stranger@example.com", nil)
	if err == nil && d.Action == ActionIssueMagicLink {
		t.Fatalf("invite_only admitted a caller with no resolved invitation: %+v", d)
	}
	if d.Action != ActionReject {
		t.Errorf("Action = %q, want %q", d.Action, ActionReject)
	}
}

func TestAnInvitationForSomebodyElseDoesNotAdmitYou(t *testing.T) {
	// The address check. Without it a single leaked link is a general-purpose
	// bypass rather than a credential for one person -- forward it once and a
	// closed cluster is open.
	invite := &Invitation{Id: "v1:identity:invitation:i1", Email: "invited@example.com", Role: "reader"}

	d, err := Decide(cfg(identity.RegistrationModeInviteOnly), "someone.else@example.com", invite)
	if err == nil && d.Action == ActionIssueMagicLink {
		t.Fatalf("an invitation for invited@example.com admitted someone.else@example.com: %+v", d)
	}
	if d.Reason == "invitation" {
		t.Errorf("the decision claims %q for an address the invitation was not issued to", d.Reason)
	}
}

func TestAValidInvitationAdmitsTheAddressItNames(t *testing.T) {
	invite := &Invitation{Id: "v1:identity:invitation:i1", Email: "invited@example.com", Role: "developer"}

	d, err := Decide(cfg(identity.RegistrationModeInviteOnly), "invited@example.com", invite)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Action != ActionIssueMagicLink {
		t.Fatalf("Action = %q, want %q", d.Action, ActionIssueMagicLink)
	}
	if d.Reason != "invitation" {
		t.Errorf("Reason = %q, want %q", d.Reason, "invitation")
	}
	// The role the ISSUER chose is what the recipient lands with. An admin who
	// deliberately invited somebody as a developer meant it.
	if d.Role != "developer" {
		t.Errorf("Role = %q, want the issuer's choice %q", d.Role, "developer")
	}
}

// Case-insensitive, because email addresses are matched that way everywhere
// else in this service and an operator typing a capital letter is not a
// different person.
func TestTheAddressMatchIsCaseInsensitive(t *testing.T) {
	invite := &Invitation{Email: "Invited@Example.com"}
	d, err := Decide(cfg(identity.RegistrationModeInviteOnly), "invited@example.com", invite)
	if err != nil || d.Action != ActionIssueMagicLink {
		t.Fatalf("case difference refused a matching address: %+v (%v)", d, err)
	}
}

func TestDomainRestrictedIsNotBypassedByAnUnresolvedInvitation(t *testing.T) {
	// The second half of memql#4282: the invitation branch returned BEFORE the
	// allowlist was consulted, so the same trick skipped the domain check too.
	c := cfg(identity.RegistrationModeDomainRestricted, "example.com")

	if _, err := Decide(c, "outsider@elsewhere.test", nil); err == nil {
		t.Error("domain_restricted admitted an address outside the allowlist")
	}
	d, err := Decide(c, "colleague@example.com", nil)
	if err != nil || d.Action != ActionIssueMagicLink {
		t.Errorf("domain_restricted refused an allowlisted address: %+v (%v)", d, err)
	}
}

func TestOpenRegistrationNeedsNoInvitation(t *testing.T) {
	// And an invitation is not REQUIRED here -- under open the link is a
	// courtesy, which is what the issuing side tells an operator.
	d, err := Decide(cfg(identity.RegistrationModeOpen), "anyone@example.com", nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Action != ActionIssueMagicLink {
		t.Errorf("Action = %q, want %q", d.Action, ActionIssueMagicLink)
	}
}

func TestWaitlistEnqueuesWithoutAnInvitationAndAdmitsWithOne(t *testing.T) {
	c := cfg(identity.RegistrationModeWaitlist)

	d, err := Decide(c, "hopeful@example.com", nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Action != ActionCreateAccessRequest {
		t.Errorf("Action = %q, want %q", d.Action, ActionCreateAccessRequest)
	}

	// Approving a waitlist request mints an invitation; presenting it is what
	// turns the queue into an admission.
	approved := &Invitation{Email: "hopeful@example.com"}
	d, err = Decide(c, "hopeful@example.com", approved)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Action != ActionIssueMagicLink {
		t.Errorf("an approved invitation did not admit: Action = %q", d.Action)
	}
}

func TestAnEmptyEmailIsRefusedWhateverIsPresented(t *testing.T) {
	if _, err := Decide(cfg(identity.RegistrationModeOpen), "  ", &Invitation{Email: "  "}); err == nil {
		t.Error("an empty address was admitted")
	}
}

// DIRECTORY MODE (memql#4611). Directory membership is the invitation, so the
// EMAIL path -- which is what Decide serves -- has nothing to admit: arriving
// here means somebody typed an address instead of using their organization
// account.
func TestDirectoryModeRefusesTheEmailPath(t *testing.T) {
	d, err := Decide(cfg(identity.RegistrationModeDirectory), "staff@example.com", nil)
	if err == nil {
		t.Fatal("directory mode admitted a self-registration by email")
	}
	if d.Action != ActionReject {
		t.Errorf("action = %q, want reject", d.Action)
	}
	// The REASON is why this is its own mode rather than a flag on invite_only:
	// there the answer is "ask an admin for an invitation", and here it is
	// "sign in with your organization account". Those are different
	// instructions to a person who has one and does not know it applies.
	if d.Reason != "directory_sign_in_required" {
		t.Errorf("reason = %q, want directory_sign_in_required", d.Reason)
	}
	if d.Reason == "invite_only_no_invitation" {
		t.Error("directory mode is reusing invite_only's reason, which sends the person to the wrong place")
	}
}

// An invitation still overrides, and that matters: a federated cluster still
// has to admit a contractor or an auditor who is not in the directory.
func TestDirectoryModeStillHonoursAnInvitation(t *testing.T) {
	invite := &Invitation{Email: "contractor@partner.example", Role: "reader"}
	d, err := Decide(cfg(identity.RegistrationModeDirectory), "contractor@partner.example", invite)
	if err != nil {
		t.Fatalf("an invitation was refused under directory mode: %v", err)
	}
	if d.Action != ActionIssueMagicLink {
		t.Errorf("action = %q, want issue_magic_link", d.Action)
	}
	if d.Role != "reader" {
		t.Errorf("role = %q, want the inviter's choice", d.Role)
	}
}
