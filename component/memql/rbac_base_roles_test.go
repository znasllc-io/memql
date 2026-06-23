package memql

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// TestRBACBaseRolesLoadWithRanks is the E1.2 (memql#2070) acceptance gate
// for the authored base-role catalog: the four base roles must load from the
// DSL with the locked ranks (owner > developer > admin > user, developer
// OUTRANKS admin) and each role's slug must back at least one capability
// grant from E1.1. DB-free: reads the loaded seed registry directly.
func TestRBACBaseRolesLoadWithRanks(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	seedRegistry := NewSeedRegistry()
	if _, err := LoadUnifiedSeeds(logger, seedRegistry); err != nil {
		t.Fatalf("LoadUnifiedSeeds: %v", err)
	}

	type roleRow struct {
		rank       int64
		predefined bool
	}
	roles := map[string]roleRow{}
	for _, def := range seedRegistry.All() {
		if def.UseConcept != "role" {
			continue
		}
		slug := def.Body.fields["slug"].str
		rr := roleRow{
			rank:       def.Body.fields["rank"].intV,
			predefined: def.Body.fields["predefined"].boolV,
		}
		roles[slug] = rr
	}

	// All four base roles present.
	for _, slug := range []string{"owner", "developer", "admin", "user"} {
		rr, ok := roles[slug]
		if !ok {
			t.Errorf("base role %q not authored as a seed", slug)
			continue
		}
		if !rr.predefined {
			t.Errorf("base role %q must be predefined=true (immutable at runtime)", slug)
		}
	}

	// Rank ordering: owner > developer > admin > user. The load-bearing
	// locked decision is developer OUTRANKS admin.
	owner, dev, admin, user := roles["owner"].rank, roles["developer"].rank, roles["admin"].rank, roles["user"].rank
	if !(owner > dev && dev > admin && admin > user) {
		t.Errorf("base-role rank ordering wrong: owner=%d developer=%d admin=%d user=%d; want owner > developer > admin > user (developer OUTRANKS admin)",
			owner, dev, admin, user)
	}

	// Ranks are spaced (not adjacent 1/2/3/4) so E1.4 custom roles can slot
	// between -- assert a gap of at least 2 between each adjacent pair.
	if dev-admin < 2 || admin-user < 2 || owner-dev < 2 {
		t.Errorf("base ranks must be spaced so custom roles can slot between (owner=%d developer=%d admin=%d user=%d)", owner, dev, admin, user)
	}

	// Every base-role slug must back at least one capability grant (the
	// roles and the E1.1 grants are wired by slug).
	grantedSlugs := map[string]bool{}
	for _, def := range seedRegistry.All() {
		if def.UseConcept != "capability" {
			continue
		}
		grantedSlugs[def.Body.fields["roleSlug"].str] = true
	}
	for _, slug := range []string{"owner", "developer", "admin", "user"} {
		if !grantedSlugs[slug] {
			t.Errorf("base role %q has no capability grants -- role/capability wiring broken", slug)
		}
	}
}

// TestRBACBaseRoleImmutableGuard pins the E1.2 runtime-immutability contract:
// the validateRbacBaseRoleImmutable guard rejects a NON-system-actor write to
// a predefined role (whether the predefined flag is in the proposed payload or
// only in the prior row), while allowing a system actor and allowing a
// non-predefined custom-role write by a user actor. The guard touches no
// engine field, so it runs through a nil receiver (DB-free).
func TestRBACBaseRoleImmutableGuard(t *testing.T) {
	userCtx, userActor := userActorContext()
	sysCtx, sysActor := systemSeedContext()

	t.Run("user-actor edit of predefined-in-payload is rejected", func(t *testing.T) {
		payload := map[string]any{"slug": "owner", "rank": int64(999), "predefined": true}
		err := validatorOnNilEngine().validateRbacBaseRoleImmutable(userCtx, payload, false, userActor)
		if err == nil {
			t.Fatal("expected rejection for user-actor edit of a predefined role; got nil")
		}
		for _, want := range []string{"immutable", "v1:rbac:role", "system actor"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing substring %q", err.Error(), want)
			}
		}
	})

	t.Run("user-actor edit of prior-predefined row is rejected even if delta flips the flag", func(t *testing.T) {
		// The caller tries to flip predefined to false on an existing base
		// role; the prior-row flag still gates the write.
		payload := map[string]any{"slug": "admin", "predefined": false}
		err := validatorOnNilEngine().validateRbacBaseRoleImmutable(userCtx, payload, true, userActor)
		if err == nil {
			t.Fatal("expected rejection: prior-predefined row must stay immutable even when the delta flips predefined=false")
		}
	})

	t.Run("system actor may (re)materialize a predefined role", func(t *testing.T) {
		payload := map[string]any{"slug": "owner", "rank": int64(400), "predefined": true}
		if err := validatorOnNilEngine().validateRbacBaseRoleImmutable(sysCtx, payload, true, sysActor); err != nil {
			t.Fatalf("system actor must be able to re-seed a base role; got %v", err)
		}
	})

	t.Run("user actor may write a non-predefined custom role", func(t *testing.T) {
		payload := map[string]any{"slug": "team-lead", "rank": int64(250), "predefined": false}
		if err := validatorOnNilEngine().validateRbacBaseRoleImmutable(userCtx, payload, false, userActor); err != nil {
			t.Fatalf("a non-predefined custom-role write must pass for a user actor; got %v", err)
		}
	})
}
