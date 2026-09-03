package memql

import (
	"strings"
	"testing"
)

// TestSiteStatusTransitionGuard pins the D10 lifecycle law (epic memql#4794).
// It is the systemOwned-delete guard's suite extended to STATUS, which is the
// gap section A of the design measured: memql#3717 guarded delete and left
// status with none, so the row the cluster is managed through could be disabled
// or archived by anybody who could reach the mutation.
//
// The guard touches no engine field, so every case runs through the nil
// receiver (DB-free), mirroring the delete guard's suite exactly.
func TestSiteStatusTransitionGuard(t *testing.T) {
	userCtx, userActor := userActorContext()
	ownerCtx, ownerActor := ownerActorContext()
	sysCtx, sysActor := systemSeedContext()
	g := validatorOnNilEngine()

	// ---- the guard does not fire where there is no transition ----

	t.Run("a write that names no status passes", func(t *testing.T) {
		payload := map[string]any{"hostname": "shop.example.com", "bundleRef": "blob://sites/a/1/"}
		if err := g.validateSiteStatusTransition(userCtx, payload, true, "live", true, userActor); err != nil {
			t.Fatalf("a publish carries no status of its own and must pass; got %v", err)
		}
	})

	t.Run("re-writing the SAME status is not a transition", func(t *testing.T) {
		// This is the case that would break every ordinary write if it were
		// wrong: update{} is a read-merge, so a rename or a publish against a
		// disabled or archived row inherits `status` and arrives here looking
		// like a transition to itself.
		for _, s := range []string{"draft", "live", "disabled", "archived"} {
			payload := map[string]any{"hostname": "shop.example.com", "status": s, "title": "renamed"}
			if err := g.validateSiteStatusTransition(userCtx, payload, true, s, false, userActor); err != nil {
				t.Fatalf("re-writing status %q must pass; got %v", s, err)
			}
		}
	})

	// ---- the ordinary lifecycle still works ----

	t.Run("the draft -> live <-> disabled edges all pass", func(t *testing.T) {
		edges := [][2]string{
			{"draft", "live"},
			{"live", "disabled"},
			{"disabled", "live"},
			{"draft", "disabled"},
			{"disabled", "archived"},
			{"archived", "disabled"},
		}
		for _, e := range edges {
			payload := map[string]any{"hostname": "shop.example.com", "status": e[1]}
			if err := g.validateSiteStatusTransition(userCtx, payload, true, e[0], false, userActor); err != nil {
				t.Fatalf("%s -> %s must pass; got %v", e[0], e[1], err)
			}
		}
	})

	// ---- archive requires disabled first ----

	t.Run("archiving a live or draft site is refused -- disable first", func(t *testing.T) {
		for _, prior := range []string{"live", "draft"} {
			payload := map[string]any{"hostname": "shop.example.com", "status": "archived"}
			err := g.validateSiteStatusTransition(userCtx, payload, true, prior, false, userActor)
			if err == nil {
				t.Fatalf("archiving from %q must be refused", prior)
			}
			if !strings.Contains(err.Error(), "disabled") {
				t.Errorf("the refusal must say what to do first; got %q", err.Error())
			}
		}
	})

	t.Run("an archived site restores to disabled, never straight to live", func(t *testing.T) {
		payload := map[string]any{"hostname": "shop.example.com", "status": "live"}
		if err := g.validateSiteStatusTransition(userCtx, payload, true, "archived", false, userActor); err == nil {
			t.Fatal("archived -> live must be refused; restoring lands on disabled")
		}
		payload["status"] = "draft"
		if err := g.validateSiteStatusTransition(userCtx, payload, true, "archived", false, userActor); err == nil {
			t.Fatal("archived -> draft must be refused too")
		}
	})

	t.Run("a site cannot be created already archived", func(t *testing.T) {
		payload := map[string]any{"hostname": "shop.example.com", "status": "archived"}
		if err := g.validateSiteStatusTransition(userCtx, payload, false, "", false, userActor); err == nil {
			t.Fatal("creating an archived site must be refused -- it would be a second entrance to the archive with no ceremony")
		}
		// The reachable positive: creating with any other status passes, so
		// the refusal above is about `archived` and not about creation.
		for _, s := range []string{"draft", "live", "disabled"} {
			payload["status"] = s
			if err := g.validateSiteStatusTransition(userCtx, payload, false, "", false, userActor); err != nil {
				t.Fatalf("creating a site with status %q must pass; got %v", s, err)
			}
		}
	})

	// ---- systemOwned is exempt from the whole axis ----

	t.Run("a status change on a systemOwned site is refused", func(t *testing.T) {
		payload := map[string]any{"hostname": "portal.example.com", "status": "disabled"}
		err := g.validateSiteStatusTransition(userCtx, payload, true, "live", true, userActor)
		if err == nil {
			t.Fatal("disabling the portal must be refused")
		}
		for _, want := range []string{"portal.example.com", "systemOwned", "v1:platform:site"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing substring %q", err.Error(), want)
			}
		}
	})

	t.Run("an owner actor is still refused -- ownership does not exempt", func(t *testing.T) {
		payload := map[string]any{"hostname": "portal.example.com", "status": "archived"}
		if err := g.validateSiteStatusTransition(ownerCtx, payload, true, "disabled", true, ownerActor); err == nil {
			t.Fatal("a cluster owner must not be able to archive the portal either")
		}
	})

	t.Run("a raw write cannot smuggle systemOwned:false in the same delta as a status change", func(t *testing.T) {
		// The bypass the delete guard has its own case for, extended here.
		// priorSystemOwned is the PERSISTED value; the delta's claim that the
		// row is not systemOwned must not be what the guard reads, or one raw
		// write flips the flag off and disables the portal in the same call.
		payload := map[string]any{
			"hostname":    "portal.example.com",
			"systemOwned": false,
			"status":      "disabled",
		}
		if err := g.validateSiteStatusTransition(userCtx, payload, true, "live", true, userActor); err == nil {
			t.Fatal("flipping systemOwned in the same write must not bypass the status guard")
		}
	})

	t.Run("system actor may re-seed a systemOwned site live", func(t *testing.T) {
		// The SeedMaterializer re-writes the portal and OS rows at every boot
		// carrying status:"live". Refusing that would leave a cluster whose
		// portal had somehow been disabled unable to heal itself.
		payload := map[string]any{"hostname": "portal.example.com", "status": "live"}
		if err := g.validateSiteStatusTransition(sysCtx, payload, true, "disabled", true, sysActor); err != nil {
			t.Fatalf("the boot re-seed must pass; got %v", err)
		}
	})
}

// TestPackageDeploymentIsAppendOnlyPastTerminal pins D7: the timeline records
// what was attempted, so the thing that produced it cannot go back and edit it.
func TestPackageDeploymentIsAppendOnlyPastTerminal(t *testing.T) {
	t.Run("a create passes", func(t *testing.T) {
		if err := validatePackageDeploymentAppendOnly(false, ""); err != nil {
			t.Fatalf("opening a deployment must pass; got %v", err)
		}
	})

	t.Run("every non-terminal stage still accepts writes", func(t *testing.T) {
		for _, s := range []string{"analyzing", "awaiting_confirm", "building", "staging", "rolling", "publishing"} {
			if err := validatePackageDeploymentAppendOnly(true, s); err != nil {
				t.Fatalf("advancing from %q must pass; got %v", s, err)
			}
		}
	})

	t.Run("every terminal status refuses further writes", func(t *testing.T) {
		for _, s := range []string{"succeeded", "refused", "failed", "abandoned"} {
			err := validatePackageDeploymentAppendOnly(true, s)
			if err == nil {
				t.Fatalf("a write onto a %q deployment must be refused", s)
			}
			if !strings.Contains(err.Error(), "append-only") {
				t.Errorf("the refusal must name the rule; got %q", err.Error())
			}
		}
	})

	t.Run("there is deliberately no system-actor escape", func(t *testing.T) {
		// The pipeline IS the system actor here, and it is exactly the writer
		// the rule binds -- so this guard takes no context at all. The
		// assertion is on the SIGNATURE: if somebody adds a ctx parameter and
		// an isSystemActor exemption, this case is what has to be deleted to
		// make it compile, which is a decision a reviewer sees.
		if err := validatePackageDeploymentAppendOnly(true, "succeeded"); err == nil {
			t.Fatal("a terminal row must refuse every writer, the pipeline included")
		}
	})
}
