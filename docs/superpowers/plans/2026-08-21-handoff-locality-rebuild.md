# Locality Edit Policy + Rebuild from Checkout Implementation Plan (PR 1 of 3)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On a local cluster whose workspace is its recorded checkout, every `.memql` file is editable and an edit reaches the cluster through a Rebuild-from-checkout action; on a remote cluster seeded files stay read-only; an edited seeded construct is reported as `edited`; and a local instance says whether it runs released images or a checkout build.

**Architecture:** The LSP gains a seventh training state (`edited`). The extension's read-only verdict gains a "workspace is the cluster's recorded checkout" fact and renders `edited`. `scripts/k3d/dev.sh` (capability `k3d.dev`) gains `repo-root`, `app-name` and `image-source=checkout`; a one-step graph `scripts/install/graph/rebuild.json` drives it, with a Go embed + accessor so the graph gates and the DSL-agreement test cover it. The Deployments view runs that graph from a preflight screen, records the run in the install receipt, and renders the resulting image-source mode on the instance row and the Connection page.

**Tech Stack:** Go (`cmd/memql-lsp`, `scripts/install/graph`, `scripts/k3d` harness tests), bash (`scripts/k3d/dev.sh` under `scripts/lib/capability.sh`), MemQL DSL (`dsl/install`), TypeScript (editors/vscode on `node --test` + the xvfb host lane).

**Spec:** `docs/superpowers/specs/2026-08-21-portal-vscode-handoff-and-locality-design.md` (sections 2.3-2.7, 3, 5). Epic memql#4242; this plan closes #4243, #4245, #4244, #4246 in ONE pull request (branch `feat/handoff-locality-rebuild`).

## Global Constraints

- **No emojis** anywhere in code, docs, copy, or script output (CLAUDE.md). Use `[ ]` / `[x]` and `SUCCESS:` / `ERROR:` / `WARNING:` / `INFO:`.
- **Stage files by explicit path.** Never `git add -A` or `git add .`.
- **One PR closes all four issues.** Commit per task; the PR body carries `Closes #4243`, `Closes #4245`, `Closes #4244`, `Closes #4246`.
- **Pre-release: no backwards-compat shims.** Rename, do not alias.
- **The read-only rule is unchanged in words** -- a file is read-only exactly when editing it cannot change what the cluster runs -- and its table is: local cluster whose workspace is its recorded checkout: every origin editable; remote: `core` read-only (`sealed`), `bundle` read-only (`remote`); `promoted` / `staged` / unknown-to-the-catalog: editable everywhere. The marking is a courtesy; `PromoteAuthoredConstruct` is the control.
- **"Local" means BOTH `ClusterConfig.local === true` AND the workspace folder is the checkout the receipt recorded** (`recordedStackDir`, from the `stackCheckout` entry's `result.dest`, falling back to `params.dest`).
- **`edited` is a TRAINING state, not an origin.** Origin stays `core | bundle | promoted | staged`. `edited` = origin `core` or `bundle` AND the catalog hash is non-empty AND it differs from the document hash. An empty catalog hash is a missing answer, never a mismatch (`hashesAgree`'s doctrine). An unrecognised origin still degrades to `seeded`.
- **The gutter keeps three marks.** `edited` takes the `drifted` mark on every cluster; only the lens wording varies by locality.
- **`trainingWireNames` in `cmd/memql-lsp/training_acceptance_test.go` and `TRAINING_STATES` in `editors/vscode/src/state/training.ts` must agree** -- the acceptance test reads the TS file as text and fails in either direction.
- **Image-source lanes:** `released` is set by install / upgrade / repair (`clusterUp --image-tag`); `checkout` is set by Rebuild (`k3d.dev --image-source=checkout`). Crossing is never silent: the preflight or confirmation of a released-lane run on a checkout-mode instance says "this returns local to released <tag> images"; the Rebuild preflight on a released instance says it switches to checkout-built images.
- **Under `--image-source=checkout`, the database operand override is preserved and `ensure_db_image` is skipped** (the database is not a node, memql#4063). Import BEFORE patching, so the pods the sync rolls find their images present.
- **Capability-script contract:** `#!/usr/bin/env bash`, `set -euo pipefail`, `cap_spec_param` before use (an undeclared flag exits 2), stderr for humans, exactly one JSON envelope on stdout, exit codes 0 ok / 2 bad param / 3 refused / 4 prerequisite missing / 5 failed, no decisions on environment.
- **`scripts/vscode/package.sh` stages `scripts/install/graph/*.json` and every script named in `runner.ts`'s `CAPABILITY_SCRIPTS` automatically** -- no packaging change.
- **Extension modules under `src/state/`, `src/constructs/` (except the listed adapters), `src/deploy/`, `src/install/` stay free of `vscode` imports** (`cmd/memql-lsp/vscodeimportrule_test.go`).
- **Information policy (memql#4194):** toasts carry a short verdict; raw script output goes to the `MemQL Install` channel through the redactor.
- Test commands: `go test ./cmd/memql-lsp/...`, `go test ./scripts/install/graph/... ./scripts/lib/... ./scripts/k3d/...`, `go test ./test/dslconformance/...`, `cd editors/vscode && npm test`, `make vscode-test-host` (needs `DISPLAY`), `go test .` (repo-root guards).

---

### Task 1: The `edited` training state -- closes #4243

**Files:**
- Modify: `cmd/memql-lsp/training.go` (header comment lines 6-12; const block lines 129-157; `trainingConstruct.State` doc line ~242; `trainingStateFor` lines 425-480)
- Modify: `cmd/memql-lsp/training_test.go` (invert `TestTrainingState_SeededStaysSeededWhenTheLocalSourceDiffers`; add the empty-hash case)
- Modify: `cmd/memql-lsp/training_acceptance_test.go` (`trainingWireNames` gains `edited`; the all-states walk gains an `edited` leg and is renamed)
- Modify: `editors/vscode/src/state/training.ts` (`TRAINING_STATES`, `gutterMarkFor`, `detailFor`, `actionsFor`, `TrainingCounts` + `countStates`, `trainingListEntries`)
- Modify: `editors/vscode/src/training/closure.ts` (`classify`: `edited` stays OUT of a promote bundle; comment)
- Modify: `editors/vscode/test/training.test.ts`, `editors/vscode/test/trainingClosure.test.ts`
- Modify: `docs/public/language/training.md` (lines 115-156: "The seven states")

**Interfaces:**
- Produces: Go constant `trainingStateEdited = "edited"`; TS union member `"edited"` in `TRAINING_STATES` (position: after `"seeded"`, before `"staged"`); `TrainingCounts.edited: number`.
- Task 3 consumes `"edited"` to add the locality-aware lens; this task gives `edited` NO actions and the `drifted` gutter mark.

- [ ] **Step 1: Write the failing Go tests**

In `cmd/memql-lsp/training_test.go`, replace `TestTrainingState_SeededStaysSeededWhenTheLocalSourceDiffers` (lines 561-592) with:

```go
// ORIGIN DECIDES THE TIER, THEN THE HASH DECIDES WHETHER THE TIER IS CURRENT. A
// seeded construct whose local source differs from what the cluster loaded is
// `edited`: the gutter's one question -- does what I am looking at match what
// runs? -- is answered "no", and on a local cluster there is now an action for
// it (Rebuild from checkout). On a remote cluster the lens says a rollout is
// the way; the state is the same because the fact is the same.
func TestTrainingState_SeededBecomesEditedWhenTheLocalSourceDiffers(t *testing.T) {
	h, s := newInitializedHandler(t)
	const uri = "file:///w/dsl/cognition/queries.memql"
	const doc = "query participant coreQuery {\n  filter  isActiveRecord && status==\"moved\"\n}\n"
	openDoc(t, s, uri, doc)

	for _, origin := range []string{"core", "bundle"} {
		pushCatalog(t, h, fmt.Sprintf(
			`[{"name":"coreQuery","kind":"query","origin":%q,"sourceHash":"1111111111111111111111111111111111111111111111111111111111111111"}]`,
			origin))
		if got := statesByName(decodeTraining(t, h, uri))["coreQuery"]; got != trainingStateEdited {
			t.Errorf("an edited %s construct = %q; want %q", origin, got, trainingStateEdited)
		}
		// The same construct with a MATCHING hash is plainly seeded.
		pushCatalog(t, h, fmt.Sprintf(
			`[{"name":"coreQuery","kind":"query","origin":%q,"sourceHash":%q}]`,
			origin, localHashOf(t, doc, "coreQuery")))
		if got := statesByName(decodeTraining(t, h, uri))["coreQuery"]; got != trainingStateSeeded {
			t.Errorf("a matching %s construct = %q; want %q", origin, got, trainingStateSeeded)
		}
	}

	// AN EMPTY CATALOG HASH IS A MISSING ANSWER, NOT A MISMATCH (hashesAgree's
	// doctrine): a seeded construct the cluster could not hash stays seeded.
	pushCatalog(t, h, `[{"name":"coreQuery","kind":"query","origin":"core","sourceHash":""}]`)
	if got := statesByName(decodeTraining(t, h, uri))["coreQuery"]; got != trainingStateSeeded {
		t.Errorf("a seeded construct with no catalog hash = %q; want %q", got, trainingStateSeeded)
	}

	// An unrecognised origin still degrades to seeded: a client bug costs a
	// missing affordance rather than a refused one.
	pushCatalog(t, h, `[{"name":"coreQuery","kind":"query","origin":"","sourceHash":"1111111111111111111111111111111111111111111111111111111111111111"}]`)
	if got := statesByName(decodeTraining(t, h, uri))["coreQuery"]; got != trainingStateSeeded {
		t.Errorf("a catalog entry with an unrecognised origin = %q; want %q", got, trainingStateSeeded)
	}
}
```

In `cmd/memql-lsp/training_acceptance_test.go`: add `trainingStateEdited: "TRAINING_STATES",` to `trainingWireNames`; rename `TestTrainingState_AllFiveStatesAgainstTheRealEngineCatalog` to `TestTrainingState_AllStatesAgainstTheRealEngineCatalog` and insert, right after its `seeded` leg and before the `trained` leg, an `edited` leg: take the UNTOUCHED real catalog (every construct is `core`), `insertDocLine` a `///` comment into the first query exactly as the `drifted` leg does, push the untouched catalog, assert the state is `trainingStateEdited`; then restore the original document text before the `trained` leg (re-open it with `openDoc`).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./cmd/memql-lsp/ -run 'TestTrainingState_SeededBecomesEdited|TestTrainingState_AllStates|TestTrainingWireNames' 2>&1 | tail -5`
Expected: compile error `undefined: trainingStateEdited`.

- [ ] **Step 3: Implement the Go side**

In `cmd/memql-lsp/training.go`:

1. Header comment (lines 6-12) lists all seven:

```go
//	untrained  the cluster has no record of it
//	drifted    the cluster has it, promoted, and its source no longer matches
//	trained    the cluster has it, promoted, and its source matches
//	staged     durable on the cluster, callable by its author alone
//	seeded     the cluster has it from disk, and the source matches what it loaded
//	edited     the cluster has it from disk, and the source no longer matches --
//	           a rollout (locally: Rebuild from checkout) is what applies it
//	unknown    nothing can be said
```

2. Const block: replace "The six states" with "The seven states"; after `trainingStateSeeded` add:

```go
	// trainingStateEdited: loaded from disk at boot, and the local source no
	// longer matches what the cluster loaded. Not `drifted`: drift is defined
	// against a promotion and this construct has none. Its way onto the cluster
	// is a rollout -- on a local cluster, Rebuild from checkout -- and the state
	// exists so the gutter can answer "no" to its one question instead of
	// reporting a seeded construct as current.
	trainingStateEdited = "edited"
```

3. `trainingConstruct.State` doc: "one of the seven constants above".

4. Replace the doc comment above `trainingStateFor` (lines 427-443) with:

```go
// trainingStateFor is the state machine, in the order the rules resolve.
//
// ORIGIN DECIDES THE TIER, THEN THE HASH DECIDES WHETHER THAT TIER IS CURRENT.
// Drift is defined against a promotion, so a promoted construct whose source
// moved is `drifted`; a seeded construct -- core or bundle -- whose source moved
// is `edited`, a different state because its way onto the cluster is a rollout
// rather than a promote (locally, Rebuild from checkout; remotely, a new
// image). A seeded construct the cluster could not hash stays `seeded`: an
// empty catalog hash is a missing answer, not a mismatch (see hashesAgree).
```

and change the seeded branch to:

```go
	if entry.Origin != memql.ConstructOriginPromoted {
		// core, bundle -- and anything a client sends that is neither, which
		// degrades to `seeded` on purpose: an unrecognised origin is a client
		// bug, and `seeded` offers no action, so a wrong guess costs a missing
		// affordance rather than a refused one.
		seededOrigin := entry.Origin == memql.ConstructOriginCore || entry.Origin == memql.ConstructOriginBundle
		if seededOrigin && entry.SourceHash != "" && !hashesAgree(entry.SourceHash, declared.SourceHash) {
			return trainingStateEdited
		}
		return trainingStateSeeded
	}
```

- [ ] **Step 4: Run the Go tests**

Run: `go test ./cmd/memql-lsp/... 2>&1 | tail -4`
Expected: `TestTrainingWireNamesMatchTheExtension` FAILS ("the server emits state "edited", which TRAINING_STATES does not list") -- the only failure. That failure is the gate working; Step 5 clears it.

- [ ] **Step 5: Write the failing TS tests**

In `editors/vscode/test/training.test.ts`:
- In the test "the gutter has three marks for five states, and trained shares with seeded" (lines 53-61): rename to "...for seven states..." and add `assert.equal(gutterMarkFor("edited"), "drifted");`.
- In "with actions on, each state offers exactly what the design's table says": add `assert.deepEqual(of("edited"), []);` with the comment `// No action here: locality decides the action (Task 3 of the plan), and the lens is where it is said.`
- In "the actions are OFF by default..." (lines 182-188) add `"edited"` to the literal array.
- In "the status bar reports only what needs attention" (lines 254-267) and "unknown counts toward nothing" (288-292) add `edited` to the expected `counts` objects.
- Add:

```ts
test("edited joins the attention list beside untrained and drifted", () => {
  const entries = trainingListEntries([construct("edited", "coreQuery"), construct("seeded", "plain")]);
  assert.deepEqual(entries.map((e) => e.construct.name), ["coreQuery"]);
  assert.match(entries[0]!.detail, /rollout|Rebuild/);
});
```

In `editors/vscode/test/trainingClosure.test.ts`, after the seeded-dependency test (lines 111-127):

```ts
test("an edited dependency is left out -- the engine refuses a core shadow regardless of the edit", async () => {
  const ws = workspace({
    "/w/queries.memql": {
      text: "query q { }\n",
      imports: ["/core/traits.memql"],
      constructs: [construct("q", "untrained")],
    },
    "/core/traits.memql": { text: "trait core { }\n", constructs: [construct("core", "edited")] },
  });
  const bundle = await assembleClosure("/w/queries.memql", "query q { }\n", ws);
  assert.deepEqual(included(bundle), ["/w/queries.memql"]);
});
```

- [ ] **Step 6: Run to verify they fail**

Run: `cd editors/vscode && npm test 2>&1 | grep -E "error TS|not ok" | head`
Expected: `tsc` errors -- `"edited"` is not a `TrainingState`.

- [ ] **Step 7: Implement the TS side**

In `editors/vscode/src/state/training.ts`:
- Doc comment: "The seven states"; add to the list after `"seeded"`: `"edited",` with the comment `// Seeded, and the buffer no longer matches what the cluster loaded. Not drifted: drift is defined against a promotion. A rollout applies it -- locally, Rebuild from checkout.`
- `gutterMarkFor`: `case "edited": return "drifted";` with the comment `// The gutter's one question is answered "no", and that is the drifted mark. HOW it gets applied -- rollout, not promote -- is the lens's business.`
- `detailFor`: `case "edited": return "Loaded from disk when the cluster booted, and your source no longer matches what it loaded. Nothing here can be promoted -- a seeded construct changes by rollout.";`
- `actionsFor`: `case "edited": return [];` (its own case label; the locality-aware action arrives with the lens options in Task 3).
- `TrainingCounts` gains `edited: number`; `countStates` initialises `edited: 0`.
- `trainingListEntries`: the filter becomes `if (construct.state !== "untrained" && construct.state !== "drifted" && construct.state !== "edited") continue;`.

In `editors/vscode/src/training/closure.ts`: the header rule gains `//   edited               -> on the cluster from disk; the engine refuses a core shadow. OUT.` and `classify()`'s `needed` expression stays as is (edited is not in it) -- add a one-line comment above it saying `edited` is deliberately absent.

- [ ] **Step 8: Docs**

In `docs/public/language/training.md` lines 115-156: heading "## The seven states"; the opening sentence says seven; add the table row `| **edited** | loaded from disk at boot, and your source no longer matches what the cluster loaded. Not drifted -- nothing was promoted. A rollout applies it: on a local cluster, **Rebuild from checkout** in the Deployments view |` after the `seeded` row; replace the paragraph "A staged construct you have edited stays `staged` ... the same argument that keeps an edited seeded construct `seeded`. Re-staging is how you update it." with: "A staged construct you have edited stays `staged` rather than becoming `drifted`, and that is deliberate: drift is defined against a **promotion**, and a staged construct has no promoted version to have drifted from. Re-staging is how you update it. An edited **seeded** construct is `edited` for the mirror reason -- there is nothing to promote over, and the editor says so rather than reporting it current."

- [ ] **Step 9: Run everything for this task**

Run: `go test ./cmd/memql-lsp/... 2>&1 | tail -3 && cd editors/vscode && npm test 2>&1 | tail -6`
Expected: Go `ok` (the wire-names gate now passes in both directions); TS all pass.

- [ ] **Step 10: Commit**

```bash
git add cmd/memql-lsp/training.go cmd/memql-lsp/training_test.go cmd/memql-lsp/training_acceptance_test.go editors/vscode/src/state/training.ts editors/vscode/src/training/closure.ts editors/vscode/test/training.test.ts editors/vscode/test/trainingClosure.test.ts docs/public/language/training.md
git commit -m "lsp: the edited training state -- seeded, but the source no longer matches" -m "Refs memql#4243"
```

---

### Task 2: `k3d.dev` gains `repo-root`, `app-name` and `image-source=checkout`; the `rebuild` graph -- closes #4245

**Files:**
- Modify: `scripts/k3d/dev.sh`
- Create: `scripts/k3d/dev_image_overrides_test.go` (harness test sourcing `dev.sh`)
- Create: `scripts/install/graph/rebuild.json`
- Modify: `scripts/install/graph/graph.go` (`//go:embed rebuild.json`, `Rebuild()`), `scripts/install/graph/loader_test.go` (the shipped-graph scan lists), `scripts/install/graph/dsl_agreement_test.go` (`graphScripts` scan list)
- Modify: `dsl/install/actions.memql` (action `rebuildLocalClusterFromCheckout`), `dsl/install/concepts.memql` (`installRun.graph` enum gains `"rebuild"`)
- Modify: `docs/public/operate/reproduce-the-cloud-locally.md`

**Interfaces:**
- Produces (Tasks 4-5 consume): flags `--repo-root=<dir>`, `--app-name=<name>`, `--image-source=checkout`; result fields `imageSource` (`"checkout"` | `"unchanged"`), `repoRoot`, `commit`, `ref` (`tag:<name>` | `branch:<name>` | `detached`), `dirtyCount` (int), `overridesPatched` (bool), `appName`, plus the existing `cluster`, `namespace`, `nodes`, `rebuilt`, `restarted`; graph `rebuild.json` with one step `rebuildFromCheckout` (script `k3d.dev`, graph-pinned `params: {"image-source": "checkout"}`, `receipt: "rebuild"`, `preExistingPath: "none"`, `verify: resultEquals result.imageSource = checkout`, `elevation: "none"`, `timeoutSeconds: 2700`, `kind: "install"`, `name: "rebuild"`); Go `graph.Rebuild() (*Graph, error)`.

- [ ] **Step 1: Write the failing Go tests**

`scripts/k3d/dev_image_overrides_test.go` (model it on `up_voice_gate_budget_test.go`'s harness: write a bash file that sources `dev.sh` and calls the function, run it with `exec.Command("bash", harness)`):

```go
package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// filter_node_image_overrides keeps the database operand's override and drops
// every node override, because under --image-source=checkout the nodes must
// resolve to the overlay's own :local images while the operand must not roll.
func runFilter(t *testing.T, input string) string {
	t.Helper()
	root := repoRoot(t)
	harness := filepath.Join(t.TempDir(), "harness.sh")
	body := "set -euo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "dev.sh") + "\"\n" +
		"filter_node_image_overrides '" + input + "'\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestFilterNodeImageOverridesKeepsTheOperandOnly(t *testing.T) {
	in := `["memql-bff=ghcr.io/znasllc-io/memql-bff:v0.17.0","memql-db=ghcr.io/znasllc-io/memql-db:16.15-timescaledb-2.29.1","memql-agent=ghcr.io/znasllc-io/memql-agent:v0.17.0"]`
	if got, want := runFilter(t, in), `["memql-db=ghcr.io/znasllc-io/memql-db:16.15-timescaledb-2.29.1"]`; got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

func TestFilterNodeImageOverridesIsIdempotent(t *testing.T) {
	in := `["memql-db=ghcr.io/znasllc-io/memql-db:16.15-timescaledb-2.29.1"]`
	if got := runFilter(t, in); got != in {
		t.Errorf("filter changed an already-filtered list: %s", got)
	}
	if got := runFilter(t, "[]"); got != "[]" {
		t.Errorf("filter of an empty list = %s", got)
	}
	if got := runFilter(t, ""); got != "[]" {
		t.Errorf("filter of no list = %s", got)
	}
}

func TestDevPrintSpecDeclaresTheRebuildFlags(t *testing.T) {
	out, err := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "k3d", "dev.sh"), "--print-spec").CombinedOutput()
	if err != nil {
		t.Fatalf("--print-spec: %v\n%s", err, out)
	}
	for _, flag := range []string{`"name":"repo-root"`, `"name":"app-name"`, `"name":"image-source"`} {
		if !strings.Contains(string(out), flag) {
			t.Errorf("--print-spec lacks %s:\n%s", flag, out)
		}
	}
}
```

(`repoRoot(t)` already exists in this package's tests -- check `up_voice_gate_budget_test.go` for its name and reuse it.)

In `scripts/install/graph/loader_test.go`: every `[]*Graph{mustLoadEmbedded(t, Install), mustLoadEmbedded(t, Uninstall)}` literal (in `TestShippedGraphsLoad`, `TestTopoOrderCoversEveryStepExactlyOnce`, `TestShippedGraphParamsAreDeclaredFlags`) gains `mustLoadEmbedded(t, Rebuild)`. In `dsl_agreement_test.go`, `graphScripts`'s literal gains it too. Do NOT touch `round_trip_coverage_test.go` (it is about the install/uninstall round trip).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./scripts/k3d/ -run 'FilterNodeImage|DevPrintSpec' 2>&1 | tail -4 && go test ./scripts/install/graph/... 2>&1 | tail -4`
Expected: the harness fails (`filter_node_image_overrides: command not found`), `--print-spec` lacks the flags; the graph package fails to compile (`undefined: Rebuild`).

- [ ] **Step 3: Change `dev.sh`**

1. Header comment: add a paragraph "Rebuilding a wizard-installed cluster" explaining `repo-root` (the packaged extension runs a staged `scripts/` with no Go source, so the build takes the checkout the install cloned) and `image-source=checkout` (a wizard install pins the Application's node images to a registry tag; this drops those overrides so the overlay's own `:local` images apply, after importing them, and leaves the database operand override alone).
2. Declarations, after `cap_spec_param "node" ...`:

```bash
cap_spec_param "repo-root"    "the MemQL checkout to build from (default: this script's own repository)"
cap_spec_param "app-name"     "ArgoCD Application name (default: \$MEMQL_K3D_APP_NAME or memql-local)"
cap_spec_param "image-source" "checkout: point the Application's node images at the locally built :local images, keeping the database operand override (default: leave the overrides as they are)" ""
```

3. New functions, placed before `main`:

```bash
# require_build_checkout -- the directory the images are built FROM.
function require_build_checkout() {
    local root="$1"
    if [[ ! -d "$root" ]]; then
        cap_fail 4 "repo-root ${root} does not exist"
    fi
    if [[ ! -f "${root}/Dockerfile" ]]; then
        cap_fail 4 "repo-root ${root} has no Dockerfile -- it is not a MemQL checkout"
    fi
    if [[ ! -f "${root}/deploy/k8s/overlays/local/kustomization.yaml" ]]; then
        cap_fail 4 "repo-root ${root} has no deploy/k8s/overlays/local/kustomization.yaml -- it is not a MemQL checkout"
    fi
    if ! git -C "$root" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        cap_fail 4 "repo-root ${root} is not a git checkout, so what was built cannot be recorded"
    fi
}

# checkout_facts -- commit, ref and dirtiness of the checkout, for the envelope.
function checkout_facts() {
    local root="$1"
    CHECKOUT_COMMIT="$(git -C "$root" rev-parse HEAD 2>/dev/null || true)"
    local tag branch
    tag="$(git -C "$root" describe --exact-match --tags HEAD 2>/dev/null || true)"
    branch="$(git -C "$root" symbolic-ref --short -q HEAD 2>/dev/null || true)"
    if [[ -n "$tag" ]]; then CHECKOUT_REF="tag:${tag}"
    elif [[ -n "$branch" ]]; then CHECKOUT_REF="branch:${branch}"
    else CHECKOUT_REF="detached"; fi
    CHECKOUT_DIRTY="$(git -C "$root" status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
}

# filter_node_image_overrides <json-string-array> -- the same list with every
# node override removed and the database operand's kept. The argument is the
# jsonpath rendering of spec.source.kustomize.images (a JSON array of
# "name=image:tag" strings); the output is a JSON array, "[]" for nothing.
function filter_node_image_overrides() {
    local raw="${1:-}"
    raw="${raw#[}"; raw="${raw%]}"
    local out="" entry name base
    local IFS=','
    for entry in $raw; do
        entry="${entry#\"}"; entry="${entry%\"}"
        [[ -z "$entry" ]] && continue
        name="${entry%%=*}"
        base="$(basename "$name")"
        if [[ "$base" == "memql-db" ]]; then
            out+="${out:+,}\"${entry}\""
        fi
    done
    printf '[%s]\n' "$out"
}

# point_application_at_local_images -- drop the node overrides a wizard install
# wrote onto the Application, so the overlay's own :local references apply.
# Call AFTER the images are imported: the sync this triggers rolls the pods.
function point_application_at_local_images() {
    section "Pointing Application '${APP_NAME}' at the locally built images"
    local current filtered
    current="$(kubectl -n "${ARGOCD_NAMESPACE}" get application "${APP_NAME}" -o 'jsonpath={.spec.source.kustomize.images}' 2>/dev/null || true)"
    if [[ -z "$current" || "$current" == "[]" ]]; then
        info "No image overrides on ${APP_NAME} -- the overlay's :local images already apply."
        return 0
    fi
    filtered="$(filter_node_image_overrides "$current")"
    if [[ "$filtered" == "$current" ]]; then
        info "Only the database operand is overridden on ${APP_NAME} -- nothing to patch."
        return 0
    fi
    info "Removing the node image overrides (keeping the database operand's)..."
    kubectl -n "${ARGOCD_NAMESPACE}" patch application "${APP_NAME}" --type=merge \
        -p "{\"spec\":{\"source\":{\"kustomize\":{\"images\":${filtered}}}}}" >&2 \
        || cap_fail 5 "patching ${APP_NAME} image overrides failed"
    kubectl -n "${ARGOCD_NAMESPACE}" annotate application "${APP_NAME}" argocd.argoproj.io/refresh=normal --overwrite >&2 || true
    OVERRIDES_PATCHED=true
    cap_changed
    wait_for_application_synced
}

# wait_for_application_synced -- ArgoCD has reconciled the patched Application.
function wait_for_application_synced() {
    local timeout="${MEMQL_K3D_SYNC_TIMEOUT:-300}"
    local deadline=$((SECONDS + timeout)) sync=""
    info "Waiting for ${APP_NAME} to sync (up to ${timeout}s)..."
    while ((SECONDS < deadline)); do
        sync="$(kubectl -n "${ARGOCD_NAMESPACE}" get application "${APP_NAME}" -o 'jsonpath={.status.sync.status}' 2>/dev/null || true)"
        if [[ "$sync" == "Synced" ]]; then
            info "${APP_NAME} is Synced."
            return 0
        fi
        sleep 5
        (( (deadline - SECONDS) % 15 == 0 )) && info "  still ${sync:-unknown} ..."
    done
    cap_fail 5 "${APP_NAME} did not reach Synced within ${timeout}s (last: ${sync:-unknown}); inspect: kubectl -n ${ARGOCD_NAMESPACE} get application ${APP_NAME}"
}
```

with `ARGOCD_NAMESPACE="argocd"` and `OVERRIDES_PATCHED=false`, `CHECKOUT_COMMIT=""`, `CHECKOUT_REF=""`, `CHECKOUT_DIRTY=0`, `IMAGE_SOURCE=""`, `APP_NAME=""` declared beside the other globals.

4. In `main()`, after the existing `cap_param` reads: `REPO_ROOT="$(cap_param repo-root "${REPO_ROOT}")"`, `APP_NAME="$(cap_param app-name "${MEMQL_K3D_APP_NAME:-memql-local}")"`, `IMAGE_SOURCE="$(cap_param image-source "")"`; then `case "$IMAGE_SOURCE" in ""|checkout) ;; *) cap_fail 2 "image-source must be empty or 'checkout' (got '${IMAGE_SOURCE}')";; esac`; then `require_build_checkout "$REPO_ROOT"` and `checkout_facts "$REPO_ROOT"`.
5. `process_node`: build + import as today; call `restart_deployment` only when `IMAGE_SOURCE != checkout` (under checkout mode the restart happens after the patch).
6. `ensure_db_image`: skipped under checkout mode with `info "Skipping the database operand image: --image-source=checkout leaves the operand override in place (the database is not a node)."`.
7. After the node loop (still inside `if [ ${#nodes_to_build[@]} -gt 0 ]`): when `IMAGE_SOURCE == checkout`: `point_application_at_local_images`; then `if [[ "$OVERRIDES_PATCHED" != true ]]; then for node in "${nodes_to_build[@]}"; do restart_deployment "$node"; done; fi` (refs unchanged, content changed -> a restart is what rolls them); then the existing `wait_for_rollouts`.
8. Result fields, before `cap_ok`: `cap_result_set imageSource "${IMAGE_SOURCE:-unchanged}"`, `cap_result_set repoRoot "$REPO_ROOT"`, `cap_result_set commit "$CHECKOUT_COMMIT"`, `cap_result_set ref "$CHECKOUT_REF"`, `cap_result_set_raw dirtyCount "${CHECKOUT_DIRTY:-0}"`, `cap_result_set_raw overridesPatched "$OVERRIDES_PATCHED"`, `cap_result_set appName "$APP_NAME"`.
9. `build_engine_node` already builds with `--file "${REPO_ROOT}/Dockerfile"` and context `"${REPO_ROOT}"` -- now the parameterised root.

- [ ] **Step 4: The graph, its embed, the DSL action**

Create `scripts/install/graph/rebuild.json`:

```json
{
  "name": "rebuild",
  "kind": "install",
  "description": "Rebuild a local cluster's node images from the recorded checkout, import them, and roll the cluster onto them.",
  "steps": [
    {
      "id": "rebuildFromCheckout",
      "script": "k3d.dev",
      "description": "Build the node images from the checkout, import them into k3d, point the Application at them, and restart the Deployments.",
      "params": { "image-source": "checkout" },
      "dependsOn": [],
      "elevation": "none",
      "receipt": "rebuild",
      "preExistingPath": "none",
      "timeoutSeconds": 2700,
      "verify": { "kind": "resultEquals", "field": "result.imageSource", "value": "checkout" }
    }
  ]
}
```

In `graph.go`, beside the two embeds: `//go:embed rebuild.json` + `var rebuildJSON []byte`, and

```go
// Rebuild is the one-step graph the extension's "Rebuild from checkout" runs:
// k3d.dev over the recorded checkout with --image-source=checkout. Install-
// shaped (forward steps, a receipt) rather than a third kind, because a
// rebuild IS an install of images the operator built.
func Rebuild() (*Graph, error) {
	rebuildOnce.Do(func() { rebuildG, rebuildErr = Load(rebuildJSON, "rebuild.json") })
	return rebuildG, rebuildErr
}
```

with the matching `sync.Once` / vars.

In `dsl/install/actions.memql`, after `bringUpLocalCluster`:

```memql
// Backend: scripts/k3d/dev.sh -- the SAME script `make dev` runs. The
// extension's "Rebuild from checkout" reaches it through the rebuild graph
// (scripts/install/graph/rebuild.json) with --image-source=checkout, which
// points a wizard-installed Application at the images this builds.
/// Build the node images from a checkout, import them into k3d, and roll the local cluster onto them.
action rebuildLocalClusterFromCheckout {
  args {
    cluster     string
    namespace   string
    appName     string
    repoRoot    string
    node        string
    imageSource string
  }
  capability script(
    script: "k3d.dev",
    cluster: args.cluster,
    namespace: args.namespace,
    app-name: args.appName,
    repo-root: args.repoRoot,
    node: args.node,
    image-source: args.imageSource
  )
}
```

In `dsl/install/concepts.memql`: `graph enum("install", "uninstall", "rebuild")` and the description names `rebuild.json` too.

- [ ] **Step 5: Run the Go, DSL and contract tests**

Run: `go test ./scripts/k3d/... ./scripts/install/graph/... ./scripts/lib/... 2>&1 | tail -5 && go test ./test/dslconformance/... 2>&1 | tail -3 && go run ./cmd/memqllint 2>&1 | tail -3`
Expected: all `ok` (including `TestShippedGraphParamsAreDeclaredFlags` for the pinned `image-source` flag and `TestActionArgsAreDeclaredScriptFlags` for the action); memqllint clean (read its `-h` if it needs a path argument and pass `./dsl`).

- [ ] **Step 6: Docs**

In `docs/public/operate/reproduce-the-cloud-locally.md`, in the section that describes `make dev`, add a short subsection "Rebuilding a wizard-installed cluster" stating the two flags, that the extension's **Rebuild from checkout** is `k3d.dev --repo-root=<checkout> --image-source=checkout`, that it drops the Application's node-image overrides (keeping the database operand's) so the overlay's `:local` images apply, and that an install, upgrade or repair returns the cluster to released images.

- [ ] **Step 7: Commit**

```bash
git add scripts/k3d/dev.sh scripts/k3d/dev_image_overrides_test.go scripts/install/graph/rebuild.json scripts/install/graph/graph.go scripts/install/graph/loader_test.go scripts/install/graph/dsl_agreement_test.go dsl/install/actions.memql dsl/install/concepts.memql docs/public/operate/reproduce-the-cloud-locally.md
git commit -m "k3d.dev: build from a repo-root and point the Application at checkout images; the rebuild graph" -m "Refs memql#4245"
```

---

### Task 3: Read-only verdict by locality and checkout match; render `edited` -- closes #4244

**Files:**
- Modify: `editors/vscode/src/constructs/readonly.ts` (header; `ReadonlyInput.workspaceIsClusterCheckout`; `readonlyVerdict`; `catalogKeyFor`; `constructsByPath`; `readonlyPatterns`; `checkoutHint`)
- Modify: `editors/vscode/src/constructs/readonlyDecorations.ts` (`update(constructs, cluster)` gains `cluster.checkout`; folder match; hint decoration)
- Modify: `editors/vscode/src/install/receipt.ts` (`recordedStackDir`)
- Modify: `editors/vscode/src/state/training.ts` (`TrainingLensOptions.cluster`, `COMMAND_REBUILD`, the `edited` lens plan)
- Modify: `editors/vscode/src/constructs/trainingLens.ts` (`setCluster`)
- Modify: `editors/vscode/src/extension.ts` (`currentRunCluster` carries `checkout`; pass the cluster to the lens provider; the readonly marker gets the checkout)
- Modify: `editors/vscode/test/readonly.test.ts`, `editors/vscode/test/training.test.ts`, `editors/vscode/test/installReceipt.test.ts`

**Interfaces:**
- Produces (Tasks 4-5 consume): `recordedStackDir(receipt: Receipt | null): string`; `COMMAND_REBUILD = "memql.deployments.rebuildFromCheckout"`; `TrainingLensOptions.cluster?: { name: string; local: boolean }`; `ReadonlyInput.workspaceIsClusterCheckout?: boolean`; `checkoutHint(clusterName: string, checkout: string): string`.

- [ ] **Step 1: Write the failing tests**

In `editors/vscode/test/readonly.test.ts` replace "core engine DSL is read-only against any cluster" with:

```ts
test("core engine DSL is read-only against every cluster except a local one whose checkout this is", () => {
  for (const clusterLocal of [true, false, undefined]) {
    const v = readonlyVerdict({ path: CORE.originPath, catalog, clusterLocal });
    assert.equal(v.readonly, true, `local=${String(clusterLocal)} without the checkout`);
    assert.equal(v.reason, "coreSealed");
  }
  const local = readonlyVerdict({ path: CORE.originPath, catalog, clusterLocal: true, workspaceIsClusterCheckout: true });
  assert.deepEqual(local, { readonly: false });
});

test("a bundle file unlocks only on a local cluster whose checkout this is", () => {
  assert.equal(readonlyVerdict({ path: BUNDLE.originPath, catalog, clusterLocal: true, workspaceIsClusterCheckout: true }).readonly, false);
  assert.equal(readonlyVerdict({ path: BUNDLE.originPath, catalog, clusterLocal: true }).reason, "remoteCluster");
  assert.equal(readonlyVerdict({ path: BUNDLE.originPath, catalog, clusterLocal: false, workspaceIsClusterCheckout: true }).reason, "remoteCluster");
});

test("the catalog path and the workspace path agree with or without the dsl/ prefix", () => {
  const bare = constructsByPath([construct({ originPath: "cognition/queries.memql" })]);
  assert.equal(readonlyVerdict({ path: "dsl/cognition/queries.memql", catalog: bare }).readonly, true);
  assert.equal(readonlyVerdict({ path: "cognition/queries.memql", catalog: bare }).readonly, true);
  const patterns = readonlyPatterns({ catalog: bare, clusterLocal: false });
  assert.deepEqual(patterns, ["cognition/queries.memql", "dsl/cognition/queries.memql"]);
});

test("a local cluster whose checkout is elsewhere gets a hint, not a lock", () => {
  const hint = checkoutHint("local", "/home/me/.memql/src");
  assert.match(hint, /not the checkout/);
  assert.match(hint, /\/home\/me\/\.memql\/src/);
  assert.match(hint, /local/);
});
```

(Adjust the existing tests that asserted the bundle unlocks on `clusterLocal: true` alone: they now pass `workspaceIsClusterCheckout: true` as well.)

In `editors/vscode/test/training.test.ts`:

```ts
test("edited offers Rebuild on a local cluster and only words on a remote one", () => {
  const local = trainingLensPlans([construct("edited")], { offerActions: true, cluster: { name: "local", local: true } })[0]!;
  assert.deepEqual(local.actions.map((a) => [a.title, a.command]), [["Rebuild from checkout", COMMAND_REBUILD]]);
  assert.match(local.detail, /Rebuild from checkout/);
  const remote = trainingLensPlans([construct("edited")], { offerActions: true, cluster: { name: "staging", local: false } })[0]!;
  assert.deepEqual(remote.actions, []);
  assert.match(remote.detail, /differs from what staging runs/);
  assert.match(remote.detail, /rollout/);
});
```

In `editors/vscode/test/installReceipt.test.ts`:

```ts
test("recordedStackDir reads the checkout directory the clone step reported", () => {
  const r = emptyReceipt("install");
  r.entries.push(entry({ stepId: "stackCheckout", script: "install.cloneStack", receipt: "checkout", params: { tag: "v0.17.0", dest: "/home/me/.memql/src" }, result: { dest: "/home/me/.memql/src", commit: "abc" } }));
  assert.equal(recordedStackDir(r), "/home/me/.memql/src");
  assert.equal(recordedStackDir(null), "");
  assert.equal(recordedStackDir(emptyReceipt("install")), "");
});
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd editors/vscode && npm test 2>&1 | grep -E "error TS|not ok" | head`
Expected: `tsc` errors (`workspaceIsClusterCheckout`, `checkoutHint`, `COMMAND_REBUILD`, `recordedStackDir`, `cluster` option do not exist).

- [ ] **Step 3: `readonly.ts`**

Rewrite the header's premise paragraph: the rule stays ("a file is read-only exactly when editing it CANNOT CHANGE WHAT THE CLUSTER RUNS"); remove the claim that a core edit changes nothing on any cluster and replace it with: "On a LOCAL cluster whose workspace is the checkout the install recorded, an edit to ANY file -- core included -- changes what the cluster runs the next time it is rebuilt from that checkout (Deployments: Rebuild from checkout). So locality is two facts, not one: the cluster is local, AND this workspace is its checkout. A different clone stays editable (it is the developer's file) but is told it is not the one the cluster builds from."

```ts
export interface ReadonlyInput {
  path: string;
  catalog?: ReadonlyMap<string, OriginatedConstruct[]>;
  clusterLocal?: boolean;
  /** Whether some workspace folder IS the selected cluster's recorded checkout. Absent means no. */
  workspaceIsClusterCheckout?: boolean;
}

/** The catalog key for a path, with or without the checkout's dsl/ prefix. */
export function catalogKeyFor(path: string): string {
  return normalizePath(path).replace(/^dsl\//, "");
}

export function readonlyVerdict(input: ReadonlyInput): ReadonlyVerdict {
  const { catalog } = input;
  if (catalog === undefined) return EDITABLE;
  const constructs = catalog.get(catalogKeyFor(input.path));
  if (constructs === undefined || constructs.length === 0) return EDITABLE;
  if (input.clusterLocal === true && input.workspaceIsClusterCheckout === true) return EDITABLE;
  if (constructs.some((c) => c.origin === "core")) return { readonly: true, reason: "coreSealed" };
  if (constructs.some((c) => c.origin === "bundle")) return { readonly: true, reason: "remoteCluster" };
  return EDITABLE;
}

export function checkoutHint(clusterName: string, checkout: string): string {
  return `This folder is not the checkout ${clusterName} rebuilds from (${checkout}). Edits here do not reach ${clusterName}; open that checkout to change what it runs.`;
}
```

`constructsByPath` keys by `catalogKeyFor(originPath)`. `readonlyPatterns` takes the same input shape (plus `workspaceIsClusterCheckout`) and, for every read-only key, emits BOTH `key` and `dsl/${key}` (sorted) -- a pattern for a path that does not exist is inert in `files.readonlyInclude`, and the two spellings are what a checkout and a bare tree respectively use. Update `reasonTooltip("remoteCluster", ...)` to say "Select the local cluster and open its checkout to edit it."

- [ ] **Step 4: `readonlyDecorations.ts`**

`update(constructs, cluster: { name: string; local?: boolean; checkout?: string } | undefined)` stores `this.checkout = cluster?.checkout ?? ""` and computes `this.workspaceIsClusterCheckout = (vscode.workspace.workspaceFolders ?? []).some((f) => samePath(f.uri.fsPath, this.checkout))` where `samePath` compares `path.resolve`d values (case-insensitive on win32). `provideFileDecoration`: compute the verdict with the new input; when the verdict is editable, the cluster is local, the workspace is NOT the checkout, the checkout is known, and the path IS in the catalog, return `{ badge: "L", tooltip: checkoutHint(this.clusterName, this.checkout), propagate: false }`. `writeSetting` passes `workspaceIsClusterCheckout` through.

- [ ] **Step 5: `receipt.ts`, `training.ts`, `trainingLens.ts`, `extension.ts`**

`receipt.ts`:

```ts
/** The directory `install.cloneStack` put the checkout in, or "" when the receipt records none. */
export function recordedStackDir(receipt: Receipt | null): string {
  if (!receipt) return "";
  const entry = entryFor(receipt, "stackCheckout");
  const dest = entry?.result?.dest ?? entry?.params?.dest;
  return typeof dest === "string" ? dest.trim() : "";
}
```

`training.ts`: `export const COMMAND_REBUILD = "memql.deployments.rebuildFromCheckout";` beside the other commands; `TrainingLensOptions` gains `cluster?: { name: string; local: boolean }`; in `trainingLensPlans`, for `construct.state === "edited"`: `detail` = cluster local ? `Your source differs from what ${name} loaded. Rebuild from checkout applies it.` : cluster known ? `Your source differs from what ${name} runs -- seeded constructs change by rollout.` : `detailFor("edited")`; `actions` = offerActions && cluster local ? `[{ title: "Rebuild from checkout", command: COMMAND_REBUILD }]` : `[]`. Other states are unchanged.

`trainingLens.ts`: `setCluster(cluster: { name: string; local: boolean } | undefined)` stored and passed as `cluster` to `trainingLensPlans`; fires `onDidChangeCodeLenses`.

`extension.ts`: `currentRunCluster` (line ~1971) returns `{ name, local, checkout: recordedStackDir(receipt) }` where the receipt is read once per connection change (`readReceipt(defaultReceiptPath())`) -- cache it beside the presence; `readonlyMarker.update(result.constructs, cluster)` passes it through; where the `TrainingCodeLensProvider` is created, call `setCluster` on every `connections.onDidChangeState`.

- [ ] **Step 6: Run the suite**

Run: `cd editors/vscode && npm test 2>&1 | tail -6`
Expected: pass.

- [ ] **Step 7: Commit**

```bash
git add editors/vscode/src/constructs/readonly.ts editors/vscode/src/constructs/readonlyDecorations.ts editors/vscode/src/install/receipt.ts editors/vscode/src/state/training.ts editors/vscode/src/constructs/trainingLens.ts editors/vscode/src/extension.ts editors/vscode/test/readonly.test.ts editors/vscode/test/training.test.ts editors/vscode/test/installReceipt.test.ts
git commit -m "vscode: a local cluster's checkout is editable; edited constructs offer Rebuild" -m "Refs memql#4244"
```

---

### Task 4: The image-source mode on the instance, and the checkout as part of it -- closes #4246 (part 1 of 2)

**Files:**
- Modify: `editors/vscode/src/install/receipt.ts` (`recordedImageSource`, `recordedRebuild`)
- Modify: `editors/vscode/src/state/deployments.ts` (`Instance.imageSource`, `Instance.checkout`, `Instance.rebuild`; `localInstance`; `RunKind` gains `"rebuild"`)
- Modify: `editors/vscode/src/state/deploymentsCatalog.ts` (`instanceRowStatus` description + tooltip for checkout mode)
- Modify: `editors/vscode/src/clusters/connectionView.ts` (`ConnectionViewInput.checkout?`; two rows)
- Modify: `editors/vscode/src/webview/connectionPanel.ts` (reads the receipt; passes the rows; "Open checkout" button)
- Modify: `editors/vscode/src/webview/addClusterPanel.ts` (`doneHtml`: "Open source checkout" in the handoff.ok branch and the terminal branch; `openCheckout` message)
- Modify: `editors/vscode/src/extension.ts` (command `memql.deployments.openCheckout`)
- Modify: `editors/vscode/package.json` (command + palette + `view/item/context` entry on `memqlLocalInstance`)
- Modify: `editors/vscode/test/installReceipt.test.ts`, `editors/vscode/test/deployments.test.ts`, `editors/vscode/test/deploymentsCatalog.test.ts`, the connectionView test file (`grep -l connectionView editors/vscode/test/*.ts`)

**Interfaces:**
- Produces (Task 5 consumes):

```ts
export type ImageSource = "released" | "checkout";
export interface RecordedRebuild { commit: string; ref: string; dirtyCount: number; nodes: string; recordedAt: string; }
export function recordedRebuild(receipt: Receipt | null): RecordedRebuild | undefined;   // the rebuildFromCheckout entry, when its result.imageSource === "checkout"
export function recordedImageSource(receipt: Receipt | null): ImageSource | "";          // "checkout" when a rebuild entry is newer than the clusterUp entry; "released" when clusterUp exists; "" otherwise
// Instance gains: imageSource?: ImageSource; checkout?: string; rebuild?: RecordedRebuild;
```

- [ ] **Step 1: Write the failing tests**

`installReceipt.test.ts`:

```ts
test("the image source is whichever lane ran last", () => {
  const r = emptyReceipt("install");
  assert.equal(recordedImageSource(r), "");
  r.entries.push(entry({ stepId: "clusterUp", script: "k3d.up", receipt: "cluster", params: { "image-tag": "v0.17.0" }, result: {}, recordedAt: "2026-08-20T10:00:00.000Z" }));
  assert.equal(recordedImageSource(r), "released");
  r.entries.push(entry({ stepId: "rebuildFromCheckout", script: "k3d.dev", receipt: "rebuild", params: { "image-source": "checkout" }, result: { imageSource: "checkout", commit: "abc1234def", ref: "tag:v0.17.0", dirtyCount: 4, nodes: "bff agent" }, recordedAt: "2026-08-21T10:00:00.000Z" }));
  assert.equal(recordedImageSource(r), "checkout");
  assert.deepEqual(recordedRebuild(r), { commit: "abc1234def", ref: "tag:v0.17.0", dirtyCount: 4, nodes: "bff agent", recordedAt: "2026-08-21T10:00:00.000Z" });
  // A later repair returns it to released images.
  r.entries = r.entries.map((e) => (e.stepId === "clusterUp" ? { ...e, recordedAt: "2026-08-22T10:00:00.000Z" } : e));
  assert.equal(recordedImageSource(r), "released");
});
```

`deployments.test.ts` (beside the existing `localInstance` tests): a receipt in checkout mode yields `imageSource: "checkout"`, `checkout: "/home/me/.memql/src"`, `rebuild.commit === "abc1234def"`; a released receipt yields `imageSource: "released"` and no `rebuild`; a null receipt yields neither.

`deploymentsCatalog.test.ts`: `instanceRowStatus` for a healthy local instance in checkout mode has `description === "healthy - checkout abc1234 (4 uncommitted)"` and a tooltip matching `/built from the checkout/` and `/returns it to released images/`; in released mode the description is unchanged (`healthy - v0.17.0`).

connectionView test: with `checkout: { path: "/home/me/.memql/src", ref: "tag:v0.17.0", imageSource: "checkout" }` the facts include `{ key: "checkout", value: "/home/me/.memql/src", note: "tag:v0.17.0" }` and `{ key: "image source", value: "checkout (built locally)", note: "an install, upgrade or repair returns it to released images" }`; with `imageSource: "released"` the value is `"released"` and the note is `""`; without `checkout` neither row appears.

- [ ] **Step 2: Run to verify they fail**

Run: `cd editors/vscode && npm test 2>&1 | grep -E "error TS|not ok" | head`
Expected: `tsc` errors.

- [ ] **Step 3: Implement**

`receipt.ts`:

```ts
export type ImageSource = "released" | "checkout";

export interface RecordedRebuild {
  commit: string;
  ref: string;
  dirtyCount: number;
  nodes: string;
  recordedAt: string;
}

/** The last rebuild, when its envelope says the cluster was pointed at checkout images. */
export function recordedRebuild(receipt: Receipt | null): RecordedRebuild | undefined {
  if (!receipt) return undefined;
  const entry = entryFor(receipt, "rebuildFromCheckout");
  if (entry === undefined || entry.result?.imageSource !== "checkout") return undefined;
  const str = (v: unknown) => (typeof v === "string" ? v : "");
  const n = Number(entry.result.dirtyCount);
  return { commit: str(entry.result.commit), ref: str(entry.result.ref), dirtyCount: Number.isFinite(n) ? n : 0, nodes: str(entry.result.nodes), recordedAt: entry.recordedAt };
}

/**
 * Which lane set the node images last. `released` is what install, upgrade
 * and repair leave (clusterUp --image-tag); `checkout` is what a rebuild
 * leaves. Decided by recordedAt, because both entries survive in the receipt
 * and only the order says which one the cluster currently runs.
 */
export function recordedImageSource(receipt: Receipt | null): ImageSource | "" {
  if (!receipt) return "";
  const up = entryFor(receipt, "clusterUp");
  const rebuild = recordedRebuild(receipt);
  if (rebuild !== undefined && (up === undefined || rebuild.recordedAt > up.recordedAt)) return "checkout";
  return up === undefined ? "" : "released";
}
```

`deployments.ts`: `RunKind` gains `"rebuild"`; `Instance` gains `imageSource?: ImageSource; checkout?: string; rebuild?: RecordedRebuild;` (doc: "Local only. `checkout` is the directory the install cloned; `imageSource` is which lane set the images last; `rebuild` is the last rebuild's facts, present in checkout mode."); `localInstance` fills them from `recordedStackDir`, `recordedImageSource`, `recordedRebuild`, omitting empty values the way `version` is omitted.

`deploymentsCatalog.ts` `instanceRowStatus`: when `instance.imageSource === "checkout" && instance.rebuild`, `versionText` becomes `checkout ${rebuild.commit.slice(0, 7)}${rebuild.dirtyCount > 0 ? ` (${rebuild.dirtyCount} uncommitted)` : ""}` and the tooltip appends `\nRunning images built from the checkout at ${commit.slice(0, 7)} (${dirtyCount} uncommitted files when it was built). An install, upgrade or repair returns it to released images.`

`connectionView.ts`: `ConnectionViewInput.checkout?: { path: string; ref: string; imageSource: ImageSource | "" }`; when present, push the two facts after `version` exactly as the test spells them. `connectionPanel.ts`: read `readReceipt(defaultReceiptPath())` when the cluster is local, pass `checkout`, and add a `data-act="openCheckout"` button beside "Open Portal" for a local cluster with a recorded checkout, dispatching `commands.executeCommand("memql.deployments.openCheckout")`.

`addClusterPanel.ts` `doneHtml`: in the `handoff.ok` branch and in the terminal branch, when `recordedStackDir(receipt)` is non-empty (read the receipt the panel already holds), add `<button class="secondary" type="button" data-act="openCheckout">Open source checkout</button>`; `onMessage` routes it to the same command.

`extension.ts`: register `memql.deployments.openCheckout`: read the receipt, `recordedStackDir`; empty -> `window.showInformationMessage("MemQL: no checkout is recorded for the local cluster. Install or repair it to clone one.")`; otherwise if no workspace folder is open, `commands.executeCommand('vscode.openFolder', Uri.file(dir), { forceNewWindow: false })`, else `{ forceNewWindow: true }`. `package.json`: the command (title "MemQL: Open Local Checkout", icon `$(folder-opened)`), a palette entry, and a `view/item/context` entry `"when": "view == memqlDeployments && viewItem == memqlLocalInstance"`, group `inline@1`.

- [ ] **Step 4: Run the suite**

Run: `cd editors/vscode && npm test 2>&1 | tail -6`
Expected: pass.

- [ ] **Step 5: Commit**

```bash
git add editors/vscode/src/install/receipt.ts editors/vscode/src/state/deployments.ts editors/vscode/src/state/deploymentsCatalog.ts editors/vscode/src/clusters/connectionView.ts editors/vscode/src/webview/connectionPanel.ts editors/vscode/src/webview/addClusterPanel.ts editors/vscode/src/extension.ts editors/vscode/package.json editors/vscode/test/installReceipt.test.ts editors/vscode/test/deployments.test.ts editors/vscode/test/deploymentsCatalog.test.ts
git add <the connectionView test file>
git commit -m "vscode: a local instance records its image source and knows its checkout" -m "Refs memql#4246"
```

---

### Task 5: Rebuild from checkout -- the action, the preflight, the run -- closes #4246 (part 2 of 2)

**Files:**
- Create: `editors/vscode/src/install/checkoutState.ts` (pure parse of git output + the spawn helper, modelled on `tags.ts`)
- Create: `editors/vscode/src/state/rebuildPreflight.ts` (pure: the checklist items)
- Create: `editors/vscode/test/checkoutState.test.ts`, `editors/vscode/test/rebuildPreflight.test.ts`
- Modify: `editors/vscode/src/install/session.ts` (`rebuildPlan`, `runRebuild`, `SessionOptions.nodes`)
- Modify: `editors/vscode/src/install/graph.ts` (`rebuildGraphPath(repoRoot)`)
- Modify: `editors/vscode/src/deploy/instanceActions.ts` (`"rebuildFromCheckout"` / `"rebuildGraph"`; header table)
- Modify: `editors/vscode/src/deploy/upgrade.ts` (`confirmationMessage` states the lane crossing)
- Modify: `editors/vscode/src/state/preflight.ts` (`PreflightInputs.imageSource`; the lane item)
- Modify: `editors/vscode/src/webview/installScreens.ts` (export `renderPreflight`; `RunMode` gains `"rebuild"`; `renderRebuildScreen`)
- Modify: `editors/vscode/src/webview/deploymentPanel.ts` (`Screen` gains `"rebuildPreflight"`; `choose("rebuildFromCheckout")`; `startRebuild`)
- Modify: `editors/vscode/src/webview/addClusterPanel.ts` (`computePreflight` passes `imageSource`)
- Modify: `editors/vscode/src/extension.ts` (command `memql.deployments.rebuildFromCheckout`; catalog refresh after a rebuild)
- Modify: `editors/vscode/package.json` (command, palette, `view/item/context` on `memqlLocalInstance`)
- Modify: `editors/vscode/README.md` ("What an instance offers" row; a "Rebuild from checkout" paragraph + the mode), `docs/public/language/vscode-runtime-panel-verification.md` (manual case)
- Modify: `editors/vscode/test/instanceActions.test.ts`, `editors/vscode/test/installSession.test.ts`, `editors/vscode/test/preflight.test.ts`, `editors/vscode/test/deploymentUpgrade.test.ts` (or `upgradeButton.test.ts`)

**Interfaces:**
- Consumes: Task 2's flags and `rebuild.json`; Task 3's `COMMAND_REBUILD`, `recordedStackDir`; Task 4's `Instance.checkout` / `.imageSource`, `RunKind "rebuild"`.
- Produces:

```ts
// checkoutState.ts
export interface CheckoutState { commit: string; ref: { kind: "tag" | "branch" | "detached"; name: string }; dirtyCount: number; deployDirty: boolean; }
export function parseCheckoutState(raw: { head: string; tag: string; branch: string; status: string }): CheckoutState;
export async function readCheckoutState(dir: string, run?: (args: string[]) => Promise<string>): Promise<CheckoutState | undefined>;  // undefined when git is unavailable or dir is not a checkout
// rebuildPreflight.ts
export interface RebuildPreflightInputs { dockerReachable: boolean; checkoutDir: string; checkoutIsMemql: boolean; state?: CheckoutState; nodes: string; imageSource: ImageSource | ""; releasedTag: string; }
export function rebuildPreflightItems(inputs: RebuildPreflightInputs): PreflightItem[];
// session.ts
export function rebuildPlan(opts: SessionOptions): (step: Step) => StepPlan;      // rebuildFromCheckout -> present({ "repo-root": stackDir, "app-name": opts.appName, node: opts.nodes })
export async function runRebuild(opts: SessionOptions, hooks: SessionHooks): Promise<ExecutionReport>;  // loadGraphFile(rebuildGraphPath(opts.root)) + execute(..., rebuildPlan(opts), ..., opts.receiptFile)
// SessionOptions gains: nodes?: string (comma-separated, "" = all app nodes); appName?: string
```

- [ ] **Step 1: Write the failing tests**

`checkoutState.test.ts`:

```ts
import test from "node:test";
import assert from "node:assert/strict";

import { parseCheckoutState } from "../src/install/checkoutState.js";

test("a tag checkout is named as one, with its dirtiness counted", () => {
  const s = parseCheckoutState({ head: "abc1234def\n", tag: "v0.17.0\n", branch: "", status: " M dsl/cognition/queries.memql\n?? deploy/x.yaml\n" });
  assert.deepEqual(s, { commit: "abc1234def", ref: { kind: "tag", name: "v0.17.0" }, dirtyCount: 2, deployDirty: true });
});

test("a branch beats detached; detached is named when neither resolves", () => {
  assert.deepEqual(parseCheckoutState({ head: "a", tag: "", branch: "main\n", status: "" }).ref, { kind: "branch", name: "main" });
  assert.deepEqual(parseCheckoutState({ head: "a", tag: "", branch: "", status: "" }).ref, { kind: "detached", name: "" });
  assert.equal(parseCheckoutState({ head: "a", tag: "", branch: "", status: "" }).deployDirty, false);
});
```

`rebuildPreflight.test.ts`:

```ts
import test from "node:test";
import assert from "node:assert/strict";

import { rebuildPreflightItems } from "../src/state/rebuildPreflight.js";

const base = {
  dockerReachable: true,
  checkoutDir: "/home/me/.memql/src",
  checkoutIsMemql: true,
  state: { commit: "abc1234def", ref: { kind: "tag" as const, name: "v0.17.0" }, dirtyCount: 4, deployDirty: false },
  nodes: "",
  imageSource: "released" as const,
  releasedTag: "v0.17.0",
};

test("the checklist states every fact the design names, in order", () => {
  const labels = rebuildPreflightItems(base).map((i) => i.label);
  assert.deepEqual(labels, ["Docker", "Checkout", "Git state", "Nodes", "Image source", "Duration"]);
});

test("crossing from released images is stated, and staying in checkout mode is not", () => {
  const crossing = rebuildPreflightItems(base).find((i) => i.label === "Image source")!;
  assert.equal(crossing.state, "note");
  assert.match(crossing.detail, /switches local to images built from your checkout/);
  assert.match(crossing.detail, /install, upgrade or repair returns it to released v0.17.0 images/);
  const staying = rebuildPreflightItems({ ...base, imageSource: "checkout" }).find((i) => i.label === "Image source")!;
  assert.equal(staying.state, "ok");
});

test("a dirty deploy/ tree is called out because manifests do not ride a rebuild", () => {
  const git = rebuildPreflightItems({ ...base, state: { ...base.state, deployDirty: true } }).find((i) => i.label === "Git state")!;
  assert.equal(git.state, "note");
  assert.match(git.detail, /deploy\/ has edits/);
  assert.match(git.detail, /manifests do not ride a rebuild/);
});

test("a missing checkout or an unreachable docker is a note naming the fix", () => {
  assert.match(rebuildPreflightItems({ ...base, dockerReachable: false })[0]!.detail, /Docker/);
  assert.equal(rebuildPreflightItems({ ...base, dockerReachable: false })[0]!.state, "note");
  assert.match(rebuildPreflightItems({ ...base, checkoutIsMemql: false })[1]!.detail, /not a MemQL checkout/);
});

test("the node line names the default honestly", () => {
  assert.match(rebuildPreflightItems(base).find((i) => i.label === "Nodes")!.detail, /all app nodes/);
  assert.match(rebuildPreflightItems({ ...base, nodes: "bff,agent" }).find((i) => i.label === "Nodes")!.detail, /bff, agent/);
});
```

`instanceActions.test.ts`: a local installed instance with `checkout` set offers `["createDeployment", "repair", "rebuildFromCheckout", "uninstall"]` (healthy) and `["repair", "createDeployment", "rebuildFromCheckout", "uninstall"]` (unreachable); without `checkout` the action is absent; `flow === "rebuildGraph"`.

`installSession.test.ts`: `rebuildPlan({ ...opts, stackDir: "/home/me/.memql/src", appName: "memql-local", nodes: "bff,agent" })` on the `rebuildFromCheckout` step yields `{ action: "run", params: { "repo-root": "/home/me/.memql/src", "app-name": "memql-local", node: "bff,agent" } }`, and with `nodes: ""` the `node` key is absent; `runRebuild` with an injected `hooks.run` (see how the existing `runInstall` tests inject one) invokes `k3d.dev` once with those params merged under the graph's pinned `image-source: checkout`.

`preflight.test.ts`: `preflightItems({ ...inputs, imageSource: "checkout", releasedTag: "v0.17.0" })` contains a `note` item whose detail matches `/returns local to released v0.17.0 images/`; with `imageSource: "released"` it does not.

`deploymentUpgrade.test.ts` / `upgradeButton.test.ts`: the `offer` confirmation for a local instance in checkout mode contains `returns local to released`.

- [ ] **Step 2: Run to verify they fail**

Run: `cd editors/vscode && npm test 2>&1 | grep -E "error TS|not ok" | head`
Expected: `tsc` errors.

- [ ] **Step 3: Implement the pure modules**

`checkoutState.ts`:

```ts
// What the recorded checkout is, right now: commit, ref, dirtiness.
//
// Read with git at the moment it matters (the rebuild preflight), never from
// the receipt -- the receipt says what was cloned; this says what is there
// today. Same spawn discipline as tags.ts: no shell, cwd set to the checkout,
// a timeout, and undefined rather than an exception when git cannot answer.

import { spawn } from "node:child_process";

export interface CheckoutState {
  commit: string;
  ref: { kind: "tag" | "branch" | "detached"; name: string };
  dirtyCount: number;
  /** Whether anything under deploy/ is modified -- manifests do not ride a rebuild. */
  deployDirty: boolean;
}

export function parseCheckoutState(raw: { head: string; tag: string; branch: string; status: string }): CheckoutState {
  const lines = raw.status.split("\n").filter((l) => l.trim() !== "");
  const tag = raw.tag.trim();
  const branch = raw.branch.trim();
  return {
    commit: raw.head.trim(),
    ref: tag !== "" ? { kind: "tag", name: tag } : branch !== "" ? { kind: "branch", name: branch } : { kind: "detached", name: "" },
    dirtyCount: lines.length,
    deployDirty: lines.some((l) => l.slice(3).startsWith("deploy/")),
  };
}

type Run = (args: string[]) => Promise<string>;

export async function readCheckoutState(dir: string, run: Run = (args) => git(dir, args)): Promise<CheckoutState | undefined> {
  try {
    const head = await run(["rev-parse", "HEAD"]);
    const tag = await run(["describe", "--exact-match", "--tags", "HEAD"]).catch(() => "");
    const branch = await run(["symbolic-ref", "--short", "-q", "HEAD"]).catch(() => "");
    const status = await run(["status", "--porcelain"]);
    return parseCheckoutState({ head, tag, branch, status });
  } catch {
    return undefined;
  }
}

function git(cwd: string, args: string[], timeoutMs = 10_000): Promise<string> {
  return new Promise((resolve, reject) => {
    const child = spawn("git", args, { cwd, stdio: ["ignore", "pipe", "pipe"], shell: false });
    let out = "";
    let err = "";
    const timer = setTimeout(() => child.kill(), timeoutMs);
    child.stdout.on("data", (d) => { out += String(d); });
    child.stderr.on("data", (d) => { err += String(d); });
    child.on("error", (e) => { clearTimeout(timer); reject(e); });
    child.on("close", (code) => {
      clearTimeout(timer);
      if (code === 0) resolve(out);
      else reject(new Error(err.trim() || `git ${args[0]} exited ${code}`));
    });
  });
}
```

`rebuildPreflight.ts`:

```ts
// The "Before it runs" list for a rebuild (design 5.3). Pure: the panel
// gathers the facts, this states them. Every item is a sentence the operator
// can check, and the lane crossing is stated here rather than discovered in
// the Deployments row afterwards.

import type { PreflightItem } from "./preflight.js";
import type { CheckoutState } from "../install/checkoutState.js";
import type { ImageSource } from "../install/receipt.js";

export interface RebuildPreflightInputs {
  dockerReachable: boolean;
  checkoutDir: string;
  checkoutIsMemql: boolean;
  state?: CheckoutState;
  /** Comma-separated node types, or "" for all app nodes. */
  nodes: string;
  imageSource: ImageSource | "";
  releasedTag: string;
}

export function rebuildPreflightItems(i: RebuildPreflightInputs): PreflightItem[] {
  const items: PreflightItem[] = [];
  items.push(
    i.dockerReachable
      ? { label: "Docker", detail: "The Docker daemon answers.", state: "ok" }
      : { label: "Docker", detail: "Docker is not answering. Start Docker Desktop (or the daemon) before running.", state: "note" },
  );
  items.push(
    i.checkoutIsMemql
      ? { label: "Checkout", detail: `${i.checkoutDir} -- the checkout the install recorded.`, state: "ok" }
      : { label: "Checkout", detail: `${i.checkoutDir} is not a MemQL checkout (no Dockerfile or local overlay). Repair the install to clone one.`, state: "note" },
  );
  if (i.state === undefined) {
    items.push({ label: "Git state", detail: "git could not read the checkout; what gets built will not be recorded.", state: "note" });
  } else {
    const ref = i.state.ref.kind === "detached" ? "detached HEAD" : `${i.state.ref.kind} ${i.state.ref.name}`;
    const dirty = i.state.dirtyCount === 0 ? "clean" : `${i.state.dirtyCount} uncommitted file${i.state.dirtyCount === 1 ? "" : "s"}`;
    const deploy = i.state.deployDirty ? " deploy/ has edits -- manifests do not ride a rebuild." : "";
    items.push({ label: "Git state", detail: `${ref} at ${i.state.commit.slice(0, 7)}, ${dirty}.${deploy}`, state: i.state.deployDirty ? "note" : "ok" });
  }
  items.push({
    label: "Nodes",
    detail: i.nodes.trim() === "" ? "all app nodes (the script's default)." : i.nodes.split(",").map((n) => n.trim()).filter(Boolean).join(", ") + ".",
    state: "ok",
  });
  items.push(
    i.imageSource === "checkout"
      ? { label: "Image source", detail: "local already runs checkout-built images; this rebuilds them.", state: "ok" }
      : {
          label: "Image source",
          detail: `This switches local to images built from your checkout. An install, upgrade or repair returns it to released ${i.releasedTag || "release"} images.`,
          state: "note",
        },
  );
  items.push({ label: "Duration", detail: "A first build takes minutes; later builds reuse Docker's cache.", state: "ok" });
  return items;
}
```

(Read `state/preflight.ts` for `PreflightItem`'s exact field names and `state` values before writing this; if its states are not `"ok" | "note"`, use its vocabulary and adjust the tests' expectations in the same commit, saying so in the report.)

- [ ] **Step 4: Session, plan, graph path**

`graph.ts`: `export function rebuildGraphPath(repoRoot: string): string { return path.join(repoRoot, "scripts", "install", "graph", "rebuild.json"); }` (the document carries `kind: "install"`, so `loadGraphFile` accepts it unchanged).

`session.ts`: `SessionOptions` gains `nodes?: string` and `appName?: string`;

```ts
export function rebuildPlan(opts: SessionOptions): (step: Step) => StepPlan {
  const stackDir = resolveStackDir(opts);
  return (step) => {
    if (step.id !== "rebuildFromCheckout") return { action: "skip", reason: `not a rebuild step: ${step.id}` };
    return { action: "run", params: { ...present({ "repo-root": stackDir, "app-name": opts.appName, node: opts.nodes }), ...(opts.stepParams[step.id] ?? {}) } };
  };
}

export async function runRebuild(opts: SessionOptions, hooks: SessionHooks): Promise<ExecutionReport> {
  const graph = hooks.graph ?? (await loadGraphFile(rebuildGraphPath(opts.root)));
  return execute(graph, rebuildPlan(opts), opts, hooks, opts.receiptFile);
}
```

`state/preflight.ts`: `PreflightInputs` gains `imageSource?: ImageSource | ""; releasedTag?: string`; `preflightItems` appends, when `imageSource === "checkout"`, `{ label: "Image source", detail: `This returns local to released ${releasedTag || "release"} images; it is running a checkout build today. Rebuild from checkout brings them back.`, state: "note" }`. `addClusterPanel.ts` `computePreflight` passes `imageSource: recordedImageSource(receipt)` and `releasedTag: recordedCheckout(receipt).tag`.

`upgrade.ts` `confirmationMessage`: when `instance.imageSource === "checkout"` append ` This returns ${instance.name} to released images; it runs a checkout build today.`

- [ ] **Step 5: The action and the panel**

`instanceActions.ts`: `InstanceActionId` gains `"rebuildFromCheckout"`, `InstanceActionFlow` gains `"rebuildGraph"` (doc: "The one-step rebuild graph: k3d.dev over the recorded checkout, image-source=checkout."), `const REBUILD: InstanceAction = { id: "rebuildFromCheckout", label: "Rebuild from checkout", detail: "Build images from the recorded checkout, import them, and roll the cluster onto them.", flow: "rebuildGraph" }`; the two local arrays become `[REPAIR, CREATE_LOCAL_INSTALLED, ...(hasCheckout ? [REBUILD] : []), UNINSTALL]` and `[CREATE_LOCAL_INSTALLED, REPAIR, ...(hasCheckout ? [REBUILD] : []), UNINSTALL]` with `hasCheckout = (instance.checkout ?? "") !== ""`; the header table gains the row `//   Rebuild from checkout  --  k3d.dev over the checkout  --`.

`installScreens.ts`: export `renderPreflight`; `RunMode` gains `"rebuild"` with heading "Rebuilding the local cluster from its checkout" and lede "Build, import, point the cluster at the images, restart. Each step reports as it goes."; add

```ts
export interface RebuildScreenInput { checkoutDir: string; nodes: string; preflight?: readonly PreflightItem[]; }
export function renderRebuildScreen(input: RebuildScreenInput): string
```

rendering `<h1>Rebuild from checkout</h1>`, a lede naming the checkout, one text field `nodes` (label "Node types to rebuild (comma-separated; empty = all app nodes)"), `renderPreflight(input.preflight)`, and `Start` / `Back` buttons (`data-act="beginRebuild"` / `"back"`).

`deploymentPanel.ts`: `Screen` gains `"rebuildPreflight"`; `choose("rebuildFromCheckout")` sets the screen, gathers facts (docker: the same probe the wizard's `computePreflight` uses -- reuse its function; checkout dir = `instance.checkout`; `checkoutIsMemql` = both `Dockerfile` and `deploy/k8s/overlays/local/kustomization.yaml` exist; `readCheckoutState(dir)`; `imageSource`, `releasedTag` from the instance/receipt), renders; `beginRebuild` runs `runRebuild(installSessionOptions-shaped options with root: this.deps.installRoot, receiptFile: this.deps.receiptFile, stackDir: instance.checkout, appName: undefined (the script's default), nodes, timeoutMs: 2_700_000, env: this.sudoEnv?.() ?? undefined)` through the same `RunRecorder.begin({ kind: "rebuild", ... })` + `onEvent` folding `startDeploy` uses, with `RunMode "rebuild"`; on success: `this.deps.refreshTree()`, `void vscode.commands.executeCommand("memql.constructs.refresh")`, and `vscode.window.showInformationMessage(`Rebuilt ${nodes} -- ${instance.name} now runs your checkout (${commit7}, ${dirty} uncommitted files).`)` reading `nodes`, `commit`, `dirtyCount` off the step's envelope; on failure the existing `failedStep` screen.

`extension.ts`: `memql.deployments.rebuildFromCheckout` opens the Deployments panel for the local instance and calls its `choose("rebuildFromCheckout")` (add a `DeploymentPanel.openAction(context, deps, "rebuildFromCheckout")` static if needed); `memql.constructs.refresh` also triggers the LSP catalog publisher's `refresh()` (expose the publisher to the runtime surface through a module-level `let catalogPublisher` set in `registerRunSurface`). `package.json`: the command (title "MemQL: Rebuild Local Cluster From Checkout", icon `$(tools)`), palette entry, `view/item/context` entry `"when": "view == memqlDeployments && viewItem == memqlLocalInstance"`, group `lifecycle@0`.

- [ ] **Step 6: Docs**

`editors/vscode/README.md`: the "What an instance offers" table gains `| Rebuild from checkout | -- | k3d.dev over the recorded checkout | -- |`; after the "Re-running the install graph is the repair..." paragraph add: "**Rebuild from checkout is a fourth, separate one-step graph.** A wizard install runs released images pulled at a tag; the checkout it cloned is inert until something builds from it. Rebuild does: it builds the node images from that checkout, imports them, points the cluster's Application at them (keeping the database operand where it is), and restarts. From then on the instance row reads `checkout <commit> (<n> uncommitted)` instead of a version, and the Connection page says so. An install, upgrade or repair returns the cluster to released images -- and says so before it runs." `docs/public/language/vscode-runtime-panel-verification.md`: a manual case "Rebuild from checkout on a wizard-installed cluster" (preflight shows the six lines; after the run the row reads `checkout`; a repair's preflight names the return to released images).

- [ ] **Step 7: Run everything**

Run: `cd editors/vscode && npm test 2>&1 | tail -6 && cd ../.. && make vscode-test-host 2>&1 | tail -6 && go test ./cmd/memql-lsp/ -run 'VSCode|Vscode' 2>&1 | tail -2`
Expected: all green. A live rebuild is not run on this machine; say so in the report.

- [ ] **Step 8: Commit**

```bash
git add editors/vscode/src/install/checkoutState.ts editors/vscode/src/state/rebuildPreflight.ts editors/vscode/test/checkoutState.test.ts editors/vscode/test/rebuildPreflight.test.ts editors/vscode/src/install/session.ts editors/vscode/src/install/graph.ts editors/vscode/src/deploy/instanceActions.ts editors/vscode/src/deploy/upgrade.ts editors/vscode/src/state/preflight.ts editors/vscode/src/webview/installScreens.ts editors/vscode/src/webview/deploymentPanel.ts editors/vscode/src/webview/addClusterPanel.ts editors/vscode/src/extension.ts editors/vscode/package.json editors/vscode/README.md docs/public/language/vscode-runtime-panel-verification.md editors/vscode/test/instanceActions.test.ts editors/vscode/test/installSession.test.ts editors/vscode/test/preflight.test.ts
git add <the upgrade test file>
git commit -m "vscode: Rebuild from checkout -- the action, its preflight, and the image-source mode it sets" -m "Refs memql#4246"
```

---

## Self-review (done while writing)

- Spec coverage: 3.2-3.3 (Task 3), 3.4 (Tasks 1 + 3), 5.1 (Task 4 + the lane statements in Task 5), 5.2 (Tasks 2 + 5), 5.3 (Task 5), 5.4 (Tasks 2 + 5: receipt entry, catalog refresh, toast), 5.5 (Task 3 lens + Task 5 command), 5.6 (Task 4: done-screen, row, Connection page), 5.7 (stated in README, Task 5). Section 7's tests map task by task; 8's docs land with their tasks.
- Type consistency: `recordedStackDir` (Task 3) is what Task 4's `localInstance` and PR 2 use; `ImageSource` / `RecordedRebuild` (Task 4) are what Task 5's preflight and `upgrade.ts` read; `COMMAND_REBUILD` (Task 3) is the command Task 5 registers; `rebuildFromCheckout` is the step id in `rebuild.json` (Task 2), `rebuildPlan` (Task 5) and `recordedRebuild` (Task 4); the envelope fields `imageSource` / `commit` / `ref` / `dirtyCount` / `nodes` are spelled identically in `dev.sh` (Task 2) and the readers (Tasks 4-5).
- Ruling, recorded in the ledger too: `rebuild.json` carries `kind: "install"` so neither `graph.go` nor `graph.ts` needs a third kind; the Go embed + the scan-list additions are what make the graph gates and the DSL-agreement test see it, and the DSL action exists because that test then requires one.
