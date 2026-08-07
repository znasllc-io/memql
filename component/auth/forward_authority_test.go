package auth

import (
	"errors"
	"testing"
	"time"
)

// forward_authority_test.go -- memql#3205 / memql#2876, absorbing memql#2814's
// "design it once for all forwards".
//
// The contract these pin: a mesh forward carries the producer's ALREADY-RESOLVED,
// ALREADY-CLAMPED authorization decision, and the receiver either verifies it or
// REFUSES. "No badge" is a VALUE, never a missing key -- that is the whole
// difference from the attempt that was rejected as a net security regression,
// which carried two OPTIONAL claims whose absence was indistinguishable from
// "not a badge session".

func liveBadge(now time.Time) ForwardedAuthority {
	return ForwardedAuthority{
		Version:         ForwardedAuthorityVersion,
		Kind:            ForwardedPrincipalUser,
		Subject:         "v1:identity:user:op-1",
		PrimaryEmail:    "op@example.io",
		Role:            RoleReader,
		CredentialClass: ForwardedClassBadge,
		RoleCeiling:     RoleReader,
		ExpiresAt:       now.Add(5 * time.Minute),
		AssertedAt:      now,
	}
}

// THE RULE THE WHOLE CONTRACT RESTS ON. An assertion that cannot PROVE the
// ceiling was applied is refused, rather than being read as "not a badge
// session". Each row below is a way the previous design failed open.
func TestVerifyForwardedAuthorityRefusesUnprovableAssertions(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		with func(a *ForwardedAuthority)
		want error
		why  string
	}{
		{
			name: "an empty credential class",
			with: func(a *ForwardedAuthority) { a.CredentialClass = "" },
			want: ErrForwardMissingClass,
			why: "this is the rule. An absent class was indistinguishable from 'not a badge " +
				"session', so a mid-stream badge grant whose claims never reached the producer " +
				"resolved the operator's UNCLAMPED stored role on the worker",
		},
		{
			name: "an unknown credential class",
			with: func(a *ForwardedAuthority) { a.CredentialClass = "brand_new_class" },
			want: ErrForwardUnknownClass,
			why: "a class the receiver does not know is a class whose ceiling semantics it cannot " +
				"reason about. Refusing makes adding a credential class a loud change rather than " +
				"a silent one that lands in the USER bucket",
		},
		{
			name: "a badge whose role is ABOVE its own ceiling",
			with: func(a *ForwardedAuthority) { a.Role = RoleOwner },
			want: ErrForwardCeilingNotApplied,
			why: "the proof-of-clamp: the receiver re-runs RoleAtMost, the same function the " +
				"producer claims to have run, and refuses on disagreement",
		},
		{
			name: "a badge with no ceiling at all",
			with: func(a *ForwardedAuthority) { a.RoleCeiling = "" },
			want: ErrForwardBadgeMissingCeiling,
			why:  "a badge session without a ceiling cannot be checked, so it cannot be trusted",
		},
		{
			name: "a badge with no expiry",
			with: func(a *ForwardedAuthority) { a.ExpiresAt = time.Time{} },
			want: ErrForwardBadgeMissingExpiry,
			why: "the direct path gates every envelope on the grant's exp; without it on the wire " +
				"the worker cannot enforce expiry even if it tries",
		},
		{
			name: "a badge already past its expiry",
			with: func(a *ForwardedAuthority) { a.ExpiresAt = now.Add(-time.Second) },
			want: ErrForwardAuthorityExpired,
			why: "a walked-away kiosk's expired grant is rejected on the direct stream; it must " +
				"not be honoured on every forwarded AiChat / CallTool",
		},
		{
			name: "a ceiling on a class that will not enforce one",
			with: func(a *ForwardedAuthority) { a.CredentialClass = ForwardedClassUser },
			want: ErrForwardStrayCeiling,
			why: "otherwise an assertion can LOOK clamped while its class says the ceiling is " +
				"never checked",
		},
		{
			name: "an unsupported contract version",
			with: func(a *ForwardedAuthority) { a.Version = "v0" },
			want: ErrForwardUnsupportedContract,
			why:  "a producer speaking a contract this receiver does not implement is not trusted by default",
		},
		{
			name: "no principal kind",
			with: func(a *ForwardedAuthority) { a.Kind = ForwardedPrincipalUnspecified },
			want: ErrForwardMissingPrincipalKind,
			why:  "the zero value must never be a valid principal -- that is what makes an unset field loud",
		},
		{
			name: "no subject",
			with: func(a *ForwardedAuthority) { a.Subject = "" },
			want: ErrForwardMissingSubject,
			why:  "an actor with no id is the zero-rows defect this issue exists to fix, not an authority",
		},
		{
			name: "an invalid role",
			with: func(a *ForwardedAuthority) { a.Role = "wizard"; a.RoleCeiling = "wizard" },
			want: ErrForwardInvalidRole,
			why:  "an unrecognised role silently ranks as least-privileged; refuse instead of guessing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := liveBadge(now)
			tc.with(&a)

			ac, err := VerifyForwardedAuthority(a, now)
			if err == nil {
				t.Fatalf("accepted, binding %+v. %s", ac, tc.why)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("refused with %v, want %v -- the sentinel is what names the failure in "+
					"logs, metrics and tests, so the wrong one is a real defect", err, tc.want)
			}
			if ac != nil {
				t.Error("a refusal returned an AccessContext. Nothing may bind on the failure path")
			}
		})
	}
}

// The positive half. A well-formed badge assertion binds the CLAMPED decision
// verbatim -- no DB, no re-resolution, no claims-derived fallback.
func TestVerifyForwardedAuthorityBindsTheClampedDecision(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	ac, err := VerifyForwardedAuthority(liveBadge(now), now)
	if err != nil {
		t.Fatalf("a well-formed live badge assertion was refused: %v", err)
	}
	if ac.UserId != "v1:identity:user:op-1" {
		t.Errorf("UserId = %q, want the asserted subject", ac.UserId)
	}
	if ac.Role != RoleReader {
		t.Errorf("Role = %q, want the CLAMPED reader -- binding the operator's stored owner role "+
			"here is the escalation the previous attempt shipped", ac.Role)
	}
	if ac.IsClusterOwner() {
		t.Error("isClusterOwner is true for a reader-ceilinged operator. An operator on a kiosk " +
			"with a reader ceiling must not become cluster owner for the whole turn by chatting " +
			"through a forward")
	}
}

// A SYSTEM principal is the ONLY way to forward without an end-user, and it is
// deliberately constrained: reader role, and a subject that cannot be mistaken
// for a real user.
func TestVerifyForwardedAuthorityConstrainsSystemPrincipals(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	base := func() ForwardedAuthority {
		return ForwardedAuthority{
			Version:         ForwardedAuthorityVersion,
			Kind:            ForwardedPrincipalSystem,
			Subject:         "system:cognition",
			Role:            RoleReader,
			CredentialClass: ForwardedClassSystem,
			AssertedAt:      now,
		}
	}

	if _, err := VerifyForwardedAuthority(base(), now); err != nil {
		t.Fatalf("a well-formed system principal was refused: %v", err)
	}

	elevated := base()
	elevated.Role = RoleWriter
	if _, err := VerifyForwardedAuthority(elevated, now); !errors.Is(err, ErrForwardSystemRoleTooHigh) {
		t.Errorf("a writer-role system principal was accepted (err=%v).\n\n"+
			"RoleLevel ranks writer(2) ABOVE reader(3) -- lower is more privileged -- and today "+
			"these hops send the invalid string \"system\", which IsValidRole rejects and "+
			"FallbackFromClaims clamps to reader. Reader is therefore the no-widening choice; "+
			"writer would grant these hops more than they have ever had.", err)
	}

	impostor := base()
	impostor.Subject = "v1:identity:user:victim"
	if _, err := VerifyForwardedAuthority(impostor, now); !errors.Is(err, ErrForwardSystemImpersonatesUser) {
		t.Errorf("a system principal named a canonical user id (err=%v).\n\n"+
			"Downstream owner resolution keys on looksLikeCanonicalUserId, so a system assertion "+
			"wearing a user id would be adopted as that user's owner id.", err)
	}
}

// A non-badge class carries no expiry, and must not be aged out by one.
func TestVerifyForwardedAuthorityDoesNotExpireANonExpiringClass(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	a := ForwardedAuthority{
		Version:         ForwardedAuthorityVersion,
		Kind:            ForwardedPrincipalUser,
		Subject:         "v1:identity:user:u-1",
		Role:            RoleWriter,
		CredentialClass: ForwardedClassUser,
		AssertedAt:      now,
	}
	if _, err := VerifyForwardedAuthority(a, now.Add(72*time.Hour)); err != nil {
		t.Errorf("an ordinary user assertion expired: %v. Only classes that carry an exp are aged "+
			"out; mirroring badgeGate, where a zero expiry means 'ungated'", err)
	}
}

// The producer constructors are the other half of "the defect is inexpressible":
// claims are DERIVED from the authority, so a call site cannot ship claims
// without one, and whatever it ships is self-consistent.
func TestForwardedAuthorityForUserDerivesConsistentClaims(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	a, err := ForwardedAuthorityForUser(
		&AccessContext{UserId: "v1:identity:user:op-1", PrimaryEmail: "op@example.io", Role: RoleReader},
		ForwardedClassBadge, RoleReader, now.Add(time.Minute), now)
	if err != nil {
		t.Fatalf("building a user authority failed: %v", err)
	}

	p := a.Principal()
	if got := p.Claims[forwardedClaimSubject]; got != a.Subject {
		t.Errorf("claims sub = %q, authority subject = %q -- the two carriers must not drift, "+
			"which is why one is derived from the other", got, a.Subject)
	}
	if _, err := VerifyForwardedAuthority(p.Authority, now); err != nil {
		t.Errorf("a producer-built authority did not verify on the receiver: %v. The constructors "+
			"and the verifier are one contract; if they disagree, every forward fails closed", err)
	}
}

// An unresolved session must not produce a shippable authority: the producer
// refuses locally rather than sending something the receiver will reject.
func TestForwardedAuthorityForUserRefusesAnUnresolvedSession(t *testing.T) {
	now := time.Now()
	if _, err := ForwardedAuthorityForUser(nil, ForwardedClassUser, "", time.Time{}, now); err == nil {
		t.Error("built an authority from a nil AccessContext")
	}
	if _, err := ForwardedAuthorityForUser(&AccessContext{Role: RoleReader}, ForwardedClassUser, "", time.Time{}, now); err == nil {
		t.Error("built an authority with no subject")
	}
}
