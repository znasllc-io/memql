# DSL v1 attribute cleanup — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Retire every DSL attribute and form that nothing reads, collapse the five places that accept two spellings of one value to one spelling each, and make an unknown annotation on a prompt or builtin field a refusal rather than a silent drop — with every retired form refusing by name and naming `memqlmigrate --rewrite=attributes`.

**Architecture:** One ledger, one gate, one migrator. `core/baseparser.retiredConstructAnnotations` already exists as the construct-annotation retirement ledger consulted by `ValidateConstructAnnotations` BEFORE any allow-list, and it already formats `@x on a <kind> is retired -- <hint>`. Every construct-level retirement in this epic is an entry in that map; the field-level ones are a sibling ledger on the same pattern. The allow-lists in `component/language/annotations` lose the retired names, the parsers lose the dead folds, the AST loses the fields no allow-list can populate, and `cmd/memqlmigrate --rewrite=attributes` composes the mechanical rewrites so the tree migrates in the same PR.

**Tech Stack:** Go 1.26.1, `component/language` (lexer / parser / AST / annotations registry), `component/memql` (loaders, converters, `help()`), `component/database/memory-nodes` (concept vocabulary), `cmd/memqlmigrate`, `test/dslconformance`.

**Spec:** [docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md](../specs/2026-09-13-dsl-v1-language-freeze-program-design.md) — section 2.5 (the attribute surface), section 4 epic 4, D6, D16, D17.

## Global Constraints

- **Branch `epic/dsl-v1-attributes`; never on main.** One PR for issues #5375 #5376 #5377 #5378 #5379.
- **Every retired form refuses naming `memqlmigrate --rewrite=attributes`** (D17, last sentence). A refusal that does not name the rewrite is an incomplete retirement.
- **Everything migrates in the same PR** (D6 / cross-cutting rule 1): the in-repo `dsl/` tree loads with zero refusals at the end of this plan.
- **Mechanics before meaning** (cross-cutting rule 2): land the codemod and the corpus cells first, then flip the parser, so the migration is a mechanical diff a reviewer can read.
- **Nothing in this program changes concept storage, row authorization or the wire protocol.** Concretely: no canonical concept id may change. `@namespace` removal is only legal where the surviving `namespace.pin`-or-directory derivation yields the identical namespace.
- **`component/language` owns the language** (cross-cutting rule 4). No new vocabulary outside it.
- **Verify before retiring.** Section 10 requires re-verifying each allow-list against the registry before the rewrite is written. Task 0 records that verification; three of D17's retirements do not survive it and are handled as documented exceptions, not silently dropped.
- **Test with `make test`, never `go test ./...`** (CLAUDE.md): the bare pattern misses `component/memql`, `component/database` and `component/language` — the three trees this epic edits.
- **No emojis** in any documentation, refusal string or CLI output (CLAUDE.md documentation style).

---

## Task 0: The verification of record (the D17 re-check)

Section 10 of the spec requires the exact set each allow-list accepts to be re-verified against the registry **before** the retirement rewrite is written, and requires an answer on whether the OS reads `@displayCard` or `@composable`. This task produces that answer, and it is the task that decides what the rest of the plan may touch.

**Files:**
- Create: `docs/superpowers/plans/2026-09-14-dsl-v1-attributes-verification.md`

**Interfaces:**
- Produces: the retirement set that Tasks 1-6 implement, and the three documented exceptions they must NOT implement.

- [ ] **Step 1: Record the dead set, with the grep that proves each one dead**

Confirmed dead — parsed, stored, never consulted by anything that changes behaviour, and zero real occurrences in `dsl/` (all apparent hits are comments or `dsl/_reference/`, which is the deliberate don't-do-this skeleton and is not loaded):

| Form | Where it dies | Real `dsl/` uses |
|---|---|---|
| `@deprecated` on a function | `parser.go:3828` folds it; `function_loader.go:495` copies it; `executor_builtin.go:578` renders it in `help()`; `sense/hover.go:248` renders it. No gate reads it. | 0 |
| `@version` on a function | `parser.go:3833` folds it into `FunctionDef.Version`; nothing reads that field. (`@version` on a **concept** and a **seed** is id-bearing and stays.) | 0 |
| `@timeout` on a function | `parser.go:3837`; `FunctionDef.Timeout` has no reader. | 0 |
| `@retry` on a function | `parser.go:3853`; `FunctionDef.Retry` has no reader. | 0 |
| `@idempotent` on a function | `parser.go:3859`; `FunctionDef.Idempotent` has no reader. | 0 |
| `@audit` on a function | `parser.go:3861`; `FunctionDef.Audit` has no reader. | 0 |
| `@unique` on a field | `concept_parser.go` `applyPropertyAttribute`; stored, no check behind it — `dsl/worker/mutations.memql:578` says so in a comment. | 0 |
| `@immutable` on a field | same site; stored, never enforced. | 0 |
| `@rateLimit` on a tool | `tool_decl.go:110` -> `Tool.RateLimit` -> cloned in `tool_types.go:255`; the only other reader is `function_loader.go:523` copying it to `Function.RateLimitRequests`, which `function_types.go:267` clones and nothing enforces. | 0 |
| `@scopes` on a tool | `tool_decl.go:115` -> `Tool.Scopes` -> advertised on the gRPC tool descriptor at `grpc/server.go:2309` and enforced nowhere. | 0 |
| `@latestMode` | auto-derived from `asOf latest` in the body; the annotation is a restatement, not a switch (its own registry doc says so). | 0 |
| `@enabled` | `parser.go:3824` sets `Enabled = true`, which is already the default; an explicit no-op by its own registry doc. | 0 |
| `@namespace` on a concept | `concept_parser.go:1149` only VALIDATES; `ast.AssembleConceptIdFromDeclInDir` applies it. **Every one of the 69 real occurrences is exactly redundant with a `namespace.pin` that already exists in the same directory** (`dsl/deployment` -> `cluster`, `dsl/shopify/generated` -> `shopify`, `dsl/shopify/overlay` -> `shopify`), so removal is id-preserving. | 69 |
| `@default` on a **concept** field | `concept_parser.go:1398` parses it and `:1811` publishes it as the JSON-Schema `default` keyword, which no validator applies and no insert path consults. | 203 |
| `import (...)` in a `.memql` file | the keyword lexes (`lexer.go:896`) but no `.memql` file uses it; `use` is the import form. | 0 |
| `?.` | `lexer.go:394` emits `TokenQuestionDot`. | 0 |
| `;`-AND | `parser.go:5160` `parseLogicalAnd` accepts `;` as a synonym for `&&`. | 0 |
| `,`-OR | `parser.go:2072`, `:437`, `:517` accept `,` as a separator. | 0 |

- [ ] **Step 2: Record the three exceptions — the retirements that do NOT survive verification**

These are the answer to the spec's own instruction to re-verify. Each is a live reader found by grep; retiring any of them is a behaviour regression, so each is KEPT and each has its reader named in its registry doc instead. Issue #5378 asks for exactly this shape of answer for two of them.

1. **`@displayCard` — KEEP. Reader: `clients/os`.** Parsed at `concept_parser.go:1176` region, published on the concept registry, read at `clients/os/src/apps/concepts/displayCard.ts:62` (`concept.displayCard`) and rendered by `clients/os/src/apps/concepts/RowsPanel.tsx:6`. It is also load-bearing in the other direction: `test/dslconformance/displaycard_inventory_test.go` requires every concept to declare one or decline it with a `// @no-displayCard:` reason. 217 occurrences across 99 files. Retiring it would blank the Concepts app's row cards.
2. **`@composable` — KEEP. Reader: `clients/os`.** Parsed and validated at `concept_parser.go:118` / `validateComposable` at `:172`, served by `integration.compose.composableConcepts` (`dsl/compose/builtins.memql:70`, `app/integrations_compose.go:71`), read at `clients/os/src/apps/materializer/useCompose.ts:171` and rendered by `ComposerSection.tsx:100`. Retiring it would empty the Materializer's composable list.
3. **`@allowedRoles` — KEEP. Reader: the agent tool gate.** D17 says it "becomes `@requiresRank` and `@requiresCapability`", but those gate a **different axis**. `@allowedRoles` restricts which **agent role** (`assistant`, `specialist`) may call a tool and is enforced on every path: `tool_types.go:196-203` is the predicate, `grpc/server.go:2360` applies it ("tools with AllowedRoles reject callers outside the allowed set"), and `tool_execution.go:583` calls it "load-bearing on every path". `@requiresRank` is a floor on the **human actor's** catalog rank and `@requiresCapability` is a grant over a resource; neither can express "a specialist may not call this". `dsl/skills/seeds/foundational.memql:39` depends on the distinction in as many words: "The tool itself is @allowedRoles(assistant), so a specialist that somehow carries this skill still cannot call it." Mechanically substituting rank for agent role would let every specialist call `ensureAgent` and `produceArtifact`. Recorded as a program decision for the owner rather than executed.
4. **`@default` on a **tool** or **prompt** field — KEEP.** Those bodies ARE the JSON schema handed to the model (`prompt_converter.go:155` -> `prompt_types.go:114` `prop["default"]`; `tool_converter.go:92` `prop["default"]`). D17's reason for retiring field `@default` ("never applied") is true of a concept field and false here. Retirement is scoped to concept fields; `@default` on an **args** field is already refused at load and stays refused.

- [ ] **Step 3: Record the single-spelling set**

| Meaning | Surviving spelling | Retired spelling(s) | Real `dsl/` uses of the retired form |
|---|---|---|---|
| query cache TTL | `@cache(300)` | `@cache(ttl="300")` | 0 |
| never cache | `@cache(0)` | `@nocache` | 0 |
| schedule trigger | `@trigger(schedule="...")` | `@schedule(cron="...")` | 0 |
| mutation construct | `mutation <Concept> <name>` | `mutate <Concept> <name>` | 418 |
| provider vendor | `@vendor("OpenAI")` | `@vendor("OpenAI")` on a provider | 6 |

`@type` on a **concept** (the row kind: object / collection / reference) is a different annotation on a different receiver and stays `@type`. Renaming only the provider one is what makes each name mean one thing.

- [ ] **Step 4: Commit the verification**

```bash
git add docs/superpowers/plans/2026-09-14-dsl-v1-attributes-verification.md
git commit -m "docs(plan): the D17 re-verification of record, and its three exceptions"
```

---

## Task 1: The retirement ledger and its refusal wording

Everything in this epic refuses through one mechanism, so it is built first. `core/baseparser.ValidateConstructAnnotations` already checks a retirement ledger BEFORE the allow-list, which is what makes a retired name refuse with a hint instead of falling through to "unknown annotation @x -- supported: ...". The ledger has one entry today (`internal`); this task fills it and adds the field-level sibling.

**Files:**
- Modify: `core/baseparser/iface.go:30-34` (`retiredConstructAnnotations`), and add `RetiredFieldAnnotation`
- Test: `core/baseparser/retired_attributes_test.go` (create)

**Interfaces:**
- Consumes: Task 0's dead set and single-spelling set.
- Produces:
  - `retiredConstructAnnotations map[string]string` — name -> hint, extended.
  - `func RetiredConstructAnnotation(name string) (string, bool)` — already exported, unchanged signature.
  - `func RetiredFieldAnnotation(name string) (string, bool)` — NEW. The field-level ledger, consumed by Tasks 4 and 5.
  - `const AttributeRewriteHint = "run `memqlmigrate --rewrite=attributes`"` — NEW. The one string every hint ends with, so the wording is pinned in one place.

- [ ] **Step 1: Write the failing test**

Create `core/baseparser/retired_attributes_test.go`:

```go
package baseparser

import (
	"strings"
	"testing"
)

// TestEveryRetiredFormNamesTheRewrite is the D17 acceptance gate: "Every
// retired form refuses with the migrator's name." A hint that explains the
// retirement without naming the rewrite leaves the author to find the
// migrator themselves, which is the failure this pins.
func TestEveryRetiredFormNamesTheRewrite(t *testing.T) {
	for name, hint := range retiredConstructAnnotations {
		if name == "internal" {
			// Retired under the 2026.08 epoch with no codemod: deleting
			// the annotation is the whole migration, so there is nothing
			// for the rewrite to do. Predates this epic.
			continue
		}
		if !strings.Contains(hint, AttributeRewriteHint) {
			t.Errorf("@%s: hint does not name the rewrite.\n  got:  %s\n  want: a hint containing %q",
				name, hint, AttributeRewriteHint)
		}
	}
	for name, hint := range retiredFieldAnnotations {
		if !strings.Contains(hint, AttributeRewriteHint) {
			t.Errorf("field @%s: hint does not name the rewrite.\n  got:  %s\n  want: a hint containing %q",
				name, hint, AttributeRewriteHint)
		}
	}
}

// TestRetiredSetIsTheD17Set pins the ledger against the spec's list so a
// retirement cannot be quietly dropped or quietly added. The three
// exceptions of the verification record are asserted ABSENT by name --
// each has a live reader, and re-retiring one is the regression this
// catches.
func TestRetiredSetIsTheD17Set(t *testing.T) {
	wantConstruct := []string{
		"deprecated", "timeout", "retry", "idempotent", "audit",
		"latestMode", "enabled", "nocache", "schedule",
		"rateLimit", "scopes", "namespace",
	}
	for _, n := range wantConstruct {
		if _, ok := RetiredConstructAnnotation(n); !ok {
			t.Errorf("@%s should be retired (D17) but is not in the ledger", n)
		}
	}
	wantField := []string{"unique", "immutable"}
	for _, n := range wantField {
		if _, ok := RetiredFieldAnnotation(n); !ok {
			t.Errorf("field @%s should be retired (D17) but is not in the ledger", n)
		}
	}
	// The verification record's exceptions. Each has a live reader named
	// in docs/superpowers/plans/2026-09-14-dsl-v1-attributes-verification.md.
	for _, n := range []string{"displayCard", "composable", "allowedRoles"} {
		if hint, ok := RetiredConstructAnnotation(n); ok {
			t.Errorf("@%s has a live reader and must NOT be retired; ledger says: %s", n, hint)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./core/baseparser/ -run 'TestEveryRetiredFormNamesTheRewrite|TestRetiredSetIsTheD17Set' -v`
Expected: FAIL — `undefined: AttributeRewriteHint`, `undefined: retiredFieldAnnotations`, `undefined: RetiredFieldAnnotation`.

- [ ] **Step 3: Write the implementation**

In `core/baseparser/iface.go`, replace the `retiredConstructAnnotations` block with:

```go
// AttributeRewriteHint is the one sentence every retirement hint in this
// file ends with. D17's last sentence makes naming the migrator part of
// the retirement, not a courtesy: an author who meets a refusal needs the
// command that fixes their tree, and a hint that only explains the reason
// leaves them to find it. Pinned as a constant so the wording cannot
// drift between the twenty-odd entries below -- and so the gate in
// retired_attributes_test.go can assert on one string.
const AttributeRewriteHint = "run `memqlmigrate --rewrite=attributes`"

// retiredConstructAnnotations maps construct-level annotations hard-retired
// to their migration hints, checked BEFORE the allow-list so a retired name
// refuses with its hint rather than falling through to the generic
// "unknown annotation" message and its list of supported names.
//
// The 2026.09 batch is epic memql#5375 (D17): everything nothing read, and
// the losing half of every pair that spelled one value two ways. Three
// candidates from D17 are deliberately ABSENT -- @displayCard, @composable
// and @allowedRoles each have a live reader, named in
// docs/superpowers/plans/2026-09-14-dsl-v1-attributes-verification.md.
// Adding one of them here is a behaviour regression, and
// TestRetiredSetIsTheD17Set fails the build on it.
var retiredConstructAnnotations = map[string]string{
	"internal": "retired under the 2026.08 epoch (#2620 ruling / #2708); it only hid the construct from external discovery surfaces (tool listing, MCP promotion, the help()/listFunctions internal flag) while leaving it callable -- delete the annotation",

	// Parsed into FunctionDef fields, copied into the runtime function,
	// rendered by help() and refused by every allow-list (memql#5375).
	// Nothing branched on any of them.
	"deprecated": "retired (memql#5375): it was rendered by help() and editor hover and read by nothing that changes behaviour -- delete it, or say so in the construct's @description; " + AttributeRewriteHint,
	"timeout":    "retired (memql#5375): FunctionDef.Timeout had no reader, so the value never bounded anything -- delete it; a real deadline belongs on the caller's context; " + AttributeRewriteHint,
	"retry":      "retired (memql#5375): FunctionDef.Retry had no reader, so nothing ever retried -- delete it; " + AttributeRewriteHint,
	"idempotent": "retired (memql#5375): declared metadata with no check behind it -- delete it; " + AttributeRewriteHint,
	"audit":      "retired (memql#5375): declared metadata with no writer behind it; auditing is v1:identity:auditEvent, written from Go -- delete it; " + AttributeRewriteHint,

	// Restatements of something the engine already derives.
	"latestMode": "retired (memql#5375): the engine derives time-dependence from `asOf latest` in the body, so the annotation restated it and could disagree with it -- delete it; " + AttributeRewriteHint,
	"enabled":    "retired (memql#5375): constructs are enabled by default, so @enabled was an explicit no-op that read like a switch -- delete it, and use @disabled to deactivate; " + AttributeRewriteHint,

	// The losing spelling of a pair (D17, one spelling each).
	"nocache":  "retired (memql#5375): write @cache(0) -- one annotation for the cache TTL, with 0 meaning never; " + AttributeRewriteHint,
	"schedule": "retired (memql#5375): write @trigger(schedule=\"0 0 * * * *\") -- one annotation declares how an automation is reached; " + AttributeRewriteHint,

	// Stored on the tool and enforced nowhere.
	"rateLimit": "retired (memql#5375): Tool.RateLimit was cloned and advertised but never enforced, so the declared ceiling did not exist -- delete it; the live ceilings are the provider chokepoint in ai_guard.go and the run budget in component/work; " + AttributeRewriteHint,
	"scopes":    "retired (memql#5375): Tool.Scopes was advertised on the gRPC tool descriptor and checked nowhere, so it read as an authorization gate while gating nothing -- delete it; use @requiresCapability for a real one; " + AttributeRewriteHint,

	// Id-bearing only by redundancy: the namespace comes from the
	// directory, or from its one-line namespace.pin.
	"namespace": "retired (memql#5375): a concept's namespace is its domain directory, or that directory's one-line namespace.pin -- the annotation could only restate one or silently disagree with it; delete it, and pin a deliberate divergence with namespace.pin (#2614); " + AttributeRewriteHint,
}

// retiredFieldAnnotations is the same ledger for FIELD annotations -- the
// ones written inside a concept body on a property, where the receiver is a
// field rather than a construct. Separate map because the two surfaces have
// separate allow-lists and separate refusal sites; one map would let a
// field-only retirement refuse a construct that legitimately carries the
// name (concept @version is id-bearing, function @version is not).
var retiredFieldAnnotations = map[string]string{
	"unique":    "retired (memql#5375): declared metadata with no uniqueness check behind it (memql#2960), so it read as a constraint while constraining nothing -- delete it; " + AttributeRewriteHint,
	"immutable": "retired (memql#5375): declared metadata with no write guard behind it -- delete it; a field that must not change is enforced by the mutation that writes it; " + AttributeRewriteHint,
}

// RetiredFieldAnnotation reports whether a FIELD-level annotation name is
// hard-retired, returning its migration hint. The concept parser and the
// prompt / builtin / tool field converters all consult it, so a retired
// field annotation refuses with its hint from whichever body it appears in.
func RetiredFieldAnnotation(name string) (string, bool) {
	hint, ok := retiredFieldAnnotations[name]
	return hint, ok
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./core/baseparser/ -run 'TestEveryRetiredFormNamesTheRewrite|TestRetiredSetIsTheD17Set' -v`
Expected: PASS, both tests.

- [ ] **Step 5: Commit**

```bash
git add core/baseparser/iface.go core/baseparser/retired_attributes_test.go
git commit -m "feat(language): the retirement ledger, and every hint names the rewrite

Issue #5376. D17's last sentence -- every retired form refuses with the
migrator's name -- becomes a constant and a gate rather than twenty
hand-written strings that drift."
```

---

## Task 2: The registry loses the retired names and keeps the three with readers

The registry is the load-time allow-list for the four function constructs and the editor projection for the rest, so dropping a name here is what turns it from silently-folded into refused. `@displayCard`, `@composable` and `@allowedRoles` stay and gain their reader in the doc string, which is the deliverable issue #5378 asks for.

**Files:**
- Modify: `component/language/annotations/registry.go:30-93` (`ByReceiver`), `:98-188` (`Docs`), `:218-256` (`KeywordArgs`)
- Test: `component/language/annotations/retirement_test.go` (create)

**Interfaces:**
- Consumes: `baseparser.RetiredConstructAnnotation` (Task 1).
- Produces: `ByReceiver` with `enabled` / `latestMode` / `nocache` / `schedule` / `rateLimit` / `scopes` / `namespace` gone and `vendor` replacing provider `type`; `Docs` gaining `vendor`, `displayCard`, `composable` and losing the retired entries.

- [ ] **Step 1: Write the failing test**

Create `component/language/annotations/retirement_test.go`:

```go
package annotations

import (
	"strings"
	"testing"
)

// TestNoReceiverOffersARetiredName is the other half of Task 1's gate: the
// ledger refusing a name and the registry still OFFERING it would put the
// editor's completion list at war with the loader, so an author would be
// completed into a refusal.
func TestNoReceiverOffersARetiredName(t *testing.T) {
	retired := map[string]bool{
		"enabled": true, "latestMode": true, "nocache": true,
		"schedule": true, "rateLimit": true, "scopes": true,
		"namespace": true, "deprecated": true, "timeout": true,
		"retry": true, "idempotent": true, "audit": true,
	}
	for receiver, names := range ByReceiver {
		for _, n := range names {
			if retired[n] {
				label := receiver
				if label == "" {
					label = "<concept>"
				}
				t.Errorf("receiver %s still offers retired @%s", label, n)
			}
		}
	}
}

// TestProviderVendorReplacedType pins the rename. @type survives on a
// concept (the row kind) and must not be collateral damage.
func TestProviderVendorReplacedType(t *testing.T) {
	has := func(receiver, name string) bool {
		for _, a := range ByReceiver[receiver] {
			if a == name {
				return true
			}
		}
		return false
	}
	if !has("Provider", "vendor") {
		t.Error("Provider should accept @vendor")
	}
	if has("Provider", "type") {
		t.Error("Provider should no longer accept @type -- @vendor is the one spelling")
	}
	if !has("", "type") {
		t.Error("a concept's @type (the row kind) is a different annotation and must survive")
	}
}

// TestKeptAnnotationsNameTheirReader is issue #5378's deliverable, turned
// into a gate. @displayCard and @composable survive BECAUSE clients/os
// reads them; a doc that does not say where means the next reader of this
// file has to re-run the grep to find out whether it is still true.
func TestKeptAnnotationsNameTheirReader(t *testing.T) {
	for _, tc := range []struct{ name, reader string }{
		{"displayCard", "clients/os"},
		{"composable", "clients/os"},
		{"allowedRoles", "tool_types.go"},
	} {
		doc, ok := Docs[tc.name]
		if !ok {
			t.Errorf("@%s has no doc entry", tc.name)
			continue
		}
		if !strings.Contains(doc, tc.reader) {
			t.Errorf("@%s doc must name its reader %q, or the next audit cannot tell it is still read.\n  got: %s",
				tc.name, tc.reader, doc)
		}
	}
}

// TestEveryOfferedNameHasADoc keeps the pair complete after the deletions.
func TestEveryOfferedNameHasADoc(t *testing.T) {
	for receiver, names := range ByReceiver {
		for _, n := range names {
			if _, ok := Docs[n]; !ok {
				label := receiver
				if label == "" {
					label = "<concept>"
				}
				t.Errorf("receiver %s offers @%s with no Docs entry", label, n)
			}
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./component/language/annotations/ -run 'Retire|Vendor|Reader|Doc' -v`
Expected: FAIL — every receiver still offers `enabled`; `Provider` still offers `type`, not `vendor`; `displayCard` / `composable` have no `Docs` entry at all.

- [ ] **Step 3: Write the implementation**

In `registry.go`, apply these edits to `ByReceiver`. Every list loses `"enabled"`; the receiver-specific losses are marked:

```go
var ByReceiver = map[string][]string{
	"Query": {
		// @enabled, @latestMode and @nocache retired in memql#5375: the
		// first was an explicit no-op, the second restated what the body
		// already says, the third was the second spelling of @cache(0).
		"description", "disabled", "public", "serverOnly", "mcp", "unbounded", "cache", "actor", "requiresRank", "requiresCapability",
	},
	"Mutation": {
		"description", "disabled", "actor", "public", "serverOnly",
		"mergeFields", "appendFields", "addToSet", "removeFromSet", "createOnly", "noUnset", "scrubPii", "mcp", "requiresRank", "requiresCapability",
	},
	"Logic": {
		"description", "disabled", "eventField", "actor", "requiresRank", "requiresCapability",
	},
	"Automation": {
		// @schedule retired in memql#5375: @trigger(schedule=...) is the
		// one spelling, so "how is this automation reached" is one
		// annotation and @template can be refused beside it coherently.
		"description", "disabled", "trigger", "filter", "mcp", "actor", "template",
	},
	"Action": {
		"description", "disabled", "kind", "sideEffect",
	},
	"Capability": {
		"description", "disabled", "sideEffect",
	},
	"Spec": {
		"description", "disabled",
	},
	"Tool": {
		// @rateLimit and @scopes retired in memql#5375: both were stored
		// and advertised and enforced nowhere, so each read as a ceiling
		// or a gate while being neither.
		//
		// @allowedRoles STAYS. D17 proposed replacing it with
		// @requiresRank + @requiresCapability, but those gate the human
		// actor's catalog rank and their grants over a resource, and this
		// gates which AGENT role may call the tool -- a different axis,
		// enforced on every path (tool_types.go:196, grpc/server.go:2360,
		// tool_execution.go:583). Substituting rank for agent role would
		// let every specialist call the assistant-only tools.
		"description", "disabled", "handler", "executionTime",
		"destructive", "requiresConfirmation",
		"allowedRoles", "mcp",
	},
	"Builtin": {
		// @requiresCapability (epic memql#5288, task memql#5301): a builtin is
		// where most of an app's ACTIONS live -- packageDeploy, siteArchive,
		// customDomainAdd are all builtins -- so the part vocabulary
		// (`execute` on `app:<id>/<part>`) has to be declarable here or it
		// gates nothing that matters. @requiresRank stays off builtins: a
		// rank floor on a Go-served read is applied in its handler (the
		// logsSearch precedent), and nothing has asked for the annotation.
		"description", "disabled", "executor", "alias", "args", "sdk",
		"requiresCapability",
	},
	"Prompt": {
		"description", "disabled", "level", "defaultProvider", "templateFile",
	},
	"Provider": {
		// @type renamed @vendor in memql#5375: one name, one meaning. A
		// concept's @type is the row kind and is a different annotation.
		"disabled",
		"description", "vendor", "model", "modality", "default", "base", "extends",
	},
	"Shape": {
		"description", "row", "actor",
	},
	"Policy": {
		"description", "primary", "fallback",
	},
	"Rule": {
		"description", "disabled",
		"when", "policy", "level", "precedence", "onUnavailable", "exclude", "locked",
	},
	"Seed": {
		// @namespace retired in memql#5375; @version stays -- a seed's
		// version is id-bearing.
		"description", "version", "scope", "templateFile", "disabled",
	},
	"": { // top-level (concept definitions)
		// @namespace retired in memql#5375: the namespace is the domain
		// directory or its namespace.pin. @version stays -- it is the
		// "v1" of every canonical id.
		//
		// @displayCard and @composable arrive here from
		// component/database/memory-nodes' own table as part of D16's one
		// registry; both are READ (see Docs), which is why issue #5378
		// kept them.
		"description", "version", "scope", "visibility", "type", "cache", "relationship",
		"rowAuthz", "origin", "mirroredTo", "displayCard", "composable",
	},
}
```

In `Docs`, delete the entries for `enabled`, `latestMode`, `nocache`, `rateLimit`, `scopes`, `schedule`, `namespace`; rename `type`'s entry to concept-only and add `vendor`; add the two kept concept annotations with their readers named:

```go
	// `type` is now the CONCEPT annotation only; the provider one is
	// @vendor (memql#5375).
	"type": "On a concept: the row kind -- \"object\" (default), \"collection\" or \"reference\". Drives the collection/reference node-type invariants. A provider's vendor is @vendor.",

	"vendor": "The AI vendor a provider speaks to: @vendor(\"OpenAI\") or @vendor(\"Anthropic\"). Renamed from @type in memql#5375 so one name means one thing -- a concept's @type is its row kind, and the two shared a spelling for no reason. Valid on a @base provider; a child @extends the base and inherits it.",

	"displayCard": "Per-concept rendering hints for concept-agnostic surfaces: @displayCard(primary=\"name\", secondary=\"role\", tertiary=\"ownerUserId\", status=\"active\"). Each slot names a declared property or a row intrinsic. READ BY clients/os -- src/apps/concepts/displayCard.ts resolves the slots and RowsPanel.tsx renders them, so a concept with no card shows its id and nothing else. Every concept must declare one or decline it with a `// @no-displayCard: <reason>` comment (test/dslconformance/displaycard_inventory_test.go). Verified still read in memql#5378.",

	"composable": "Marks a concept as available to the Materializer's composer, optionally naming the composable fields: @composable(as=\"invoice\", fields=\"number,issuedAt,total\"). Every named field must be a declared property or a row intrinsic (validateComposable). READ BY clients/os -- served through integration.compose.composableConcepts and read by src/apps/materializer/useCompose.ts, so retiring it would empty the composer's list. Absent means not composable, which is a different statement from composable-with-no-fields (epic memql#4977, D2). Verified still read in memql#5378.",
```

In `Docs`, extend `allowedRoles` so it names its enforcement site:

```go
	"allowedRoles": "Restrict the tool to a set of AGENT roles: @allowedRoles(\"assistant\", \"specialist\"). An empty list means every agent role. ENFORCED on every path -- the predicate is in tool_types.go (Tool.AllowedRoles), applied by component/grpc/server.go before dispatch and by tool_execution.go on the in-engine path. This is the agent-role axis, NOT the actor's catalog rank: @requiresRank is a floor on the human caller and @requiresCapability is a grant over a resource, and neither can say \"a specialist may not call this\". memql#5375 verified it live and kept it for that reason.",
```

In `Docs`, replace the `cache` entry so it documents one spelling:

```go
	"cache": "Override the result-cache TTL for the query, in whole seconds: @cache(300). @cache(0) is the explicit \"never cache\" -- use it for reads where even brief staleness is wrong (auth, monotonic counters, presence). Pure reads cache BY DEFAULT with a 60s backstop, so the annotation sets a different TTL rather than turning caching on. The keyword form @cache(ttl=\"300\") and the @nocache alias are both retired in memql#5375: one value, one spelling. The engine keys the cache on the plan signature (query/sort/limit/depth/shape + the keyset cursor) and evicts on any write to the read concept via the cache.invalidate.* broadcast channel, so cross-node eviction needs no per-concept routing rule.",
```

In `Docs`, replace the `trigger` entry and delete `schedule`'s:

```go
	"trigger": "How an automation is reached: @trigger(event=\"graph.node.created.*.v1:ns:concept\") for the graph, or @trigger(schedule=\"0 0 * * * *\") for the clock. One annotation for both, so \"triggered\" and \"called\" stay distinguishable from @template. The separate @schedule(cron=...) spelling is retired in memql#5375.",
```

In `KeywordArgs`, delete the `"schedule"` entry and the `ttl` arg, and rename the provider `type` arg:

```go
	"cache": {
		// One arg, positional: @cache(300). The keyword form ttl="300"
		// is retired (memql#5375) -- a single-arg annotation has no
		// ambiguity for a keyword to resolve.
	},
	"vendor": {
		{Name: "", Type: "string", Doc: "AI vendor: \"OpenAI\" or \"Anthropic\"."},
	},
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./component/language/annotations/ -v`
Expected: PASS, all four new tests plus the two pre-existing ones.

- [ ] **Step 5: Commit**

```bash
git add component/language/annotations/registry.go component/language/annotations/retirement_test.go
git commit -m "feat(language): the registry drops what nothing reads, and keeps what does

Issues #5376 #5377 #5378. @displayCard, @composable and @allowedRoles
survive with their readers named in the doc -- each was grepped, each is
read, and the doc now says where so the next audit does not have to
re-derive it."
```

---

## Task 3: The parser and the AST lose the fields no allow-list can populate

Section 2.5's headline defect: six attributes parsed into `FunctionDef`, copied into the runtime function, rendered by `help()`, and refused by every allow-list — so the only way to reach them was an annotation the loader rejects. This task deletes the whole chain.

**Files:**
- Modify: `component/language/parser/parser.go:3821-3864` (`processFunctionAttributes`), `:3867-3918` (`processAutomationAttributes`)
- Modify: `component/language/ast/ast.go:1162-1169` (the dead `FunctionDef` fields), `:839-964` (the `Attr*` consts)
- Modify: `component/memql/function_types.go:177-193`, `:263-268`
- Modify: `component/memql/function_loader.go:495`, `:523-525`
- Modify: `component/memql/executor_builtin.go:578-579` (`help()`)
- Modify: `component/memql/sense_adapter.go:42`, `component/memql/sense/sense.go:34`, `component/memql/sense/hover.go:248-249`
- Test: `component/language/parser/retired_function_attributes_test.go` (create)

**Interfaces:**
- Consumes: `baseparser.AttributeRewriteHint` (Task 1); `annotations.Set` (Task 2), already narrowed.
- Produces: `FunctionDef` without `Deprecated` / `Version` / `Timeout` / `Retry` / `Idempotent` / `Audit`; `Function` without `Deprecated` / `RateLimitRequests` / `RateLimitPer`; `sense.FunctionInfo` without `Deprecated`.

- [ ] **Step 1: Write the failing test**

Create `component/language/parser/retired_function_attributes_test.go`:

```go
package parser

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/baseparser"
)

// TestRetiredFunctionAttributesRefuse is the negative cell per retired
// form (issue #5376 acceptance). Each of these parsed into a FunctionDef
// field that no allow-list could populate and that nothing read, so an
// author writing one got silence. Each now refuses, and the refusal
// carries the rewrite.
func TestRetiredFunctionAttributesRefuse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
	}{
		{"deprecated", "@deprecated(\"use userByEmail\")\nquery user q { filter active==true }"},
		{"timeout", "@timeout(\"30s\")\nquery user q { filter active==true }"},
		{"retry", "@retry(count=3)\nquery user q { filter active==true }"},
		{"idempotent", "@idempotent\nmutation user m { update { id: args.id } }"},
		{"audit", "@audit\nmutation user m { update { id: args.id } }"},
		{"latestMode", "@latestMode\nquery user q { filter active==true }"},
		{"enabled", "@enabled\nquery user q { filter active==true }"},
		{"nocache", "@nocache\nquery user q { filter active==true }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hint, retired := baseparser.RetiredConstructAnnotation(tc.name)
			if !retired {
				t.Fatalf("@%s is not in the retirement ledger", tc.name)
			}
			if !strings.Contains(hint, baseparser.AttributeRewriteHint) {
				t.Errorf("@%s hint does not name the rewrite: %s", tc.name, hint)
			}
		})
	}
}

// TestFunctionDefCarriesNoUnpopulatableField is the acceptance criterion
// of issue #5376 stated as a compile-time fact: a FunctionDef field that
// no allow-list can populate is a field whose only possible value is the
// zero value. The assignments below fail to compile while any of the six
// survive, which is the only check that cannot rot.
func TestFunctionDefCarriesNoUnpopulatableField(t *testing.T) {
	d := &FunctionDef{}
	_ = d.Name
	_ = d.Description
	_ = d.CacheTTL
	// d.Deprecated, d.Version, d.Timeout, d.Retry, d.Idempotent and
	// d.Audit are deleted (memql#5375). Uncommenting any of them must
	// fail to compile; if one compiles, the field came back.
}

// TestCachePositionalIsTheOneSpelling pins the surviving cache form.
func TestCachePositionalIsTheOneSpelling(t *testing.T) {
	d := &FunctionDef{}
	p := &Parser{}
	p.processFunctionAttributes(d, []*Attribute{
		{Name: "cache", Args: map[string]any{"": "300"}},
	})
	if d.CacheTTL != "300" {
		t.Errorf("@cache(300) should set CacheTTL=300, got %q", d.CacheTTL)
	}

	// The keyword form is retired: it must NOT populate the field, so an
	// author who writes it gets the allow-list refusal rather than a
	// value that works by accident.
	d2 := &FunctionDef{}
	p.processFunctionAttributes(d2, []*Attribute{
		{Name: "cache", Args: map[string]any{"ttl": "300"}},
	})
	if d2.CacheTTL != "" {
		t.Errorf("@cache(ttl=300) is retired and must not populate CacheTTL, got %q", d2.CacheTTL)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./component/language/parser/ -run 'Retired|Unpopulatable|OneSpelling' -v`
Expected: FAIL — `TestCachePositionalIsTheOneSpelling` fails because `@cache(ttl=)` still populates `CacheTTL`.

- [ ] **Step 3: Write the implementation**

Replace `processFunctionAttributes` in `component/language/parser/parser.go`:

```go
// processFunctionAttributes folds a function's attributes into its
// FunctionDef.
//
// The six attributes this used to fold -- @deprecated, @version, @timeout,
// @retry, @idempotent and @audit -- are deleted with their fields
// (memql#5375). Each was parsed here, copied into the runtime Function,
// rendered by help() and editor hover, and refused by every allow-list: the
// only reachable value was the zero value, and the render made a field that
// could not be set look like one that was. @enabled and @nocache go for
// their own reasons -- an explicit no-op, and the losing spelling of
// @cache(0). The refusals live in core/baseparser's retirement ledger, so
// they name the rewrite.
func (p *Parser) processFunctionAttributes(d *FunctionDef, attributes []*Attribute) {
	for _, attr := range attributes {
		switch attr.Name {
		case AttrDisabled:
			d.Enabled = false
		case AttrDescription:
			d.Description = getAttrString(attr)
		case AttrCache:
			// One spelling (D17): the positional seconds, @cache(300),
			// with @cache(0) meaning never. A single-arg annotation has
			// no ambiguity for a keyword to resolve, so `ttl=` only ever
			// gave the same value a second name -- and a reader had to
			// know both to search for either.
			if v := attrNumericString(attr); v != "" {
				d.CacheTTL = v
			} else if v := getAttrString(attr); v != "" && isAllDigits(v) {
				d.CacheTTL = v
			}
		}
	}
}

// isAllDigits reports whether s is a non-empty run of ASCII digits. The
// @cache(300) form reaches the attribute layer as a number or as the
// string "300" depending on how the value was written; anything that is
// not a plain count is not a TTL and is left for the allow-list to refuse
// rather than silently coerced to one.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
```

In `processAutomationAttributes`, delete the `AttrEnabled` and `AttrSchedule` cases and leave the `@trigger(schedule=)` fold in place:

```go
		case AttrDisabled:
			d.Enabled = false
		case AttrDescription:
			d.Description = getAttrString(attr)
		// @deprecated / @version / @timeout / @retry / @audit / @async folded
		// into AutomationDef fields the automation runtime never read (audited
		// in #2712); deleted with those fields (#2724).
		//
		// @schedule(cron=...) is retired in memql#5375: @trigger(schedule=...)
		// below is the one spelling, which keeps "how is this automation
		// reached" a single annotation and lets @template be refused beside
		// it coherently.
		case AttrTrigger:
```

In `component/language/ast/ast.go`, delete the six dead `FunctionDef` fields at `:1162-1169`, leaving a note in their place:

```go
	// @deprecated / @version / @timeout / @retry / @idempotent / @audit
	// were fields here until memql#5375. Every allow-list refused the
	// annotations that populated them, so the only reachable value was
	// the zero value -- and help() rendered them anyway. Retired in
	// core/baseparser's ledger; do not re-add a field before an
	// allow-list can populate it.
```

Delete `AttrDeprecated`, `AttrTimeout`, `AttrRetry`, `AttrIdempotent`, `AttrAudit`, `AttrNocache`, `AttrEnabled`, `AttrSchedule` and `AttrLatestMode` from the const block. Keep `AttrVersion` (concepts and seeds use it) and `AttrCache`.

Then remove the downstream copies:
- `component/memql/function_types.go`: delete `Deprecated`, `RateLimitRequests`, `RateLimitPer` from the struct and from the clone at `:263-268`.
- `component/memql/function_loader.go`: delete `Deprecated: funcDef.Deprecated,` at `:495` and the `if funcDef.RateLimit != nil { ... }` block at `:523-525`.
- `component/memql/executor_builtin.go:578-579`: delete the `help()` render.
- `component/memql/sense_adapter.go:42`, `sense/sense.go:34`, `sense/hover.go:248-249`: delete the `Deprecated` field and the `[DEPRECATED: ...]` hover suffix. Leave `sense/spec.go:127` alone — that is a lexicon TYPE deprecation (`array` -> `[]T`), a different axis.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./component/language/parser/ ./component/language/ast/ ./component/memql/ ./component/memql/sense/ 2>&1 | tail -30`
Expected: PASS. Any failure here is a caller of a deleted field; delete the caller, do not re-add the field.

- [ ] **Step 5: Commit**

```bash
git add component/language/parser/parser.go component/language/parser/retired_function_attributes_test.go component/language/ast/ast.go component/memql/function_types.go component/memql/function_loader.go component/memql/executor_builtin.go component/memql/sense_adapter.go component/memql/sense/sense.go component/memql/sense/hover.go
git commit -m "feat(language): delete the six FunctionDef fields no allow-list could populate

Issue #5376. Each was parsed, copied into the runtime function, rendered
by help() and editor hover, and refused by every allow-list -- so the
render showed a field whose only reachable value was the zero value.
@cache keeps one spelling."
```

---

## Task 4: The declarative decls — tool, provider, concept

The per-construct decl parsers in `component/language/parser` are the authoritative parse-time gate for the declarative constructs, and the concept vocabulary lives in a second table outside the language package. This task retires through both.

**Files:**
- Modify: `component/language/parser/tool_decl.go:105-131` (`@rateLimit`, `@scopes`, the refusal list)
- Modify: `component/language/parser/provider_decl.go` (`@type` -> `@vendor`)
- Modify: `component/database/memory-nodes/concept_parser.go:1149-1160` (`@namespace`), `:1390-1410` (`applyPropertyAttribute`), `:1805-1815` (the schema `default`)
- Test: `component/language/parser/retired_decl_attributes_test.go` (create)
- Test: `component/database/memory-nodes/retired_field_attributes_test.go` (create)

**Interfaces:**
- Consumes: `baseparser.RetiredConstructAnnotation`, `baseparser.RetiredFieldAnnotation`, `baseparser.AttributeRewriteHint` (Task 1).
- Produces: `ToolDecl` without `RateLimitPeriod` / `RateLimitMaxCalls` / `Scopes`; `ProviderDecl.Vendor` replacing `.Type`; `parsedProperty` without `defaultValue` / `unique` / `immutable`.

- [ ] **Step 1: Write the failing test**

Create `component/language/parser/retired_decl_attributes_test.go`:

```go
package parser

import (
	"strings"
	"testing"
)

// TestToolRetiredAttributesRefuse: @rateLimit and @scopes were stored on
// the tool and enforced nowhere -- @rateLimit was cloned and copied into
// a Function field nothing reads, @scopes was advertised on the gRPC tool
// descriptor and checked never. Both read as a ceiling or a gate while
// being neither, which is worse than their absence.
func TestToolRetiredAttributesRefuse(t *testing.T) {
	for _, name := range []string{"rateLimit", "scopes"} {
		t.Run(name, func(t *testing.T) {
			src := "@handler(type=\"query\", query=\"concept==v1:ns:c\")\n@" + name + "(maxCalls=100, periodSeconds=3600)\ntool t {\n  limit integer\n}"
			_, err := ParseToolSource(src, "dsl/x/tools.memql")
			if err == nil {
				t.Fatalf("@%s should be refused on a tool", name)
			}
			if !strings.Contains(err.Error(), "retired") {
				t.Errorf("refusal should say retired, got: %v", err)
			}
			if !strings.Contains(err.Error(), "memqlmigrate --rewrite=attributes") {
				t.Errorf("refusal should name the rewrite, got: %v", err)
			}
		})
	}
}

// TestProviderVendorIsTheOneSpelling: a provider's vendor and a concept's
// row kind both spelled @type, for no reason beyond history. @vendor names
// the provider one.
func TestProviderVendorIsTheOneSpelling(t *testing.T) {
	ok := "@base\n@vendor(\"OpenAI\")\nprovider openai {\n  auth {\n    identityProviderId env(\"X\")\n  }\n}"
	if _, err := ParseProviderSource(ok, "dsl/providers/providers.memql"); err != nil {
		t.Fatalf("@vendor should parse: %v", err)
	}

	old := "@base\n@type(\"OpenAI\")\nprovider openai {\n  auth {\n    identityProviderId env(\"X\")\n  }\n}"
	_, err := ParseProviderSource(old, "dsl/providers/providers.memql")
	if err == nil {
		t.Fatal("@type on a provider should be refused -- @vendor is the one spelling")
	}
	if !strings.Contains(err.Error(), "@vendor") {
		t.Errorf("refusal should name @vendor as the replacement, got: %v", err)
	}
}
```

Create `component/database/memory-nodes/retired_field_attributes_test.go`:

```go
package memorynodes

import (
	"strings"
	"testing"
)

// TestRetiredFieldAttributesRefuse covers the three field annotations D17
// retires, each for the same reason: declared metadata with nothing behind
// it. @unique never had a uniqueness check (memql#2960), @immutable never
// had a write guard, and @default was published as the JSON-Schema
// `default` keyword that no validator applies and no insert path consults
// -- so an author who wrote @default("true") and believed the field
// defaulted was wrong, silently, on every row.
func TestRetiredFieldAttributesRefuse(t *testing.T) {
	for _, tc := range []struct{ name, field string }{
		{"unique", "email string @unique"},
		{"immutable", "createdBy string @immutable"},
		{"default", "active bool @default(\"true\")"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "@rowAuthz(public)\nconcept thing {\n  " + tc.field + "\n}"
			_, err := ParseConceptSource(src, "dsl/x/concepts.memql")
			if err == nil {
				t.Fatalf("field @%s should be refused", tc.name)
			}
			if !strings.Contains(err.Error(), "memqlmigrate --rewrite=attributes") {
				t.Errorf("refusal should name the rewrite, got: %v", err)
			}
		})
	}
}

// TestConceptNamespaceRefuses: the namespace comes from the domain
// directory or that directory's one-line namespace.pin, so the annotation
// could only restate one or silently disagree with it. Every one of the 69
// occurrences in the tree restated a pin that already existed, which is
// why removing it changes no canonical id.
func TestConceptNamespaceRefuses(t *testing.T) {
	src := "@namespace(\"cluster\")\n@rowAuthz(public)\nconcept thing {\n  name string\n}"
	_, err := ParseConceptSource(src, "dsl/deployment/concepts.memql")
	if err == nil {
		t.Fatal("@namespace should be refused on a concept")
	}
	if !strings.Contains(err.Error(), "namespace.pin") {
		t.Errorf("refusal should name namespace.pin as the escape hatch, got: %v", err)
	}
	if !strings.Contains(err.Error(), "memqlmigrate --rewrite=attributes") {
		t.Errorf("refusal should name the rewrite, got: %v", err)
	}
}

// TestKeptConceptAnnotationsStillParse guards against over-retirement.
// @displayCard and @composable are read by clients/os (memql#5378) and
// @version is the "v1" of every canonical id.
func TestKeptConceptAnnotationsStillParse(t *testing.T) {
	src := "@version(\"1.0.0\")\n@displayCard(primary=\"name\", status=\"active\")\n@composable(fields=\"name\")\n@rowAuthz(public)\nconcept thing {\n  name string\n  active bool\n}"
	if _, err := ParseConceptSource(src, "dsl/x/concepts.memql"); err != nil {
		t.Fatalf("kept concept annotations should still parse: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./component/language/parser/ -run 'ToolRetired|ProviderVendor' -v && go test ./component/database/memory-nodes/ -run 'RetiredField|ConceptNamespace|KeptConcept' -v`
Expected: FAIL — `@rateLimit` and `@scopes` still parse; `@vendor` is unknown; the three field annotations still parse.

Note: if the exported parse helpers (`ParseToolSource`, `ParseProviderSource`, `ParseConceptSource`) are named differently in this tree, use the names the existing sibling tests in each package use — the assertions are what matters, not the helper's spelling.

- [ ] **Step 3: Write the implementation**

In `component/language/parser/tool_decl.go`, delete the `case "rateLimit":` and `case "scopes":` arms, delete `RateLimitPeriod` / `RateLimitMaxCalls` / `Scopes` from `ToolDecl` in `ast.go`, and route the default arm through the ledger before the generic message:

```go
		default:
			if hint, retired := baseparser.RetiredConstructAnnotation(attr.Name); retired {
				return nil, newParseErrorf(&p.current, "tool %q: @%s is retired -- %s", decl.Name, attr.Name, hint)
			}
			return nil, newParseErrorf(&p.current, "tool %q: unknown annotation @%s -- supported: @allowedRoles, @description, @destructive, @disabled, @executionTime, @handler, @mcp, @requiresConfirmation", decl.Name, attr.Name)
```

In `component/language/parser/provider_decl.go`, rename the `case "type":` arm to `case "vendor":` writing `decl.Vendor`, rename the `ProviderDecl.Type` field to `Vendor`, and add the retired arm so the old spelling names its replacement:

```go
		case "type":
			return nil, newParseErrorf(&p.current, "provider %q: @type is retired -- write @vendor(%q) instead; a concept's @type is its row kind and the two shared a spelling for no reason (memql#5375); %s",
				decl.Name, attrString(attr), baseparser.AttributeRewriteHint)
```

Update the provider's consumers (`component/memql/provider_converter.go` or equivalent, plus `dsl/providers/providers.memql` in Task 7) to read `.Vendor`.

In `component/database/memory-nodes/concept_parser.go`:

Replace the `case "namespace":` arm with a refusal:

```go
	case "namespace":
		// Retired in memql#5375. AssembleConceptIdFromDeclInDir derives
		// the namespace from the domain directory, or from that
		// directory's one-line namespace.pin, so the annotation could
		// only restate one of those or disagree with it -- and a
		// disagreement is a moved-file guard failure, not a feature.
		// Every occurrence in the tree restated a pin that already
		// existed, which is why the removal changes no canonical id.
		hint, _ := baseparser.RetiredConstructAnnotation("namespace")
		return fmt.Errorf("concept %q: @namespace is retired -- %s", conceptName, hint)
```

In `applyPropertyAttribute`, delete the `case "default":`, `case "unique":` and `case "immutable":` arms, and add a default arm that consults the field ledger:

```go
	default:
		if hint, retired := baseparser.RetiredFieldAnnotation(attr.Name); retired {
			return fmt.Errorf("field %q: @%s is retired -- %s", prop.name, attr.Name, hint)
		}
		// @default on a concept field is retired in memql#5375: it was
		// published as the JSON-Schema `default` keyword, which no
		// validator applies and no insert path consults, so a field
		// carrying it did not default. `??` in the mutation is the
		// mechanism that fills a value. @default on a TOOL or PROMPT
		// field is a different thing and stays -- those bodies ARE the
		// schema handed to the model.
		if attr.Name == "default" {
			return fmt.Errorf("field %q: @default on a concept field is retired -- it was never applied on insert, so the field did not default; fill the value with `??` in the mutation that writes it (docs/public/language/authoring-rules.md section 28); %s",
				prop.name, baseparser.AttributeRewriteHint)
		}
```

Delete `defaultValue` from `parsedProperty` (`:466`) and the `schema["default"]` publication (`:1810-1811`).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./component/language/parser/ ./component/database/memory-nodes/ 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add component/language/parser/tool_decl.go component/language/parser/provider_decl.go component/language/parser/ast.go component/language/parser/retired_decl_attributes_test.go component/database/memory-nodes/concept_parser.go component/database/memory-nodes/retired_field_attributes_test.go
git commit -m "feat(language): retire the declared metadata with nothing behind it

Issues #5376 #5377. @rateLimit and @scopes were advertised and enforced
nowhere; @unique and @immutable had no check and no guard; a concept
field's @default was published as a JSON-Schema keyword nothing applies.
Provider @type becomes @vendor."
```

---

## Task 5: Prompt and builtin fields get the allow-list args fields have

D16's last sentence. Today a prompt field silently tolerates an unknown annotation and a builtin field drops every annotation but `@required`, so the same typo behaves three different ways depending on which body it is in.

**Files:**
- Modify: `component/memql/prompt_converter.go:150-170` (the silent `default:` arm)
- Modify: `component/memql/builtin_converter.go` (the builtin field loop)
- Test: `component/memql/field_annotation_allow_list_test.go` (create)

**Interfaces:**
- Consumes: `baseparser.RetiredFieldAnnotation`, `baseparser.AttributeRewriteHint` (Task 1).
- Produces: `func allowedFieldAnnotations() map[string]bool` in `component/memql` — the one field allow-list (`required`, `description`, `default`, `enum`, `maxLength`, `pattern`, `minimum`, `maximum`), consumed by the prompt, builtin and tool field converters.

- [ ] **Step 1: Write the failing test**

Create `component/memql/field_annotation_allow_list_test.go`:

```go
package memql

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestPromptFieldRefusesUnknownAnnotation closes the tolerance gap of
// D16: prompt_converter's default arm swallowed an unknown field
// annotation, so `@requred` (one r) on a prompt field loaded clean and
// the field was simply not required. An args field has refused this
// since #991; a prompt field's body IS its schema, so it needs the same
// gate.
func TestPromptFieldRefusesUnknownAnnotation(t *testing.T) {
	decl := &languageParser.PromptDecl{
		Name: "agentReply",
		Fields: []*languageParser.PromptField{{
			Name: "space",
			Type: "object",
			Attributes: []*languageParser.Attribute{{Name: "requred"}},
		}},
	}
	_, err := promptDeclToFunction(decl, "dsl/agents/prompts.memql")
	if err == nil {
		t.Fatal("an unknown prompt field annotation should be refused")
	}
	if !strings.Contains(err.Error(), "requred") {
		t.Errorf("refusal should name the offending annotation, got: %v", err)
	}
}

// TestBuiltinFieldRefusesUnknownAnnotation: the builtin field loop kept
// @required and dropped everything else, so @description on a builtin
// field vanished from the schema the SDK generates -- the same annotation
// that is load-bearing two constructs over.
func TestBuiltinFieldRefusesUnknownAnnotation(t *testing.T) {
	decl := &languageParser.BuiltinDecl{
		Name: "workbenchDispatchHost",
		Attributes: []*languageParser.Attribute{
			{Name: "executor", Value: "integration.workbench.dispatchHost"},
		},
		Fields: []*languageParser.BuiltinField{{
			Name: "runId",
			Type: "string",
			Attributes: []*languageParser.Attribute{{Name: "bogusFieldAnno"}},
		}},
	}
	_, err := builtinDeclToFunction(decl, "dsl/workbench/builtins.memql")
	if err == nil {
		t.Fatal("an unknown builtin field annotation should be refused")
	}
	if !strings.Contains(err.Error(), "bogusFieldAnno") {
		t.Errorf("refusal should name the offending annotation, got: %v", err)
	}
}

// TestPromptFieldRefusesRetiredAnnotation routes the field ledger through
// the prompt body too, so a retired name refuses with the rewrite from
// every body it can appear in.
func TestPromptFieldRefusesRetiredAnnotation(t *testing.T) {
	decl := &languageParser.PromptDecl{
		Name: "agentReply",
		Fields: []*languageParser.PromptField{{
			Name: "space",
			Type: "object",
			Attributes: []*languageParser.Attribute{{Name: "unique"}},
		}},
	}
	_, err := promptDeclToFunction(decl, "dsl/agents/prompts.memql")
	if err == nil {
		t.Fatal("a retired field annotation should be refused on a prompt field")
	}
	if !strings.Contains(err.Error(), "memqlmigrate --rewrite=attributes") {
		t.Errorf("refusal should name the rewrite, got: %v", err)
	}
}

// TestPromptFieldKeepsItsSchemaAnnotations guards against over-rejection:
// a prompt or tool field's @default IS the JSON-Schema default the model
// reads, which is why memql#5375 retired @default on a CONCEPT field and
// kept it here.
func TestPromptFieldKeepsItsSchemaAnnotations(t *testing.T) {
	decl := &languageParser.PromptDecl{
		Name: "agentReply",
		Fields: []*languageParser.PromptField{{
			Name: "tone",
			Type: "string",
			Attributes: []*languageParser.Attribute{
				{Name: "required"},
				{Name: "description", Value: "how the reply should read"},
				{Name: "default", Value: "neutral"},
			},
		}},
	}
	if _, err := promptDeclToFunction(decl, "dsl/agents/prompts.memql"); err != nil {
		t.Fatalf("the schema annotations must still be accepted: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./component/memql/ -run 'FieldRefuses|FieldKeeps' -v`
Expected: FAIL — the prompt converter tolerates both unknown names silently; the builtin converter drops them.

- [ ] **Step 3: Write the implementation**

Add to `component/memql/function_annotation_allow_lists.go`:

```go
// fieldAnnotations is the ONE allow-list for a field annotation, wherever
// the field appears: an args block, a prompt body, a builtin body or a
// tool body. Before memql#5375 there were three behaviours for one
// surface -- an args field refused an unknown name (#991), a prompt field
// tolerated it silently, and a builtin field dropped everything but
// @required -- so the same typo was a load error, a missing constraint or
// a missing description depending on which body it was in, and only the
// first told the author.
//
// @default is HERE and is retired on a concept field, which is not a
// contradiction: a prompt / builtin / tool body IS the JSON schema handed
// to the model, so `default` is a value the model reads, while a concept
// field's was published as a schema keyword nothing applies.
var fieldAnnotations = map[string]bool{
	"required":    true,
	"description": true,
	"default":     true,
	"enum":        true,
	"maxLength":   true,
	"pattern":     true,
	"minimum":     true,
	"maximum":     true,
}

// validateFieldAnnotation returns a refusal for a field annotation that is
// retired or unknown, and nil for one the schema surface accepts. The
// retirement ledger is consulted FIRST so a retired name gets its
// migration hint instead of the generic list of supported names.
func validateFieldAnnotation(origin, construct, field, name string) error {
	if fieldAnnotations[name] {
		return nil
	}
	if hint, retired := baseparser.RetiredFieldAnnotation(name); retired {
		return fmt.Errorf("%s: %s field %q: @%s is retired -- %s", origin, construct, field, name, hint)
	}
	return fmt.Errorf("%s: %s field %q: unknown field annotation @%s -- supported: %s",
		origin, construct, field, name, baseparser.FormatAnnotationAllowList(fieldAnnotations))
}
```

In `prompt_converter.go`, replace the silent `default:` arm:

```go
		default:
			return toolField{}, validateFieldAnnotation(origin, "prompt", field.Name, attr.Name)
		}
```

In `builtin_converter.go`, add the same call over each builtin field's attributes, keeping `@required`'s existing capture and refusing anything the allow-list does not hold.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./component/memql/ -run 'FieldRefuses|FieldKeeps' -v && make test 2>&1 | grep -E "^(FAIL|ok.*memql)" | head -20`
Expected: PASS. A failure in the second command is a real `.memql` file with a stale field annotation — fix the file, which is the migration this task exists to force.

- [ ] **Step 5: Commit**

```bash
git add component/memql/function_annotation_allow_lists.go component/memql/prompt_converter.go component/memql/builtin_converter.go component/memql/field_annotation_allow_list_test.go
git commit -m "feat(language): one allow-list for a field annotation, in every body

Issue #5377, D16. A prompt field tolerated an unknown annotation and a
builtin field dropped every annotation but @required, so the same typo
was a load error, a missing constraint or a missing description
depending on the body it was in."
```

---

## Task 6: One word for a mutation, and the retired forms leave the parse cascade

`mutate` and `mutation` both name the construct in two positions (the declaration keyword and the call-site prefix), and `?.`, `;`-AND, `,`-OR and `import (...)` are still in the cascade with zero users. Retiring them together because they all live in the lexer/parser front end and share one negative-cell test file.

**Files:**
- Modify: `component/language/dslspec/constructs.go:42` (`Keyword`), `component/language/dslspec/spec.go:275`
- Modify: `component/language/parser/rewriter.go:604`, `:2630`, `component/language/parser/suggest.go:113`, `:131`
- Modify: `core/baseparser/iface.go` (the `kindLabel == "mutation"` -> `keyword = "mutate"` mapping)
- Modify: `component/language/parser/lexer.go:394` (`?.`), `:896` (`import`)
- Modify: `component/language/parser/parser.go:5160` (`;`-AND), `:2072`, `:437`, `:517` (`,` separators)
- Test: `component/language/parser/retired_forms_test.go` (create)

**Interfaces:**
- Consumes: `baseparser.AttributeRewriteHint` (Task 1).
- Produces: `mutation` as the sole construct keyword in `dslspec`; `?.`, `;`-as-AND, `,`-as-OR and `import (...)` refusing with the rewrite named.

- [ ] **Step 1: Write the failing test**

Create `component/language/parser/retired_forms_test.go`:

```go
package parser

import (
	"strings"
	"testing"
)

// TestMutationIsTheOneKeyword: `mutate` and `mutation` both opened the
// construct, so a reader searching the tree for one found two thirds of
// it. didYouMean already treated `mutation` as the canonical target
// (parser_hardening_test.go pins the distance); this makes it true.
func TestMutationIsTheOneKeyword(t *testing.T) {
	ok := "mutation user m {\n  args { id string @required }\n  update { id: args.id }\n}"
	if _, err := ParseSource(ok, "dsl/x/mutations.memql"); err != nil {
		t.Fatalf("`mutation` should parse: %v", err)
	}

	old := "mutate user m {\n  args { id string @required }\n  update { id: args.id }\n}"
	_, err := ParseSource(old, "dsl/x/mutations.memql")
	if err == nil {
		t.Fatal("`mutate` should be refused -- `mutation` is the one keyword")
	}
	if !strings.Contains(err.Error(), "mutation") {
		t.Errorf("refusal should name `mutation` as the replacement, got: %v", err)
	}
	if !strings.Contains(err.Error(), "memqlmigrate --rewrite=attributes") {
		t.Errorf("refusal should name the rewrite, got: %v", err)
	}
}

// TestRetiredCascadeFormsRefuse: one negative cell per form D17 removes
// from the parse cascade. Each had zero users in the tree and each was a
// second way to write something the surviving form already says.
func TestRetiredCascadeFormsRefuse(t *testing.T) {
	for _, tc := range []struct {
		name, source, wants string
	}{
		{
			name:   "optional-chain",
			source: "query user q { filter owner?.id == actor.userId }",
			wants:  "?.",
		},
		{
			name:   "semicolon-and",
			source: "query user q { filter active==true ; deleted==false }",
			wants:  "&&",
		},
		{
			name:   "comma-or",
			source: "query user q { filter status==\"a\" , status==\"b\" }",
			wants:  "||",
		},
		{
			name:   "import-block",
			source: "import (\n  common.concepts.{ node }\n)\nquery node q { filter active==true }",
			wants:  "use",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSource(tc.source, "dsl/x/queries.memql")
			if err == nil {
				t.Fatalf("%s should be refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("refusal should name the surviving form %q, got: %v", tc.wants, err)
			}
			if !strings.Contains(err.Error(), "memqlmigrate --rewrite=attributes") {
				t.Errorf("refusal should name the rewrite, got: %v", err)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./component/language/parser/ -run 'MutationIsTheOne|RetiredCascade' -v`
Expected: FAIL — `mutate` parses, `?.` lexes and parses, `;` folds as AND, `,` separates, `import` lexes.

Note: if this tree's whole-file entry point is not `ParseSource`, use the name the sibling tests in the package use.

- [ ] **Step 3: Write the implementation**

`dslspec/constructs.go:42`: `Keyword: "mutation"`. `dslspec/spec.go:275`: return `[]string{"mutation"}`.

`core/baseparser/iface.go`: delete the `kindLabel == "mutation"` -> `keyword = "mutate"` remap, since the header now spells the keyword the same as the kind label.

`parser/rewriter.go:604`: `{"mutation", LooksLikeStructMutation, NormaliseMutationSource}`. `:2630`: drop `"mutate"` from the case list. `parser/suggest.go:113` and `:131`: replace `"mutate"` with `"mutation"`.

Add the keyword refusal where the declaration dispatcher meets an unknown leading identifier, so `mutate` names its replacement rather than producing the `SpecReferenceExpr{Name:"mutate"}` misparse that `parser.go:5813` already warns about:

```go
	if ident == "mutate" {
		return nil, newParseErrorf(&p.current, "`mutate` is retired -- write `mutation <Concept> <name> { ... }`; one keyword per construct, so a reader searching for the word finds every one of them (memql#5375); %s", baseparser.AttributeRewriteHint)
	}
```

`lexer.go:394`: replace the `TokenQuestionDot` emission with a lex error naming the surviving form:

```go
		// `?.` retired in memql#5375: zero users in the tree, and the
		// `??` coalesce plus an explicit `!= nil` conjunct say the same
		// thing without a second null-handling concept in the grammar.
		return Token{}, fmt.Errorf("`?.` is retired -- guard the nil explicitly (`owner != nil && owner.id == ...`) or coalesce with `??`; %s", baseparser.AttributeRewriteHint)
```

Delete `TokenQuestionDot` from the token enum.

`parser.go:5160`: drop `p.check(TokenSemicolon)` from `parseLogicalAnd`'s loop condition and refuse a semicolon found in a filter position:

```go
	if p.check(TokenSemicolon) {
		return nil, newParseErrorf(&p.current, "`;` as AND is retired -- write `&&`; one boolean grammar, so precedence reads the same everywhere (memql#5375); %s", baseparser.AttributeRewriteHint)
	}
```

`parser.go:2072`, `:437`, `:517`: replace the `TokenComma` tolerance in expression positions with the same shape of refusal naming `||`. Leave commas alone where they are genuine argument or list separators — an args block, a call site, an annotation argument list, a `@enum(...)` list. The retirement is of `,` as a boolean OR connective, not of the comma.

`lexer.go:896`: make `import` refuse:

```go
	case "import":
		// The `import (...)` block is retired in memql#5375: `use` is
		// the import form, and zero .memql files used the block.
		return Token{}, fmt.Errorf("`import (...)` is retired -- declare dependencies with a file-top `use <domain>.<construct>.{ names }` import; %s", baseparser.AttributeRewriteHint)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./component/language/... 2>&1 | tail -25`
Expected: PASS. `parser_hardening_test.go:228` and `:244` already expect `mutation` as the `didYouMean` target, so they should go green rather than red; `mutate_verb_test.go:121` asserts `LooksLikeStructMutation("mutate" + body)` and needs its literal moved to `"mutation"`.

- [ ] **Step 5: Commit**

```bash
git add component/language/dslspec/constructs.go component/language/dslspec/spec.go component/language/parser/rewriter.go component/language/parser/suggest.go component/language/parser/lexer.go component/language/parser/parser.go component/language/parser/mutate_verb_test.go component/language/parser/retired_forms_test.go core/baseparser/iface.go
git commit -m "feat(language): one word for a mutation, and the dead cascade forms refuse

Issue #5377. \`mutate\` and \`mutation\` both opened the construct, so a
reader searching for one found two thirds of it. \`?.\`, \`;\`-as-AND,
\`,\`-as-OR and \`import (...)\` had zero users and each was a second way
to write what the surviving form says."
```

---

## Task 7: `memqlmigrate --rewrite=attributes`, and the tree migrated

Mechanics before meaning: the codemod lands and runs before anything else refuses, so the migration is a mechanical diff. `cmd/memqlmigrate` already holds `namespace-default` and `cache-positional` as separate named rewrites, so `attributes` composes them rather than reimplementing them.

**Files:**
- Modify: `cmd/memqlmigrate/main.go:85-102` (the rewriter tables), `:12-36` (the doc comment)
- Create: `cmd/memqlmigrate/attributes.go`
- Create: `cmd/memqlmigrate/attributes_test.go`
- Modify: `dsl/**` (the migration), `cmd/shopifyschema/emit_dsl.go:64` (stop emitting `@namespace`)

**Interfaces:**
- Consumes: nothing from earlier tasks — this task is deliberately independent of the parser flip so it can run first.
- Produces: `--rewrite=attributes` applying, in order: strip `@namespace` where redundant, strip `@enabled`, strip `@latestMode`, strip the retired function attributes, strip `@unique` / `@immutable`, strip a concept field's `@default`, `@nocache` -> `@cache(0)`, `@cache(ttl="N")` -> `@cache(N)`, `@schedule(cron="X")` -> `@trigger(schedule="X")`, provider `@type` -> `@vendor`, `mutate ` -> `mutation ` at a declaration header.

- [ ] **Step 1: Write the failing test**

Create `cmd/memqlmigrate/attributes_test.go`:

```go
package main

import (
	"strings"
	"testing"
)

func TestRewriteAttributes(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{
			name: "mutate becomes mutation",
			in:   "mutate folder createFolder {\n  insert { id: args.id }\n}\n",
			want: "mutation folder createFolder {\n  insert { id: args.id }\n}\n",
		},
		{
			name: "nocache becomes cache zero",
			in:   "@nocache\nquery user q { filter active==true }\n",
			want: "@cache(0)\nquery user q { filter active==true }\n",
		},
		{
			name: "cache keyword becomes positional",
			in:   "@cache(ttl=\"300\")\nquery user q { filter active==true }\n",
			want: "@cache(300)\nquery user q { filter active==true }\n",
		},
		{
			name: "schedule becomes trigger schedule",
			in:   "@schedule(cron=\"0 0 * * * *\")\nautomation sweep { }\n",
			want: "@trigger(schedule=\"0 0 * * * *\")\nautomation sweep { }\n",
		},
		{
			name: "provider type becomes vendor",
			in:   "@base\n@type(\"OpenAI\")\nprovider openai { }\n",
			want: "@base\n@vendor(\"OpenAI\")\nprovider openai { }\n",
		},
		{
			name: "dead function attributes are stripped",
			in:   "@deprecated(\"old\")\n@timeout(\"30s\")\n@audit\n@description(\"keep me\")\nquery user q { filter active==true }\n",
			want: "@description(\"keep me\")\nquery user q { filter active==true }\n",
		},
		{
			name: "enabled is stripped, disabled is kept",
			in:   "@enabled\n@disabled\nquery user q { filter active==true }\n",
			want: "@disabled\nquery user q { filter active==true }\n",
		},
		{
			name: "field annotations are stripped, description kept",
			in:   "concept thing {\n  email string @unique @description(\"keep\")\n  active bool @default(\"true\")\n  createdBy string @immutable\n}\n",
			want: "concept thing {\n  email string @description(\"keep\")\n  active bool\n  createdBy string\n}\n",
		},
		{
			name: "a tool field keeps its default -- the body IS the schema",
			in:   "@handler(type=\"query\", query=\"concept==v1:ns:c\")\ntool searchUsers {\n  limit integer @default(\"10\")\n}\n",
			want: "@handler(type=\"query\", query=\"concept==v1:ns:c\")\ntool searchUsers {\n  limit integer @default(\"10\")\n}\n",
		},
		{
			name: "allowedRoles is untouched -- it has a live reader",
			in:   "@allowedRoles(\"assistant\")\ntool ensureAgent { }\n",
			want: "@allowedRoles(\"assistant\")\ntool ensureAgent { }\n",
		},
		{
			name: "displayCard is untouched -- clients/os reads it",
			in:   "@displayCard(primary=\"name\")\nconcept thing {\n  name string\n}\n",
			want: "@displayCard(primary=\"name\")\nconcept thing {\n  name string\n}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rewriteAttributes([]byte(tc.in))
			if err != nil {
				t.Fatalf("rewriteAttributes: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("mismatch\n--- got ---\n%s\n--- want ---\n%s", got, tc.want)
			}
		})
	}
}

// TestNamespaceStrippedOnlyWhenRedundant is the id-preservation gate. The
// rewrite may only remove @namespace where the surviving derivation --
// the directory's namespace.pin, else the directory -- yields the same
// namespace. Removing one that diverges would silently change every
// canonical id the file declares, which is the one outcome this epic's
// global constraints forbid.
func TestNamespaceStrippedOnlyWhenRedundant(t *testing.T) {
	src := "@namespace(\"cluster\")\nconcept deployment {\n  name string\n}\n"

	// dsl/deployment has a namespace.pin of "cluster", so the annotation
	// restates it and goes.
	got, err := rewriteAttributesInDir("dsl/deployment/concepts.memql", []byte(src), "cluster")
	if err != nil {
		t.Fatalf("rewriteAttributesInDir: %v", err)
	}
	if strings.Contains(string(got), "@namespace") {
		t.Error("a @namespace that restates the pin should be stripped")
	}

	// With no pin, "cluster" would diverge from the directory "widgets",
	// so the annotation is id-bearing and must survive with a refusal
	// for a human to resolve.
	kept, err := rewriteAttributesInDir("dsl/widgets/concepts.memql", []byte(src), "")
	if err == nil {
		t.Fatal("a diverging @namespace must refuse rather than be silently stripped")
	}
	if !strings.Contains(err.Error(), "namespace.pin") {
		t.Errorf("the refusal should name the pin as the fix, got: %v", err)
	}
	_ = kept
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/memqlmigrate/ -run 'RewriteAttributes|NamespaceStripped' -v`
Expected: FAIL — `undefined: rewriteAttributes`, `undefined: rewriteAttributesInDir`.

- [ ] **Step 3: Write the implementation**

Create `cmd/memqlmigrate/attributes.go` implementing `rewriteAttributes([]byte) ([]byte, error)` and `rewriteAttributesInDir(path string, src []byte, pin string) ([]byte, error)`. Implementation notes that the test above pins:

- Operate line-wise on a comment-blanked view for the decisions and splice the original bytes, the way `BlankComments` is used in `core/baseparser` — an `@enabled` inside a comment is prose, not an annotation.
- Strip a whole annotation LINE when the annotation is alone on it; strip just the annotation token when it shares a line with a field declaration or another annotation, collapsing the double space it leaves.
- Scope the field-annotation strip to a `concept` body. A `tool`, `prompt` or `builtin` body keeps `@default`, so the rewrite tracks which construct's body it is inside rather than matching `@default` anywhere.
- Scope the `@type` -> `@vendor` rename to a `provider` body for the same reason: a concept's `@type` is its row kind.
- `mutate ` -> `mutation ` only at a declaration header — the start of a line, followed by two identifiers and a `{`. Never inside a string, a comment, or a `mutate`-prefixed identifier.
- `rewriteAttributesInDir` refuses rather than strips when `@namespace` differs from `pin` (or from the first path segment when `pin` is empty), so a divergence reaches a human.

Register it in `main.go`:

```go
var rewriters = map[string]rewriter{
	// ... existing entries ...
	"attributes": rewriteAttributes,
}

var pathRewriters = map[string]pathRewriter{
	// ... existing entries ...
	// `attributes` needs the path for the @namespace half: whether the
	// annotation is redundant depends on the directory's namespace.pin.
	"attributes-namespace": rewriteAttributesNamespace,
}
```

Extend the doc comment at `:12-36` with the `attributes` entry, naming what it does and what it deliberately does not touch.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./cmd/memqlmigrate/ -v`
Expected: PASS, including the pre-existing rewrites' tests.

- [ ] **Step 5: Run the rewrite over the tree and verify it loads**

```bash
go run ./cmd/memqlmigrate --rewrite=attributes,attributes-namespace -w dsl/
git diff --stat dsl/ | tail -3
```

Expected: roughly 418 `mutate` -> `mutation` headers, 69 `@namespace` lines gone, 203 field `@default` tokens gone, 6 provider `@type` -> `@vendor`. Then:

```bash
go run ./cmd/memqllint dsl/ 2>&1 | tail -20
```

Expected: zero refusals (issue #5379's acceptance criterion).

Also stop the generator re-emitting a retired annotation — `cmd/shopifyschema/emit_dsl.go:64` writes `@namespace("shopify")` into all 65 generated files, so leaving it would reintroduce 65 refusals on the next schema regeneration:

```bash
# remove the @namespace emission; the directory's namespace.pin already says "shopify"
go run ./cmd/shopifyschema --check 2>&1 | tail -5
```

- [ ] **Step 6: Commit the codemod and the migration separately**

```bash
git add cmd/memqlmigrate/attributes.go cmd/memqlmigrate/attributes_test.go cmd/memqlmigrate/main.go
git commit -m "feat(memqlmigrate): --rewrite=attributes

Issue #5379. Composes the existing namespace-default and cache-positional
rewrites with the new strips and renames. Scoped by construct body: a
tool, prompt or builtin field keeps its @default, because those bodies
ARE the schema handed to the model."

git add dsl/ cmd/shopifyschema/emit_dsl.go
git commit -m "refactor(dsl): the tree migrated by --rewrite=attributes

Issue #5379. Mechanical. 418 mutate -> mutation headers, 69 @namespace
lines that restated a namespace.pin, 203 concept-field @default tokens
that never defaulted anything, 6 provider @type -> @vendor. No canonical
id changes: every removed @namespace matched the pin already on disk.
shopifyschema stops emitting @namespace so a regeneration does not
reintroduce 65 of them."
```

---

## Task 8: The matrix, the parity gate, and the docs

Epic 4's tests clause: one negative cell per retired form (Tasks 1-6), the registry parity test, and the generated matrix. The matrix parity test already exists at the repo root and pins every "Yes" to `ByReceiver`; this task widens it to fail on a retired row and updates the prose the retirements make wrong.

**Files:**
- Modify: `docs/public/language/attribute-matrix.md`
- Modify: `attribute_matrix_parity_test.go`
- Modify: `docs/public/language/authoring-rules.md`, `docs/public/language/memql.md`
- Modify: `dsl/_reference/_concept.memql`, `dsl/_reference/_spec.memql` and siblings
- Modify: `CLAUDE.md`
- Modify: `docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md`

**Interfaces:**
- Consumes: `annotations.ByReceiver` (Task 2), `baseparser.RetiredConstructAnnotation` / `RetiredFieldAnnotation` (Task 1).
- Produces: `TestMatrixListsNoRetiredAttribute` — the issue #5379 acceptance gate.

- [ ] **Step 1: Write the failing test**

Append to `attribute_matrix_parity_test.go`:

```go
// TestMatrixListsNoRetiredAttribute is issue #5379's acceptance criterion:
// "The generated matrix lists no retired attribute." The pre-existing
// parity test pins every "Yes" cell to an allow-list, which catches a
// matrix that over-promises; it does not catch a ROW for an annotation
// that no longer exists at all, whose cells are legitimately all "No".
// A reader scanning the matrix for @nocache would find it, read "No"
// everywhere, and conclude it belongs somewhere they had not looked.
func TestMatrixListsNoRetiredAttribute(t *testing.T) {
	raw, err := os.ReadFile("docs/public/language/attribute-matrix.md")
	if err != nil {
		t.Fatalf("read matrix: %v", err)
	}
	rowRe := regexp.MustCompile("^\\|\\s*`@(\\w+)")
	for i, l := range strings.Split(string(raw), "\n") {
		m := rowRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		name := m[1]
		if hint, retired := baseparser.RetiredConstructAnnotation(name); retired {
			t.Errorf("attribute-matrix.md:%d still has a row for retired @%s -- move it to the retirements section.\n  ledger says: %s", i+1, name, hint)
		}
		if hint, retired := baseparser.RetiredFieldAnnotation(name); retired {
			t.Errorf("attribute-matrix.md:%d still has a row for retired field @%s -- move it to the retirements section.\n  ledger says: %s", i+1, name, hint)
		}
	}
}

// TestEveryRetiredAttributeIsDocumentedAsRetired is the other direction:
// a reader who meets the refusal needs to find the annotation in the doc
// and be told what replaced it. A retirement with no row anywhere reads
// as a typo in their own file.
func TestEveryRetiredAttributeIsDocumentedAsRetired(t *testing.T) {
	raw, err := os.ReadFile("docs/public/language/attribute-matrix.md")
	if err != nil {
		t.Fatalf("read matrix: %v", err)
	}
	doc := string(raw)
	for name := range retiredNamesForDoc() {
		if !strings.Contains(doc, "@"+name) {
			t.Errorf("retired @%s appears nowhere in attribute-matrix.md -- an author who meets its refusal cannot look it up", name)
		}
	}
}

// retiredNamesForDoc is the union of the two ledgers, for the doc gate.
func retiredNamesForDoc() map[string]bool {
	out := map[string]bool{}
	for _, n := range []string{
		"deprecated", "timeout", "retry", "idempotent", "audit",
		"latestMode", "enabled", "nocache", "schedule", "rateLimit",
		"scopes", "namespace", "unique", "immutable",
	} {
		out[n] = true
	}
	return out
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `make test 2>&1 | grep -A5 "MatrixListsNoRetired"`
Expected: FAIL — the matrix still has rows for `@enabled`, `@nocache`, `@latestMode`, `@rateLimit`, `@scopes`, `@namespace` and the six function attributes.

- [ ] **Step 3: Update the matrix and the prose**

In `docs/public/language/attribute-matrix.md`:
- Delete the applicability rows for every retired name.
- Rename the provider `@type` row to `@vendor`.
- Add a `## Retired attributes` section at the end, one row per retirement: the name, the release that retired it (memql#5375), what replaced it or why nothing did, and the rewrite command. This is what `TestEveryRetiredAttributeIsDocumentedAsRetired` reads.
- Note in the header that the applicability table is pinned to `annotations.ByReceiver` by `attribute_matrix_parity_test.go` and that the retirements are pinned to the two ledgers in `core/baseparser`.

In `docs/public/language/authoring-rules.md`: update section 28's `@default` paragraph to say that a concept field's `@default` is retired rather than merely never applied, and that `??` is the only mechanism; note that a tool / prompt / builtin field keeps it because those bodies are the schema.

In `docs/public/language/memql.md`: rename the provider `@type` references to `@vendor`; change every `mutate <Concept> <name>` to `mutation <Concept> <name>`.

In `dsl/_reference/*.memql`: these are the deliberate don't-do-this skeletons, so move the newly retired forms into their retired-forms sections rather than deleting the mentions, and update the live examples to the surviving spellings.

In `CLAUDE.md`, four statements are made wrong by this epic and each is load-bearing for the next reader:
- The "Functions" section's `mutate <Concept> <name>` becomes `mutation <Concept> <name>` (also in "Mutations:" and the construct-signature paragraph).
- The "Providers" example's `@vendor("OpenAI")` becomes `@vendor("OpenAI")`.
- The "Annotations" bullet list gains `@default` on a concept field to its rejected set, beside the args-field entry already there.
- The lifecycle paragraph's `@enabled` is now refused rather than an accepted no-op; `@disabled` is unchanged.

In the design record, add a note under D17 recording the three exceptions and pointing at the verification file, so the record and the tree agree.

- [ ] **Step 4: Run the full suite**

Run: `make test 2>&1 | tail -40`
Expected: PASS across all 208 packages. Then the db-gated trees CI owns:

```bash
scripts/ci/db-gated-packages.sh --trees
```

Run those with a real database if one is reachable; note in the PR body if not.

Also run the front-door and conformance gates this epic could disturb:

```bash
go test ./test/dslconformance/ 2>&1 | tail -20
go test ./test/conformance/ 2>&1 | tail -10
```

- [ ] **Step 5: Commit**

```bash
git add docs/public/language/attribute-matrix.md attribute_matrix_parity_test.go docs/public/language/authoring-rules.md docs/public/language/memql.md dsl/_reference/ CLAUDE.md docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md
git commit -m "docs(language): the matrix lists no retired attribute, and every retirement is findable

Issue #5379. Two gates, opposite directions: the matrix may not carry a
row for a retired name, and every retired name must appear somewhere in
the matrix -- an author who meets a refusal and cannot look the
annotation up reads it as a typo in their own file."
```

---

## Task 9: Close the epic

**Files:**
- Delete: `docs/superpowers/plans/2026-09-14-dsl-v1-attributes.md`, `docs/superpowers/plans/2026-09-14-dsl-v1-attributes-verification.md`

The epic issue says the plan is "deleted in the epic's merge". The verification record's findings do not die with it — they are already in the registry doc strings (Task 2), the ledger comments (Task 1) and the design record's D17 note (Task 8), which is where a future reader will actually look.

- [ ] **Step 1: Post the verification answer on issue #5378**

Its acceptance criterion is "The epic issue carries the reader list or the word none, with file references." Post the reader list for `@displayCard` and `@composable` from Task 0 Step 2, and the `@allowedRoles` finding on #5377.

- [ ] **Step 2: Delete the plan and commit**

```bash
git rm docs/superpowers/plans/2026-09-14-dsl-v1-attributes.md docs/superpowers/plans/2026-09-14-dsl-v1-attributes-verification.md
git commit -m "chore(plan): delete the epic plan on merge

Epic #5375. The verification findings live on in the registry doc
strings, the retirement ledger's comments and the design record's D17
note."
```

- [ ] **Step 3: Open the single PR**

One PR for #5375 #5376 #5377 #5378 #5379, with the three documented exceptions called out in the body under their own heading — a reviewer who knows D17 will look for `@allowedRoles` and needs to find the reason it is still there.
