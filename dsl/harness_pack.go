package dsl

import (
	"embed"
	"io/fs"
)

// harness_pack.go makes the harness a PACK -- the proving migration of the
// module-registry epic (memql#4190; design docs/superpowers/specs/
// 2026-08-20-module-registry-design.md section 8).
//
// The .memql files stay exactly where they were, at dsl/harness/, and
// still compile into every binary; what changes is HOW they reach the
// unified tree. The core embed directive in embed.go no longer includes
// them (which automatically frees the domain name -- coreDomains() reads
// that FS), and this file's own directive embeds the same files and
// registers them through RegisterTree, the same primitive every external
// pack uses. The overlay mounts at "harness/", so every merged-tree path
// ("harness/mutations.memql") is byte-identical to what the core mount
// produced -- the conformance walkers, sense's ambient-domain resolution
// and the sdk generator (which walks the DIRECTORY, not the runtime tree)
// all see the world unchanged.
//
// What the move buys: v1:platform:packState can now disable the harness
// per instance. Disabled, it loads mounted-inert -- concepts (and the
// dsl/actions plan/step relationship edge, and any written rows) keep
// resolving, while recall/harnessTrace, the eleven mutations, the
// consolidation automation and the Go reconciler wiring are absent. The
// Go half rides the same switch via BindPluginToPack in
// integrations/harnessrecall + harnesstrace and the app-phase gate on
// setupHarnessReconciler.
//
// Registration is UNCONDITIONAL, exactly like every pack's init():
// registered is not enabled. The enablement read happens at app phase 3;
// this init() just owns the namespace.

// HarnessPackDomain is the harness pack's registered domain name.
const HarnessPackDomain = "harness"

//go:embed all:harness
var harnessPackFS embed.FS

func init() {
	sub, err := fs.Sub(harnessPackFS, "harness")
	if err != nil {
		panic("dsl: embedded harness pack tree missing: " + err.Error())
	}
	RegisterTree(HarnessPackDomain, sub)
}
