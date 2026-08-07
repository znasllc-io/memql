package auth

import (
	"testing"
	"time"
)

// forward_authority_names_test.go -- memql#3221.
//
// The forwarded-auth contract replaced a `map[string]string` claims carrier
// with a typed ForwardedAuthority carrying the resolved DECISION. Right shape,
// but the old carrier also shipped given_name / family_name, and the new one
// did not -- so a piece of PROVENANCE stopped crossing the hop alongside the
// authorization inputs that were meant to stop crossing.
//
// The fields themselves are trivial. The CONSTRAINT is the deliverable: they
// must be inert everywhere an authorization decision is made, so they cannot
// quietly become an input later. These tests are that constraint, written so
// they fail if anyone wires a name into the decision path.

func namedAuthority() ForwardedAuthority {
	return ForwardedAuthority{
		Version:         ForwardedAuthorityVersion,
		Kind:            ForwardedPrincipalUser,
		Subject:         "v1:identity:user:alice",
		PrimaryEmail:    "alice@example.com",
		Role:            RoleWriter,
		CredentialClass: ForwardedClassUser,
		FirstName:       "Alice",
		LastName:        "Nakamura",
	}
}

// The guard rail, stated three ways. Each of these is a way a name could
// become an authorization input, and each must stay shut.
func TestForwardedAuthorityNamesAreProvenanceOnly(t *testing.T) {
	now := time.Now()

	t.Run("the verifier ignores them", func(t *testing.T) {
		// Same assertion twice, differing ONLY in the names. If either the
		// verdict or the bound decision moves, a name reached a decision.
		withNames := namedAuthority()
		withoutNames := namedAuthority()
		withoutNames.FirstName = ""
		withoutNames.LastName = ""

		gotWith, errWith := VerifyForwardedAuthority(withNames, now)
		gotWithout, errWithout := VerifyForwardedAuthority(withoutNames, now)

		if (errWith == nil) != (errWithout == nil) {
			t.Fatalf("the verifier's verdict changed with the names present: with=%v without=%v.\n\n"+
				"The name fields are provenance. A verifier that consults them has made "+
				"'who this is called' part of 'what this may do'. memql#3221.", errWith, errWithout)
		}
		if errWith != nil {
			t.Fatalf("VerifyForwardedAuthority refused a well-formed assertion: %v", errWith)
		}
		if *gotWith != *gotWithout {
			t.Errorf("the bound AccessContext differs with the names present:\n with:    %+v\n without: %+v",
				*gotWith, *gotWithout)
		}
	})

	t.Run("a garbage name cannot buy anything", func(t *testing.T) {
		// The adversarial spelling: a name field carrying something that looks
		// like a role or a subject. It must be as inert as "Alice".
		a := namedAuthority()
		a.FirstName = "owner"
		a.LastName = "v1:identity:user:someone-else"

		access, err := VerifyForwardedAuthority(a, now)
		if err != nil {
			t.Fatalf("VerifyForwardedAuthority: %v", err)
		}
		if access.Role != RoleWriter {
			t.Errorf("bound role = %q, want writer -- a name field moved the role", access.Role)
		}
		if access.UserId != "v1:identity:user:alice" {
			t.Errorf("bound subject = %q, want alice -- a name field moved the subject", access.UserId)
		}
		if access.IsClusterOwner() {
			t.Error("a name field spelled \"owner\" produced a cluster owner")
		}
	})

	t.Run("the AccessContext cannot carry them", func(t *testing.T) {
		// Structural rather than behavioural: VerifyForwardedAuthority builds
		// an AccessContext from four named fields, and AccessContext has no
		// name field for a fifth to land in. Asserted through the round trip so
		// this fails the day someone adds one and wires it up.
		access, err := VerifyForwardedAuthority(namedAuthority(), now)
		if err != nil {
			t.Fatalf("VerifyForwardedAuthority: %v", err)
		}
		want := AccessContext{
			UserId:       "v1:identity:user:alice",
			PrimaryEmail: "alice@example.com",
			Role:         RoleWriter,
		}
		if *access != want {
			t.Errorf("VerifyForwardedAuthority returned %+v, want exactly %+v.\n\n"+
				"The derived AccessContext is the ONLY thing the receiver binds as an "+
				"authorization decision. A name reaching it is a name becoming an "+
				"authorization input. memql#3221.", *access, want)
		}
	})
}

// The other half: the names must actually cross. Principal() is what turns the
// authority into the attribution claims map the receiver rebuilds a TokenInfo
// from, and component/metadata reads that TokenInfo to stamp
// identity.displayName on every row a mutation writes.
func TestForwardedPrincipalCarriesTheDisplayName(t *testing.T) {
	principal := namedAuthority().Principal()

	if got := principal.Claims["given_name"]; got != "Alice" {
		t.Errorf("claims[given_name] = %q, want %q.\n\n"+
			"Without it, a row written by a worker on a forwarded turn omits the "+
			"identity.displayName that the same user's direct-path rows carry, and nothing "+
			"in the data explains the difference. memql#3221.", got, "Alice")
	}
	if got := principal.Claims["family_name"]; got != "Nakamura" {
		t.Errorf("claims[family_name] = %q, want %q", got, "Nakamura")
	}

	// Round-tripped through the shape the receiver actually reads.
	id := buildIdentityFromToken(TokenInfoFromForwardedClaims(principal.Claims))
	if id.FirstName != "Alice" || id.LastName != "Nakamura" {
		t.Errorf("rebuilt identity = %q / %q, want Alice / Nakamura -- the claim keys must be the "+
			"ones extractNamesFromClaims looks for", id.FirstName, id.LastName)
	}
}

// A nameless principal must produce a claims map with no name keys at all,
// rather than empty ones. component/metadata's set() skips empty values, so an
// empty-string claim and an absent claim land the same on a row -- but they do
// NOT land the same in a claims map someone later iterates.
func TestForwardedPrincipalOmitsAbsentNames(t *testing.T) {
	a := namedAuthority()
	a.FirstName = ""
	a.LastName = ""

	claims := a.Principal().Claims
	if _, ok := claims["given_name"]; ok {
		t.Error("claims carries an empty given_name; absent fields must be omitted, not blanked")
	}
	if _, ok := claims["family_name"]; ok {
		t.Error("claims carries an empty family_name; absent fields must be omitted, not blanked")
	}
}

// WithDisplayName is the ONLY way names get onto an authority, and it must not
// be able to disturb anything else. The constructors deliberately do not take
// them: an AccessContext has no names, so a producer reads them off its session
// identity, and keeping that a separate explicit step is what stops a name
// being mistaken for part of the resolved decision.
func TestWithDisplayNameTouchesNothingElse(t *testing.T) {
	before, err := ForwardedAuthorityForUser(
		&AccessContext{UserId: "v1:identity:user:alice", PrimaryEmail: "alice@example.com", Role: RoleWriter},
		ForwardedClassUser, "", time.Time{}, time.Now())
	if err != nil {
		t.Fatalf("ForwardedAuthorityForUser: %v", err)
	}

	after := before.WithDisplayName("Alice", "Nakamura")

	// Everything but the two name fields must be identical.
	stripped := after
	stripped.FirstName = ""
	stripped.LastName = ""
	if stripped != before {
		t.Errorf("WithDisplayName changed a field other than the names:\n before: %+v\n after:  %+v",
			before, stripped)
	}
	if after.FirstName != "Alice" || after.LastName != "Nakamura" {
		t.Errorf("WithDisplayName did not set the names: %q / %q", after.FirstName, after.LastName)
	}
	// Value receiver: the original must be untouched, so a caller that forgets
	// to use the return value gets no names rather than a surprise mutation.
	if before.FirstName != "" || before.LastName != "" {
		t.Error("WithDisplayName mutated its receiver")
	}
}
