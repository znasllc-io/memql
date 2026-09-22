# Procedure Learning Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn recorded work-spine steps into parameterized MemQL automations by a pure Go module that spends no model, and wire it to the rows so a learned construct enters the catalog as an inactive candidate with provenance.

**Architecture:** `component/procedure` is a new leaf Go module holding the algorithms as functions over values -- canonicalize, symbolize, mine, structure, generalize, classify, score, select -- importing only the standard library and `component/work`. `integrations/procedure` is the only half that touches rows: it reads `v1:work:step` / `v1:work:observation` into pure values, runs the pipeline, renders the winner through the existing deterministic lift, and persists it as a validated bundle. One bounded model call may propose a derivation for a hole the rules cannot explain; the CHECK of that proposal is pure and lives in the module.

**Tech Stack:** Go 1.26.1, MemQL DSL, PostgreSQL + TimescaleDB (wiring only; the module is database-free).

**Spec:** `docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md` -- section 4 "Epic C -- Learning", decisions D6, D13, D14, D24.

## Global Constraints

- **Purity is a build-graph fact.** `go list -deps github.com/znasllc-io/memql/component/procedure` must show only the standard library and `github.com/znasllc-io/memql/component/work`. No engine, no database, no provider, no `core/`.
- **Induction spends no model** (D6). The single permitted model call proposes a derivation for one unexplained hole and is made by `integrations/procedure`, never by the module.
- **Hole classification order is data flow, then constant, then free parameter** (D13). A free parameter is priced ABOVE a data-flow hole in the score.
- **Compression is the score; two uses is the floor** (D14). The library grows one abstraction at a time, rewriting the corpus between acceptances.
- **Two corpus levels** (D24): an action inside a session, and an automation invocation as a subrun step. One pipeline, both levels.
- **Nothing auto-activates.** A lifted construct is persisted `status: "validated"` on the bundle and never `active`.
- **Values, not constants:** the symbolization budget, mining support, gap tolerance and argument ceiling are parameters with the defaults this plan names, never literals buried in a function.
- **Vocabulary:** a procedure is a construct. Never a fourth extension word; never "skill" (`v1:skills:*` means something else here).
- **No environment branching.** Nothing here names a deploy tier.
- **Commit format:** `Issue #<N>: <description>`. Branch `epic/procedure-learning`; never commit to main.

---

## File Structure

**`component/procedure/` -- the new leaf module (pure).** One file per pipeline stage, because each stage is independently testable and a reviewer can reject one without the others.

| File | Responsibility |
|---|---|
| `go.mod` | Module manifest; requires `component/work` only |
| `doc.go` | Package doc: the pipeline order, the purity rule, why no model |
| `value.go` | The input value types (`Step`, `Call`) and the argument tree (`Node`) |
| `canonicalize.go` | `Canonicalize` -- steps to actions; argv/JSON/path parsing; noise drop |
| `symbolize.go` | `AntiUnify`, `Distance`, `Symbolize` -- clustering under a budget |
| `mine.go` | `Mine` -- closed frequent sub-sequences, gap tolerance, remove-and-remine |
| `structure.go` | `Structure` -- the inductive miner, so a retry loop is a loop node |
| `generalize.go` | `Generalize`, `Classify` -- templates and D13's hole order |
| `derive.go` | The derivation expression language and `CheckDerivation` (the pure half of the one model call) |
| `score.go` | `Score`, `Select` -- D14's compression utility and the one-at-a-time loop |
| `params.go` | `Params` -- the budget, support, gap and ceilings as values with defaults |
| `purity_test.go` | The `go list -deps` build-graph gate |
| `reference/` | The parity harness: runs the shared fixtures through reference implementations, skipped when absent |
| `testdata/` | Golden fixtures, one directory per claim, each with its negative control |

**`integrations/procedure/` -- the wiring (the only half that touches rows).**

| File | Responsibility |
|---|---|
| `plugin.go` | `memql.RegisterPlugin("procedure", ...)`; the two builtin executors |
| `corpus.go` | Rows to `[]procedure.Step`, both D24 levels, disliked excluded |
| `learn.go` | The pipeline run: canonicalize, symbolize, mine, generalize, classify, score, select |
| `derive.go` | The one bounded model call at level `reasoning`, checked by `procedure.CheckDerivation` |
| `lift.go` | Template to MemQL source through the extended transcript renderer |
| `persist.go` | Bundle + construct, `goalSignature`, the D9 provenance stamp, the compile gate |

**Modified:**

- `integrations/planner/agent_loop_authoring_transcript.go` -- export the renderer and add the template-with-holes variant
- `dsl/authoring/mutations.memql` -- no schema change; `recordConstructGoalSignature` gains its first production caller
- `dsl/procedure/` -- new DSL domain: `memql.toml`, `builtins.memql`, `automations.memql`, `prompts.memql`, `prompts/deriveHole.tmpl`
- The new-module tax: `go.work`, `go.mod`, `integrations/go.mod`, `Dockerfile`, `cmd/deploy-gate-check/Dockerfile`, `scripts/ci/db-gated-packages.sh`, `module_taxonomy_test.go`

---

### Task 1: The module boundary and its purity gate

Covers issue #5403's third acceptance criterion. Nothing in this task computes anything; it makes the boundary a build-graph fact before any algorithm leans on it.

**Files:**
- Create: `component/procedure/go.mod`, `component/procedure/doc.go`, `component/procedure/params.go`, `component/procedure/purity_test.go`
- Modify: `go.work`, `go.mod`, `integrations/go.mod`, `Dockerfile`, `cmd/deploy-gate-check/Dockerfile`, `scripts/ci/db-gated-packages.sh`, `module_taxonomy_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: the module path `github.com/znasllc-io/memql/component/procedure`; `procedure.Params` with fields `SymbolBudget int`, `MinSupport int`, `Gap int`, `MaxArgs int`, `Noise float64`, and `procedure.DefaultParams() Params` returning `{SymbolBudget: 2, MinSupport: 2, Gap: 2, MaxArgs: 8, Noise: 0.2}`.

- [ ] **Step 1: Create the module manifest**

```
// component/procedure/go.mod
// Epic memql#5402, design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md
// section 4 epic C, decision D6. The LEARNING half of the work spine, pure:
// functions over values that turn recorded steps into a parameterized
// template. It must stay reachable from integrations/procedure with
// GOWORK=off, which is why it is a module rather than a root-module package.
module github.com/znasllc-io/memql/component/procedure

go 1.26.1

toolchain go1.27.1

require github.com/znasllc-io/memql/component/work v0.0.0

replace github.com/znasllc-io/memql/component/work => ../work
```

- [ ] **Step 2: Write the purity gate BEFORE any code it guards**

```go
// component/procedure/purity_test.go
package procedure

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// allowedNonStdlib is the whole exemption list, and it has one entry on
// purpose. component/work is the work spine's own pure half -- GoalSignature
// is the key this module's output is filed under, and re-deriving it here
// would be a second copy that drifts.
var allowedNonStdlib = map[string]bool{
	"github.com/znasllc-io/memql/component/work": true,
}

// TestProcedureImportsNothingBeyondStdlibAndWork is what makes "induction
// spends no model" checkable rather than promised (design D6).
//
// The claim this epic rests on is that a recorded corpus becomes a
// parameterized construct with NO provider call. A package that could reach a
// provider, an engine or a database is one nobody can check without running
// them. `go list -deps` is the build graph, and the build graph has no
// opinions.
//
// WHY A MODULE AND NOT A PACKAGE BOUNDARY. component/proving answers the same
// question with a package boundary plus a gate, and says so, because a nested
// module fires roughly twelve repo-wide gates. That answer is not available
// here: integrations/procedure must import this code, integrations is its own
// module, and it does not require the root module -- so a root-module package
// is unreachable from it with GOWORK=off. The twelve-gate tax is paid in
// Task 1 for that reason and no other.
func TestProcedureImportsNothingBeyondStdlibAndWork(t *testing.T) {
	const pkg = "github.com/znasllc-io/memql/component/procedure"
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}
	var offenders []string
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep = strings.TrimSpace(dep)
		switch {
		case dep == "" || dep == pkg:
		case allowedNonStdlib[dep]:
		case !strings.Contains(strings.SplitN(dep, "/", 2)[0], "."):
			// A standard-library path's first segment carries no dot,
			// because a module path's does.
		default:
			offenders = append(offenders, dep)
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("component/procedure imports outside stdlib + component/work:\n  %s\n\n"+
			"This module's whole value is that its decisions can be checked without "+
			"running anything. If the import is genuinely needed, the code belongs in "+
			"integrations/procedure, which may import anything.",
			strings.Join(offenders, "\n  "))
	}
}
```

- [ ] **Step 3: Run it and watch it fail for the right reason**

Run: `cd component/procedure && go test -run TestProcedureImportsNothing ./...`
Expected: FAIL -- `no Go files in .../component/procedure`. That is the negative control for the gate itself: it cannot pass before there is a package.

- [ ] **Step 4: Add the package doc and the parameters**

```go
// component/procedure/doc.go

// Package procedure turns recorded work-spine steps into a parameterized
// template, spending no model.
//
// The pipeline, in order, each stage a function over values:
//
//	Canonicalize(steps)            -> []Action   each step as tool + argument tree + digests
//	Symbolize(actions, params)     -> []Symbol   same-tool actions clustered by anti-unification distance
//	Mine(sequences, params)        -> []Pattern  closed frequent sub-sequences, gap-tolerant
//	Structure(sequences, params)   -> ProcessTree the inductive miner, so a retry is a loop node
//	Generalize(instances)          -> Template   instances anti-unified into holes
//	Classify(template, instances)  -> []Hole     data flow, then constant, then free (D13)
//	Score(template, corpus)        -> Utility    compression, two uses the floor (D14)
//	Select(candidates, corpus)     -> []Accepted one at a time, rewriting between
//
// Nothing here reads a row or calls a provider; see purity_test.go. The one
// permitted model call in this epic proposes a derivation for a hole the rules
// could not explain, and integrations/procedure makes it -- what lives here is
// CheckDerivation, which decides whether the proposal holds on every recorded
// instance, and which is pure precisely so that the rejection is testable.
package procedure
```

```go
// component/procedure/params.go
package procedure

// Params are the knobs of the pipeline. They are a VALUE and not a set of
// constants because the design record's cross-cutting rule says so: the
// symbolization budget, the mining support and gap and the argument ceiling
// are rows or manifest values with the defaults named here, so an operator can
// move them without a release.
type Params struct {
	// SymbolBudget is the greatest generalization distance at which an action
	// still joins a cluster. Too loose and every action of a tool collapses
	// into one symbol, which symbolize_test.go's negative control catches.
	SymbolBudget int
	// MinSupport is how many sequences a pattern must occur in. Two is the
	// floor of D14 and the lowest value that can mean anything.
	MinSupport int
	// Gap is how many unmatched symbols may fall between two elements of an
	// occurrence.
	Gap int
	// MaxArgs is the ceiling on a lifted procedure's free parameters. A
	// template needing more is not an abstraction, it is the corpus.
	MaxArgs int
	// Noise is the fraction of behaviour the inductive miner may discard
	// before it falls back to a flower model.
	Noise float64
}

// DefaultParams are the design record's values.
func DefaultParams() Params {
	return Params{SymbolBudget: 2, MinSupport: 2, Gap: 2, MaxArgs: 8, Noise: 0.2}
}
```

- [ ] **Step 5: Run the gate green**

Run: `cd component/procedure && go test ./...`
Expected: PASS.

- [ ] **Step 6: Pay the new-module tax, all six files in one commit**

`go.work`: add `./component/procedure` in sorted position.

`go.mod` and `integrations/go.mod`: add to `require` and a `replace`:
```
	github.com/znasllc-io/memql/component/procedure v0.0.0
replace github.com/znasllc-io/memql/component/procedure => ./component/procedure     # root
replace github.com/znasllc-io/memql/component/procedure => ../component/procedure    # integrations
```

Both Dockerfiles, beside the sibling COPY lines and BEFORE `go mod download`:
```
COPY component/procedure/go.* ./component/procedure/
```

`scripts/ci/db-gated-packages.sh`: add `"component/procedure"` to `KNOWN_GO_MOD_DIRS`, with a comment block that MEASURES what the complement now means rather than asserting it:
```
# epic memql#5402 added `component/procedure`, the learning half's pure
# algorithms, for the reason component/frontdoor and component/skills exist:
# it is imported from `integrations`, and a root-module package cannot be
# imported from a nested one with GOWORK=off. What the complement now means,
# having MEASURED rather than assumed:
#
#	go list github.com/znasllc-io/memql/... | grep -cE 'component/procedure$'
#	  1
#	scripts/ci/db-gated-packages.sh --complement | grep -cE 'component/procedure$'
#	  1
#
# It is enumerated in workspace mode, so its golden tests run in THIS lane and
# it needs no lane of its own. It is NOT db-gated: every function is a decision
# over values and the module cannot reach a database by construction
# (purity_test.go), so adding it to DB_GATED_TREES would move real coverage out
# of every lane that runs without Postgres.
```

`module_taxonomy_test.go`: add `"procedure": kindComponent,` under the COMPONENTS block with the reason:
```go
	// procedure learns a template from rows the engine already wrote. It
	// talks to nobody else's system -- that is the whole test -- and an
	// operator cannot switch it off without leaving the catalog unable to
	// grow, which is engine behaviour rather than a feature.
	"procedure": kindComponent,
```

- [ ] **Step 7: Run the sweep that catches the three invisible gates**

Run:
```bash
GOWORK=off go build ./... && (cd integrations && GOWORK=off go build ./...) \
  && scripts/ci/db-gated-packages.sh --trees >/dev/null \
  && go test -count=1 -run 'TestModuleTaxonomy|TestDockerfilesCopy' .
```
Expected: all four succeed. `GOWORK=off` is the one that proves the boundary; the workspace hides it.

- [ ] **Step 8: Commit**

```bash
git add component/procedure go.work go.mod integrations/go.mod Dockerfile \
  cmd/deploy-gate-check/Dockerfile scripts/ci/db-gated-packages.sh module_taxonomy_test.go
git commit -m "Issue #5403: component/procedure as a leaf module, with the purity gate that defines it"
```

---

### Task 2: The value types and the argument tree

**Files:**
- Create: `component/procedure/value.go`, `component/procedure/value_test.go`

**Interfaces:**
- Consumes: Task 1's module.
- Produces: `Step`, `Call`, `Action`, `Node`, `NodeKind` with constants `KindLit`, `KindObject`, `KindArray`, `KindHole`; `(*Node).Equal(*Node) bool`; `(*Node).Size() int`; `(*Node).At(path []string) (*Node, bool)`. Every later task builds on these.

- [ ] **Step 1: Write the failing test**

```go
// component/procedure/value_test.go
package procedure

import "testing"

func TestNodeEqual_IsStructuralAndOrderIndependentForObjects(t *testing.T) {
	a := Obj(map[string]*Node{"b": Lit("2"), "a": Lit("1")})
	b := Obj(map[string]*Node{"a": Lit("1"), "b": Lit("2")})
	if !a.Equal(b) {
		t.Fatal("two objects with the same keys and values must be equal regardless of insertion order")
	}
}

func TestNodeEqual_ArraysAreOrderSensitive(t *testing.T) {
	a := Arr(Lit("1"), Lit("2"))
	b := Arr(Lit("2"), Lit("1"))
	if a.Equal(b) {
		t.Fatal("an array is a sequence: [1,2] must not equal [2,1]")
	}
}

func TestNodeSize_CountsEveryNodeSoCompressionHasAUnit(t *testing.T) {
	// {a: 1, b: [2, 3]} -- object + 2 keys' values + array + 2 elements = 6.
	n := Obj(map[string]*Node{"a": Lit("1"), "b": Arr(Lit("2"), Lit("3"))})
	if got := n.Size(); got != 6 {
		t.Fatalf("Size() = %d, want 6 -- the score's unit must count the whole tree", got)
	}
}

func TestNodeAt_WalksAPathAndReportsAMiss(t *testing.T) {
	n := Obj(map[string]*Node{"a": Obj(map[string]*Node{"b": Lit("x")})})
	if got, ok := n.At([]string{"a", "b"}); !ok || got.Lit != "x" {
		t.Fatalf("At(a.b) = %v, %v; want the literal x", got, ok)
	}
	if _, ok := n.At([]string{"a", "zzz"}); ok {
		t.Fatal("At must report a miss rather than an empty node -- absent and empty are different answers")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd component/procedure && go test ./... 2>&1 | head -20`
Expected: FAIL -- `undefined: Obj`, `undefined: Lit`, `undefined: Arr`.

- [ ] **Step 3: Write the implementation**

```go
// component/procedure/value.go
package procedure

import "sort"

// Step is one recorded step, the pure value the wiring maps a v1:work:step row
// onto. It is defined HERE rather than taken from the row because this module
// may not read a row: the mapping is integrations/procedure's job, and the
// boundary is what lets every algorithm below be tested on a literal.
type Step struct {
	RunId string
	Key   string
	Seq   int
	// StepType is what the step IS. For a recorded app session it is the
	// action -- exec, fs_write, fs_read, fetch, mcp, app_answer. For a
	// compiled statement it is the statement type, and `automation` is the
	// one D24's second level cares about.
	StepType string
	Call     Call
	Input    map[string]any
	// ResultDigest is the row's resultFingerprint. A digest rather than the
	// result, because the spine stays about actions.
	ResultDigest string
	// EffectDigest is a digest of the observed footprint. Two actions with the
	// same tool and arguments but different effects are not the same action.
	EffectDigest string
	// ResultValue is the trimmed result, when the row carried one. Data-flow
	// classification needs the VALUE to see that a later literal came from it;
	// where only a digest survives, the digest is compared instead.
	ResultValue any
	// Consumed is true when some later step's arguments referenced this
	// step's result. Canonicalize uses it to drop pure reads that went
	// nowhere.
	Consumed bool
	// GoalSignature is the run's key, component/work.GoalSignature. Mining
	// groups by it, so two goals never blend into one procedure.
	GoalSignature string
}

// Call is what a step invoked, by name only.
type Call struct {
	Construct string
	Name      string
}

// Action is a canonicalized step: an identity plus an argument TREE.
type Action struct {
	// Tool is the canonical identity. An app action is its StepType; a subrun
	// step is "automation:" + the construct name, so D24's two levels are one
	// vocabulary and the pipeline never asks which writer wrote a row.
	Tool         string
	Args         *Node
	ResultDigest string
	EffectDigest string
	Key          string
	Seq          int
}

// NodeKind is what a node in an argument tree is.
type NodeKind uint8

const (
	// KindLit is a scalar, held as its string spelling. One representation,
	// because 1 and "1" arriving from JSON and from argv must compare equal
	// or two recordings of the same command never generalize.
	KindLit NodeKind = iota
	KindObject
	KindArray
	// KindHole is a position where instances disagreed.
	KindHole
)

// Node is one node of an argument tree.
type Node struct {
	Kind NodeKind
	Lit  string
	// Keys are sorted for KindObject and align with Kids.
	Keys []string
	Kids []*Node
	// HoleId names the hole for KindHole; Classify fills the rest.
	HoleId string
	// HoleType is the widest type observed at this position: string, number,
	// bool, object, array or mixed.
	HoleType string
}

// Lit builds a scalar node.
func Lit(s string) *Node { return &Node{Kind: KindLit, Lit: s} }

// Arr builds an array node.
func Arr(kids ...*Node) *Node { return &Node{Kind: KindArray, Kids: kids} }

// Obj builds an object node with its keys sorted, which is what makes Equal
// order-independent and the whole tree hashable.
func Obj(m map[string]*Node) *Node {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kids := make([]*Node, len(keys))
	for i, k := range keys {
		kids[i] = m[k]
	}
	return &Node{Kind: KindObject, Keys: keys, Kids: kids}
}

// Hole builds a hole node.
func Hole(id, typ string) *Node { return &Node{Kind: KindHole, HoleId: id, HoleType: typ} }

// Equal reports structural equality. A hole equals only a hole with the same
// id: two templates that differ in which position is open are different
// templates.
func (n *Node) Equal(o *Node) bool {
	switch {
	case n == nil || o == nil:
		return n == o
	case n.Kind != o.Kind:
		return false
	}
	switch n.Kind {
	case KindLit:
		return n.Lit == o.Lit
	case KindHole:
		return n.HoleId == o.HoleId
	case KindObject:
		if len(n.Keys) != len(o.Keys) {
			return false
		}
		for i := range n.Keys {
			if n.Keys[i] != o.Keys[i] || !n.Kids[i].Equal(o.Kids[i]) {
				return false
			}
		}
		return true
	default: // KindArray
		if len(n.Kids) != len(o.Kids) {
			return false
		}
		for i := range n.Kids {
			if !n.Kids[i].Equal(o.Kids[i]) {
				return false
			}
		}
		return true
	}
}

// Size counts every node in the tree. It is the unit the compression score is
// denominated in, which is why a hole costs the same as a literal here and is
// priced differently in score.go: size measures the SHAPE, and what a hole
// costs the caller is a separate question.
func (n *Node) Size() int {
	if n == nil {
		return 0
	}
	total := 1
	for _, k := range n.Kids {
		total += k.Size()
	}
	return total
}

// At walks a path of object keys and array indices, reporting a miss rather
// than an empty node -- absent and empty are different answers, and a
// data-flow reference that silently resolved to empty would classify a hole
// that nothing explains.
func (n *Node) At(path []string) (*Node, bool) {
	cur := n
	for _, seg := range path {
		if cur == nil {
			return nil, false
		}
		found := false
		switch cur.Kind {
		case KindObject:
			for i, k := range cur.Keys {
				if k == seg {
					cur, found = cur.Kids[i], true
					break
				}
			}
		case KindArray:
			idx := 0
			for _, r := range seg {
				if r < '0' || r > '9' {
					return nil, false
				}
				idx = idx*10 + int(r-'0')
			}
			if idx < len(cur.Kids) {
				cur, found = cur.Kids[idx], true
			}
		}
		if !found {
			return nil, false
		}
	}
	return cur, cur != nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd component/procedure && go test ./...`
Expected: PASS, 4 tests.

- [ ] **Step 5: Commit**

```bash
git add component/procedure/value.go component/procedure/value_test.go
git commit -m "Issue #5403: the argument tree and the recorded-step value the pipeline reads"
```

---

> **Tasks 3 onward are written at signature-and-test fidelity.** Tasks 1 and 2
> carry full implementations because they fix the vocabulary every later task
> spells against. From here the ACCEPTANCE TEST is the contract: each task
> names its exact signatures, the test that must fail first, and the decisions
> that are not the implementer's to make. Where a test is named below, write it
> verbatim -- the epic's issues cite these by name.

---

### Task 3: Canonicalize

Issue #5403. **Files:** create `canonicalize.go`, `canonicalize_test.go`.

**Produces:** `func Canonicalize(steps []Step) []Action`.

**Decisions that are not the implementer's:**

- **One tool vocabulary for both D24 levels.** An app action's `Tool` is its
  `StepType`; a subrun step's is `"automation:" + Call.Name`. A reader asking
  what a step did must not first have to ask which writer wrote the row.
- **argv, JSON and paths are parsed into trees, not kept as strings.** An
  `exec` command is split into an argv array honouring quotes; a string that
  parses as JSON becomes its object or array; a value that looks like a path is
  split on `/` into segments. This is the whole reason two recordings of the
  same command with a different filename can generalize: unparsed, they differ
  in one opaque string and anti-unification learns nothing.
- **Scalars are held as their string spelling** (`Node.Lit`), so `1` from JSON
  and `1` from argv compare equal.
- **A pure read whose result nothing later consumed is noise and is dropped.**
  `Step.Consumed` carries that fact from the wiring, which is the only half
  that can see the whole run. Dropping is confined to `fs_read`, `fetch` and
  `mcp`; an `exec` is never dropped, because a command's effect is not
  knowable from whether its output was read.

**Tests (all four required):**

```go
func TestCanonicalize_AnAutomationSubrunIsASymbolLikeAnyAction(t *testing.T)
// stepType "automation", Call.Name "deployThing" -> Tool "automation:deployThing".

func TestCanonicalize_ArgvIsParsedIntoATreeSoOneFilenameIsOneDifference(t *testing.T)
// exec `grep -n foo a.txt` and `grep -n foo b.txt` differ in exactly ONE leaf.

func TestCanonicalize_AnUnconsumedPureReadIsDropped(t *testing.T)
// fs_read with Consumed=false is absent from the output; the exec beside it stays.

func TestCanonicalize_AnUnconsumedExecIsNotDropped(t *testing.T)
// NEGATIVE CONTROL for the rule above: dropping on Consumed alone would
// silently delete every command whose output nobody read, which is most of
// them, and the corpus would lose exactly the side effects it exists to learn.
```

**Commit:** `Issue #5403: Canonicalize -- steps as tool plus argument tree, with unconsumed reads dropped`

---

### Task 4: Anti-unification and Symbolize

Issue #5403. **Files:** create `symbolize.go`, `symbolize_test.go`.

**Produces:**
```go
type Symbol struct { Id string; Tool string; Template *Node; Members []int }
func AntiUnify(a, b *Node, next func() string) (*Node, int)   // generalization, distance
func Symbolize(actions []Action, p Params) []Symbol
```

**Decisions:**

- **Objects pair by KEY, arrays by LONGEST COMMON SUBSEQUENCE.** A key present
  on one side only is a hole. Pairing arrays positionally would make one
  inserted argument shift every later position and report a distance equal to
  the array's length.
- **Distance counts holes INTRODUCED, weighted by the size of what they
  replace**, so generalizing away a whole object costs more than one literal.
- **Clustering is greedy over same-`Tool` actions only.** Grouping by tool
  FIRST is what makes "two unrelated tools never share a symbol" true by
  construction rather than by budget.
- **The cluster template is re-anti-unified on every join**, so a cluster's
  template is the generalization of all its members, not of the first two.

**Tests:**

```go
func TestSymbolize_TwoUnrelatedToolsNeverShareASymbol(t *testing.T)
// Issue #5403 acceptance. An exec and an fs_write with IDENTICAL argument
// trees land in different clusters at any budget, including a budget of 0 and
// a budget larger than either tree.

func TestSymbolize_TwoTracesDifferingOnlyInALiteralAreOneCluster(t *testing.T)
// Issue #5403 acceptance.

func TestSymbolize_ABudgetOfZeroClustersOnlyIdenticalActions(t *testing.T)
// NEGATIVE CONTROL: proves the budget is load-bearing rather than decorative.

func TestAntiUnify_ArraysPairByLongestCommonSubsequence(t *testing.T)
// [a,b,c] and [a,x,b,c] generalize with ONE hole, not three.

func TestAntiUnify_IsCommutativeInDistance(t *testing.T)
// Distance(a,b) == Distance(b,a); a clustering that depended on read order
// would give two replicas two different symbol sets from one corpus.
```

**Commit:** `Issue #5403: Symbolize -- first-order anti-unification under a generalization budget`

---

### Task 5: Mine

Issue #5404. **Files:** create `mine.go`, `mine_test.go`.

**Produces:**
```go
type Occurrence struct { Sequence int; Positions []int }
type Pattern struct {
    Symbols     []string
    Occurrences []Occurrence
    Support     int
    Coverage    float64
    Cohesion    float64
}
func Mine(sequences [][]string, p Params) []Pattern
```

**Decisions:**

- **Closed only.** A pattern with the same support as a longer one that
  contains it is not reported; without closure the output is every prefix of
  every pattern and the ranking is meaningless.
- **Ranked by coverage and cohesion BEFORE frequency.** Coverage is the
  fraction of corpus actions the pattern accounts for (`support x length /
  total`); cohesion is `1/(1+mean gap)` over its occurrences. A frequent but
  scattered pair is a coincidence; a slightly rarer contiguous run of five is a
  procedure.
- **Remove and re-mine.** After the top pattern is taken, its occurrences are
  removed from the sequences and mining repeats until nothing clears
  `MinSupport`. This is what stops the winner's sub-patterns from filling the
  rest of the list.
- **Gap tolerance is per adjacent pair**, not per occurrence, so one long
  interruption cannot be amortized across a short one.

**Tests:**

```go
func TestMine_ACorpusWithNoRepeatsYieldsNoPattern(t *testing.T)
// Issue #5404 acceptance, and the negative control the whole file rests on.

func TestMine_AContiguousRunOutranksAMoreFrequentScatteredPair(t *testing.T)
// The ranking rule, stated as the case that would falsify it.

func TestMine_OnlyClosedPatternsAreReported(t *testing.T)
// [a b c] occurring 3 times reports [a b c] and NOT [a b] at the same support.

func TestMine_GapToleranceIsPerAdjacentPair(t *testing.T)
// With Gap=1, a b _ _ c does not match [a b c]; a b _ c does.
```

**Commit:** `Issue #5404: Mine -- closed frequent sub-sequences ranked by coverage and cohesion`

---

### Task 6: Structure

Issue #5404. **Files:** create `structure.go`, `structure_test.go`.

**Produces:**
```go
type TreeOp string
const (OpLeaf TreeOp = "leaf"; OpSeq = "seq"; OpXor = "xor"; OpAnd = "and"; OpLoop = "loop")
type ProcessTree struct { Op TreeOp; Symbol string; Children []*ProcessTree }
func Structure(sequences [][]string, p Params) *ProcessTree
```

**Decisions:**

- **The inductive miner's cut order is exclusive choice, sequence, parallel,
  loop, and it is not negotiable** -- a different order finds a different tree
  for the same log, and two replicas must agree.
- **A retry is a loop node.** This is the acceptance criterion and the reason
  the stage exists: `Mine` alone reports a retried action seven times as a
  length-seven pattern, which lifts into an automation that hard-codes seven
  attempts.
- **Fall back to a flower model** (a loop over an exclusive choice of every
  symbol) when no cut applies, rather than returning nil. Nil would make an
  un-structurable corpus indistinguishable from an empty one.

**Tests:**

```go
func TestStructure_ARetryLoopYieldsALoopNodeAndNotALengthSevenPattern(t *testing.T)
// Issue #5404 acceptance, stated as the record states it.

func TestStructure_ASequenceWithNoRepetitionHasNoLoopNode(t *testing.T)
// NEGATIVE CONTROL: a miner that returns a loop for everything passes the
// test above and is useless.

func TestStructure_AnUnstructurableLogReturnsAFlowerNotNil(t *testing.T)
```

**Commit:** `Issue #5404: Structure -- the inductive miner, so a retry is a loop and not seven steps`

---

### Task 7: Generalize and Classify

Issue #5405. **Files:** create `generalize.go`, `generalize_test.go`.

**Produces:**
```go
type HoleClass string
const (
    HoleDataFlow    HoleClass = "dataflow"
    HoleConstant    HoleClass = "constant"
    HoleFree        HoleClass = "free"
    HoleUnexplained HoleClass = "unexplained"
)
type DataFlowRef struct { StepIndex int; Path []string }
type Hole struct {
    Id        string
    StepIndex int
    Path      []string
    Type      string
    Class     HoleClass
    Ref       *DataFlowRef  // Class == HoleDataFlow
    Const     string        // Class == HoleConstant
    Evidence  int           // instances the classification held on
}
type TemplateStep struct { Tool string; Args *Node }
type Template struct { Steps []TemplateStep; Holes []Hole }
func Generalize(instances [][]Action) Template
func Classify(t Template, instances [][]Action) []Hole
```

**Decisions:**

- **D13's order is data flow, then constant, then free, and it is checked in
  that order even when two would fit.** A hole whose value is constant across
  instances AND equals an earlier step's result in every instance is a data-flow
  hole, because the equality is the explanation and the constancy is a
  coincidence of a thin corpus.
- **A classification must hold on EVERY instance.** `Evidence` records how many
  it held on, and a count below `len(instances)` may not be recorded as
  explained.
- **`HoleUnexplained` is the only thing that may reach the model** (D6), and it
  means PARTIAL evidence: at least one instance is derivable from an earlier
  result and at least one is not. A hole with no derivation evidence at all is
  free, and free is an answer -- sending it to a model would be spending
  intelligence on a value the caller is simply going to supply.

**Tests:**

```go
func TestClassify_ALiteralEqualToAnEarlierResultYieldsADataFlowHole(t *testing.T)
// Issue #5405 acceptance.

func TestClassify_AValueConstantAcrossEveryInstanceIsKeptLiteral(t *testing.T)

func TestClassify_DataFlowBeatsConstantWhenBothFit(t *testing.T)
// D13's order, as the case that would falsify it.

func TestClassify_AClassificationHoldingOnSomeInstancesIsNotRecordedAsExplained(t *testing.T)
// NEGATIVE CONTROL for Evidence: the whole over-generalization failure mode.

func TestClassify_AHoleWithNoDerivationEvidenceIsFreeAndNotUnexplained(t *testing.T)
// The gate on the model call: without this, every free parameter costs a call.
```

**Commit:** `Issue #5405: Generalize and Classify -- templates with holes in D13's order`

---

### Task 8: The derivation language and its checker

Issue #5405, the pure half of the one bounded model call. **Files:** create `derive.go`, `derive_test.go`.

**Produces:**
```go
type Derivation struct { Expr string }
func ParseDerivation(expr string) (Derivation, error)
func CheckDerivation(d Derivation, h Hole, instances [][]Action) (ok bool, heldOn int)
```

**Decisions:**

- **The expression language is closed and tiny:** `ref(<stepIndex>, <path...>)`,
  `basename(x)`, `dirname(x)`, `lower(x)`, `upper(x)`, `trim(x)`,
  `concat(x, y, ...)` and string literals. A closed grammar is what lets a
  model's proposal be CHECKED rather than trusted; an open one would be code
  execution by another name, in a module whose whole claim is purity.
- **A proposal is kept only when it holds on EVERY instance.** `heldOn` is
  returned so the wiring can log what was rejected and why, because a rejection
  nobody can read is indistinguishable from a call nobody made.
- **An unparseable proposal is a rejection, never an error that aborts the
  run.** The hole stays free and the procedure is still learned.

**Tests:**

```go
func TestCheckDerivation_AProposalHoldingOnEveryInstanceIsKept(t *testing.T)

func TestCheckDerivation_AProposalHoldingOnSomeInstancesIsRejected(t *testing.T)
// Issue #5405 acceptance, verbatim from the record's failure-mode list.

func TestCheckDerivation_AnUnparseableProposalIsARejectionAndNotAnError(t *testing.T)

func TestParseDerivation_RefusesAnythingOutsideTheClosedGrammar(t *testing.T)
// NEGATIVE CONTROL: the grammar is the safety property, so the case that
// would falsify it is a function name nobody allowed.
```

**Commit:** `Issue #5405: the derivation grammar and the check that makes a model proposal falsifiable`

---

### Task 9: Score and Select

Issue #5405. **Files:** create `score.go`, `score_test.go`.

**Produces:**
```go
type Utility struct {
    Uses, BodyCost, ArgCost, Saved, Net int
    Accepted bool
    Reason   string
}
type Candidate struct { Template Template; Occurrences []Occurrence }
type Accepted struct { Candidate Candidate; Utility Utility }
func Score(t Template, occurrences []Occurrence, corpus [][]Action, p Params) Utility
func Select(candidates []Candidate, corpus [][]Action, p Params) []Accepted
```

**Decisions:**

- **D14's arithmetic:** `Saved` is the tree size the corpus loses by replacing
  every occurrence with one call; `BodyCost` is the template's own size;
  `ArgCost` prices holes -- **a free parameter costs strictly more than a
  data-flow hole, which costs more than a constant** (3 / 1 / 0). `Net = Saved -
  BodyCost - ArgCost`. Accepted when `Net > 0 && Uses >= 2`.
- **`Reason` is filled on a refusal** -- `"one use"`, `"net <= 0"`,
  `"free parameters exceed MaxArgs"` -- because an abstraction that was
  considered and refused is half of what makes the score falsifiable, and a
  bare false says nothing.
- **`Select` takes the max-utility candidate, REWRITES the corpus, and
  repeats.** The rewrite is what makes the hierarchy emerge: the second
  abstraction is scored against a corpus that already contains the first, so it
  can reference it.
- **A tie is broken by the template's own size, smaller first, then by the
  first occurrence's position.** A tie broken by map order gives two replicas
  two different libraries from one corpus.

**Tests:**

```go
func TestScore_APatternWhoseHolesAreAllFreeScoresBelowTheFloor(t *testing.T)
// Issue #5405 acceptance.

func TestScore_AFreeParameterIsPricedAboveADataFlowHole(t *testing.T)
// D13's pricing rule, isolated: the same template scored twice, differing
// only in one hole's class.

func TestScore_OneUseIsRefusedWithTheReasonNamed(t *testing.T)
// NEGATIVE CONTROL for the two-uses floor.

func TestSelect_RewritesTheCorpusSoTheSecondAbstractionCanReferenceTheFirst(t *testing.T)
// D14's headline claim, stated as a test.

func TestSelect_IsDeterministicUnderATie(t *testing.T)
```

**Commit:** `Issue #5405: Score and Select -- compression is the score and the library grows one at a time`

---

### Task 10: The reference parity harness

Issue #5407. **Files:** create `reference/parity_test.go`, `reference/README.md`, `testdata/**`.

**Decisions:**

- **Skipped when the references are absent, and LOUD about why.** `t.Skip` with
  the exact command that would install them. A harness that fails when a
  research sidecar is missing would red the build for everyone; one that skips
  silently is a harness nobody notices has never run.
- **The references are never product** (D6). They live under `reference/`,
  nothing in the module imports them, and the purity gate of Task 1 covers the
  module, not this directory.
- **Fixtures are shared.** The same `testdata/` corpora the golden tests read
  are what the references are run against; a harness with its own fixtures
  proves the references agree with themselves.

**Tests:**

```go
func TestParity_TheReferencesAgreeWithTheGoResultsOnTheSharedFixtures(t *testing.T)
// Issue #5407 acceptance. Skips with an installation hint when absent.

func TestParity_TheHarnessFailsWhenAReferenceDisagrees(t *testing.T)
// NEGATIVE CONTROL, run against a deliberately wrong stub reference committed
// under reference/testdata/: a parity harness that cannot fail is a harness
// that proves nothing, and this is the only way to know it can.
```

**Commit:** `Issue #5407: the reference parity harness, skipped when absent and falsifiable when present`

---

### Task 11: The corpus read

Issue #5406. **Files:** create `integrations/procedure/corpus.go`, `corpus_test.go`.

**Produces:**
```go
type corpusKey struct { OwnerUserId, GoalSignature string; Level int }
func (i *Integration) loadCorpus(ctx context.Context, k corpusKey) ([][]procedure.Step, error)
```

**Decisions:**

- **Two levels, one loader** (D24). Level 1 is the app-session action rows
  (`stepType` in exec / fs_write / fs_read / fetch / mcp / app_answer); level 2
  is the subrun rows (`stepType == "automation"`). The level is a parameter,
  not two functions, because the pipeline below it is identical and a second
  copy is a copy that drifts.
- **Disliked recordings are EXCLUDED and liked ones weighted** (D23). A run
  carrying a disliked feedback observation contributes no sequence at all;
  exclusion happens in the loader, so nothing downstream can forget.
- **Reads run under the OWNER's actor**, never the sweep's cluster-owner
  variant. The composite owner tier means a cross-owner read would silently
  blend two people's procedures into one template.
- **`Consumed` is computed HERE**, not in the module: it needs the whole run,
  which is a row fact.
- **A run with no `goalSignature` is skipped, not defaulted.** Grouping
  unsignatured runs together would mine across unrelated goals, and the
  resulting procedure would be correct about nothing.

**Tests:**

```go
func TestLoadCorpus_ExcludesADislikedRecordingEntirely(t *testing.T)
func TestLoadCorpus_LevelTwoSeesAutomationSubrunsAndNotAppActions(t *testing.T)
func TestLoadCorpus_MarksAResultConsumedWhenALaterStepReferencesIt(t *testing.T)
func TestLoadCorpus_SkipsARunWithNoGoalSignatureRatherThanGroupingIt(t *testing.T)
```

**Commit:** `Issue #5406: the two-level corpus read, with disliked recordings excluded`

---

### Task 12: The lift, the persist, and the first caller of recordConstructGoalSignature

Issue #5406. **Files:** create `integrations/procedure/{learn,derive,lift,persist,plugin}.go` and tests; modify `integrations/planner/agent_loop_authoring_transcript.go`.

**Produces:**
```go
// integrations/planner -- the existing renderer, exported and extended.
type ToolCalleeFunc func(toolName string) (kind, target string, ok bool)
func RenderTemplateAutomation(name, goal string, t procedure.Template, callee ToolCalleeFunc) (source string, everyStepWritten bool)

// integrations/procedure
func (i *Integration) learnFromRun(ctx context.Context, runId string) (constructId string, err error)
func (i *Integration) mineCorpus(ctx context.Context, ownerUserId, goalSignature string, level int) (constructId string, err error)
```

**Decisions:**

- **The renderer is EXTENDED, not replaced.** A template with no holes must
  render byte-identically to what `renderTranscriptAutomation` renders today,
  and a test asserts exactly that -- otherwise the one-off capture path quietly
  changes shape on a refactor nobody reviewed as such.
- **Free parameters become the automation's `args` block; data-flow holes
  become step references** (`call3.result.path`); constants stay literal. That
  mapping IS the lift, and it is why classification had to come first.
- **`goalSignature` is written LAST**, after the compile gate passes. It is the
  key compile's exact-match tier serves a later goal from without a model; a
  signature written before the construct is known to compile points a future
  goal at a template that cannot run. This is `recordConstructGoalSignature`'s
  first production caller (issue #5406 acceptance).
- **The D9 provenance stamp is rendered INTO the source** as a leading comment
  naming app, model, effort and session id, plus the run ids the procedure was
  learned from. The record says a lifted construct's source READS its
  provenance; a field nobody renders is provenance the OS cannot show.
- **Nothing auto-activates.** The bundle is persisted `validated`, the
  construct `draft`, and a test asserts no path sets `active`.
- **The model call is made here and nowhere else**, at level `reasoning`, only
  for a hole `Classify` returned as `HoleUnexplained`, and its answer passes
  through `procedure.CheckDerivation` before it is kept.

**Tests:**

```go
func TestRenderTemplateAutomation_AHolelessTemplateRendersIdenticallyToTheTranscriptPath(t *testing.T)
func TestRenderTemplateAutomation_FreeParametersBecomeArgsAndDataFlowHolesBecomeStepReferences(t *testing.T)
func TestLearn_TwoRunsSharingASequenceOfThreeAutomationsYieldOneHigherAutomation(t *testing.T)
// Issue #5406 acceptance, and D24's headline.
func TestLearn_ALiftedConstructIsValidatedAndNeverActive(t *testing.T)
// Issue #5406 acceptance.
func TestLearn_GoalSignatureIsWrittenOnlyAfterTheCompileGatePasses(t *testing.T)
func TestLearn_ACorpusOfOneSessionYieldsNoProcedure(t *testing.T)
// NEGATIVE CONTROL: the two-uses floor, end to end through the wiring.
func TestDerive_TheModelIsAskedOnlyForAnUnexplainedHole(t *testing.T)
// NEGATIVE CONTROL for D6's "induction spends no model": a corpus whose holes
// all classify must make ZERO provider calls, asserted against a recording
// provider that fails the test if it is called at all.
```

**Commit:** `Issue #5406: integrations/procedure -- the lift, the provenance stamp, and goalSignature's first caller`

---

### Task 13: The DSL domain and the triggers

Issue #5406. **Files:** create `dsl/procedure/{memql.toml,builtins.memql,automations.memql,prompts.memql,prompts/deriveHole.tmpl}`; modify `embed_inventory_test.go`.

**Decisions:**

- **Two triggers, both authored, neither auto-activating.** A `node.updated` on
  `v1:work:run` reaching `succeeded` for a session subrun, and a schedule over
  the corpus per goal signature at both levels. The schedule is
  `@trigger(schedule="0 0 */6 * * *")` -- the one spelling; `@schedule` is
  refused at parse.
- **The builtins are `procedureLearnFromRun` and `procedureMineCorpus`**,
  reached through `@executor("integration.procedure.learnFromRun")` and
  `...mineCorpus`. Both are `@serverOnly`: a caller who could drive learning on
  an arbitrary run could mint a catalogued template pointing at somebody else's
  work.
- **The prompt carries `@level("reasoning")`** -- required on every prompt --
  and its output schema is a derivation expression string and nothing else. A
  free-form answer is one `CheckDerivation` would reject anyway, so the schema
  is where the refusal belongs.
- **`embed_inventory_test.go` is MEASURED, not derived** -- run the inventory
  and paste what it reports, per the gate's own instruction.

**Tests:** `go test -count=1 .` (root gates: embed inventory, DSL conformance,
retired forms, relative links) and `MEMQL_REQUIRE_DB=1 make test`.

**Commit:** `Issue #5406: the procedure DSL domain -- two triggers, two server-only builtins, one reasoning prompt`

---

### Task 14: The repo sweep and the epic's own cleanup

All issues. **Files:** modify `CLAUDE.md` (the Project Structure tree and the extension list), delete `docs/superpowers/plans/2026-09-22-procedure-learning.md`.

**Decisions:**

- **The plan is deleted in the epic's merge**, which every issue says. It is
  scaffolding, and a plan left behind reads as a design record that nothing
  keeps true.
- **CLAUDE.md gains one line, not a section.** The root file is at 140,548 of a
  150,000-character budget and per-epic write-ups belong in
  `docs/internal/design/feature-notes.md`.

- [ ] **Step 1: The four-command sweep for the new module**

```bash
GOWORK=off go build ./... && (cd integrations && GOWORK=off go build ./...) \
  && scripts/ci/db-gated-packages.sh --trees >/dev/null \
  && gitleaks dir . --no-banner
```

- [ ] **Step 2: The gates that read the repo as files**

```bash
go test -count=1 .
```

- [ ] **Step 3: The engine**

```bash
MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=postgres://memql:memql_dev@localhost:15434/memql \
  go test -count=1 github.com/znasllc-io/memql/...
```

- [ ] **Step 4: Delete the plan, commit, open ONE PR for #5403-#5407**

---

## Self-Review

**Spec coverage.** Epic C's five numbered functions map to Tasks 3-9;
the wiring paragraph to Tasks 11-13; the failure-modes paragraph to the
negative controls named in Tasks 4, 5, 6, 9 and 12; the tests paragraph to
Tasks 3-10 and the parity harness in Task 10. D6 is Task 1's gate plus Task
12's zero-provider-call control; D13 is Task 7; D14 is Task 9; D24's two levels
are Task 11.

**Gaps closed during review.** The record's "two runs sharing a sequence of
three automations yield one higher automation" is a WIRING claim, not a module
one -- it moved from Task 9 to Task 12, where the corpus is real. The record
says a one-session corpus "is still lifted as a one-off validated bundle, as
today"; Task 12's `TestLearn_ACorpusOfOneSessionYieldsNoProcedure` asserts the
procedure half and must not assert that the existing capture path stops
working.

**Type consistency.** `Hole` is defined once (Task 7) and used by Tasks 8, 9
and 12. `Occurrence` is defined in Task 5 and consumed by Task 9's `Candidate`.
`Params` is Task 1's and threaded through 4, 5, 6 and 9. `Template` is Task 7's
and is what Task 12 renders.
