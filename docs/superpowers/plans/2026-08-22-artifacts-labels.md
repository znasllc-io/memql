# Artifacts page + labels — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development.

**Goal:** Artifacts get labels the owner and their agents can add and remove, and the portal gets a page to see and filter them.

**Spec:** `docs/superpowers/specs/2026-08-22-artifacts-page-and-labels-design.md`

## Global Constraints

- **Never call MemQL a database.** `TestNoDatabaseProductClaims` fails the build.
- **No emojis** anywhere — docs, code comments, UI copy, commit messages.
- **Test with `make test`**, never `go test ./...` (it misses the engine's modules).
- **Stage files by explicit path** (`git add <file>`). Never `git add -A` or `.`.
- **Per-row authz:** every new query/mutation must classify. `TestPerRowAuthzClassification` requires FLAG=0. The owner conjunct (`ownerUserId==actor.userId`) must be a genuine TOP-LEVEL conjunct — never only inside a `when()` guard, never on one arm of a `||`.
- **Filter syntax:** payload fields bare; row intrinsics under `row.`; `&&`/`||`/parens only; `??` for defaults (`@default` on an args field is rejected at load); describe an args field with a `///` doc comment, not `@description`.
- **The field is named `labels`**, and its `@description` must state the distinction from `worker.labels` / `cluster.labels` (which are `object` key=value maps for machine routing).
- **A builtin is reached from a tool through `@handler(type="function", name="<builtin>")`.** There is no `"builtin"` handler type.
- Portal: no raw button/input class strings outside `src/ui/`. Every destination is a URL, including drill-in and the active filter.

---

### Task 1: The DSL surface for labels

**Files:**
- Modify: `dsl/library/concepts.memql` (concept `artifact`)
- Modify: `dsl/library/mutations.memql` (`createArtifact`)
- Modify: `dsl/library/shapes.memql` (`artifactFull`)
- Modify: `dsl/library/queries.memql` (new query)

**Produces:** field `artifact.labels []string`; arg `labels` on `createArtifact`; `labels` in `artifactFull`; query `libraryArtifactsByLabel(label)`.

- [ ] **Step 1: Add the field.** On `concept artifact`, after `scope`, add:

```memql
  labels           []string  @description("Free-text labels the owner or their agents put on this artifact to say what it was for. Rendered as chips in the portal Artifacts page and filterable one label at a time. Distinct from v1:worker:registration.labels and v1:cluster:node.labels, which are key=value OBJECT maps for machine routing -- these are display labels on a []string, matching note.tags and skill.tags.")
```

- [ ] **Step 2: Thread it through `createArtifact`.** Add `labels []string` to the args block (optional — no `!`), and add `labels` to whatever accept/insert list the mutation uses, following exactly how `scope` is threaded in the same mutation.

- [ ] **Step 3: Add it to `artifactFull`.** One bare `labels` path line, placed with the other payload fields.

- [ ] **Step 4: Write the facet query** in `dsl/library/queries.memql`, modelled on `libraryArtifactsByKind` for structure and on `dsl/notes/queries.memql:42` (`notesByTag`) for the membership predicate:

```memql
/// List the caller's artifacts carrying a given label. Owned: the owner conjunct is top-level and
/// unguarded, so it holds on every path -- a caller-scope term that sits only inside a when() guard
/// is dropped when the arg is absent and is flagged by the per-row authz classifier.
@actor
query artifact libraryArtifactsByLabel {
  args {
    /// The single label to narrow to.
    label  string!
  }
  filter  ownerUserId==actor.userId && when(args.label) { args.label in labels }
  shape   artifactFull
  sort    "updatedAt", "desc"
  paginate 50
}
```

Match the surrounding file's exact annotation set and sort/paginate spelling — copy from `libraryArtifactsByKind` and change only what differs.

- [ ] **Step 5: Verify it loads and classifies.**

Run: `cd <repo> && go test ./test/dslconformance/... -run 'TestPerRowAuthzClassification|TestFilter|TestSort' -count=1`
Expected: PASS, FLAG=0.

Then: `go build -o /tmp/memqllint ./cmd/memqllint && /tmp/memqllint` (or the documented invocation) to confirm the tree still loads.

- [ ] **Step 6: Regenerate the SDK.**

Run: `make sdk-gen` then `make sdk-gen-check`
Expected: `sdk-gen: no drift`. The generated TS/Go client gains the new query.

- [ ] **Step 7: Commit** (explicit paths only — include the regenerated SDK files).

---

### Task 2: Add and remove, without clobbering — and the label-loss fix

**Files:**
- Modify: `integrations/library/library.go`
- Modify: `dsl/library/builtins.memql`
- Create: `dsl/library/tools.memql`
- Test: `integrations/library/library_test.go` (create if absent)

**Consumes:** Task 1's `artifact.labels` field and `createArtifact`'s `labels` arg.

- [ ] **Step 1: Write the failing regression test for the label-loss hazard.**

This is the spec's named hazard (D3). `touchArtifact` (`integrations/library/library.go:437`) re-versions the artifact row by re-calling `createArtifact` with a fixed argument list; MemQL is insert-versioning, so the new version carries only the named fields. Adding `labels` without touching this function makes every document edit silently drop the artifact's labels.

Write a test that labels an artifact, runs the document-edit path, and asserts the labels survived. It MUST fail before Step 4.

- [ ] **Step 2: Run it and watch it fail.** Record the failure message in your report.

- [ ] **Step 3: Implement the two capabilities.** In `integrations/library/library.go`, following the existing `handleEditDocument` shape exactly (load under the system actor via `systemActorContext`, act under a synthetic owner actor via `withUserActor` derived from the row's own `ownerUserId`):

  - `addArtifactLabel(artifactId, label)` — load the artifact row, merge the label into `labels` if absent, write back.
  - `removeArtifactLabel(artifactId, label)` — load, drop the label if present, write back.

  Both are **idempotent**: adding a label already present and removing one absent both succeed and change nothing. A blank label is refused.

  Register them alongside the existing capabilities.

- [ ] **Step 4: Fix `touchArtifact`.** Read the current row's `labels` and pass them through the `createArtifact` call so a re-version preserves them. Quote through `langparser.QuoteString` exactly as the neighbouring fields do — never hand-interpolate.

- [ ] **Step 5: Run the regression test and watch it pass.**

- [ ] **Step 6: Add the DSL builtins** in `dsl/library/builtins.memql`, matching the file's existing three:

```memql
@enabled
@description("Add a label to an artifact. Idempotent -- a label already present is left alone.")
@executor("integration.library.addArtifactLabel")
builtin libraryAddArtifactLabel {
  artifactId  string  @required
  label       string  @required
}
```

and the mirror `libraryRemoveArtifactLabel` on `integration.library.removeArtifactLabel`.

- [ ] **Step 7: Add the agent tools** in a new `dsl/library/tools.memql`. Model the annotation set on `dsl/agents/tools/editDocument.memql`. The handler reaches a builtin through the **`function`** type:

```memql
@enabled
@description("Put a label on an artifact so the person can tell later what it was for.")
@handler(type="function", name="libraryAddArtifactLabel")
@allowedRoles("assistant", "specialist")
@executionTime("fast")
tool artifactAddLabel {
  artifactId  string  @required  @description("Id of the artifact to label.")
  label       string  @required  @description("The label to add. Short and human-readable -- a project or topic name, not a sentence.")
}
```

and the mirror `artifactRemoveLabel`. Handler targets resolve at load, so a typo refuses boot.

- [ ] **Step 8: Test both capabilities directly** — add idempotency both directions, and that labelling an artifact the caller does not own is refused.

- [ ] **Step 9: Verify.** `make test`, plus `go test ./test/dslconformance/... -count=1`, plus `make sdk-gen && make sdk-gen-check`.

- [ ] **Step 10: Commit** (explicit paths).

---

### Task 3: `LabelChips` — the one new design-system piece

**Files:**
- Create: `clients/portal/src/ui/LabelChips.tsx`
- Modify: `clients/portal/src/ui/index.ts` (export it)
- Test: `clients/portal/test/labelChips.test.tsx`

**Produces:** `<LabelChips labels={string[]} onAdd={(l:string)=>void} onRemove={(l:string)=>void} busy?:boolean readOnly?:boolean />`

- [ ] **Step 1: Write the failing test.** Cover: renders one chip per label; the remove control calls `onRemove` with that label; the add input calls `onAdd` with the trimmed value and clears; a blank or whitespace-only entry calls nothing; a duplicate calls nothing; `readOnly` renders chips with no controls. Use `@testing-library/react` and follow the structure of an existing `clients/portal/test/*.test.tsx`.

- [ ] **Step 2: Run it, watch it fail.**

- [ ] **Step 3: Implement.** Build on the existing `Badge` primitive for each chip. All classes come from `src/ui/` conventions — a raw button/input class string outside `src/ui/` is a defect, and this file IS `src/ui/`, so it is where they belong. Colors go through `--memql-*` roles; no `dark:` variants (theme resolves through `brand/theme.css`).

- [ ] **Step 4: Run it, watch it pass.**

- [ ] **Step 5: Verify.** `cd clients/portal && npm run typecheck && npm test && npm run build`. The build matters independently — vitest has passed while the build was broken.

- [ ] **Step 6: Commit.**

---

### Task 4: The Artifacts page

**Files:**
- Create: `clients/portal/src/artifacts/{ArtifactsRoutes,ArtifactsPage,ArtifactDetailPage}.tsx`, `urls.ts`, `concepts.ts`, `useArtifacts.ts`
- Modify: `clients/portal/src/app/routes.tsx` (mount the splat), `clients/portal/src/app/AppShell.tsx` (nav entry)
- Test: `clients/portal/test/artifacts.test.tsx`

**Consumes:** Task 1's query, Task 2's builtins (through the generated TS client), Task 3's `LabelChips`.

- [ ] **Step 1: Write the failing test** against a fake connection, modelled on `clients/portal/test/modules.test.tsx`: the list renders artifacts; the label filter narrows and puts the label in the URL (`?label=`); the detail route renders at `/artifacts/:artifactId`; adding a label calls the builtin; removing calls the other; the empty state appears when there are none; an error state appears when the read fails.

- [ ] **Step 2: Run it, watch it fail.**

- [ ] **Step 3: Implement, copying `clients/portal/src/sites/` structure exactly** — routes splat, list page, detail page, `urls.ts`, hooks over `useConceptRows(ARTIFACT_CONCEPT_ID)`. Render rows with `RowList` off `@displayCard`; do not hand-render a row. Follow the sites pages' loading/empty/error ladder verbatim in shape (`ErrorMessage` / `Skeleton` / `EmptyState` / content).

  The create affordance mints a `generatedOutput` (title, summary, markdown body) — the existing `indexGeneratedOutputOnCreate` automation promotes it into an artifact. Do NOT write a bare artifact index row: `sourceConceptRef` is the idempotency key and a row with nothing behind it renders as broken.

  Add one nav entry in `AppShell.tsx` alongside the other manually-added `NavItem` entries.

- [ ] **Step 4: Run it, watch it pass.**

- [ ] **Step 5: Verify.** `npm run typecheck && npm test && npm run build`, and `go test . -run TestPortal -count=1` from the repo root for the portal render-path and view-composition guards.

- [ ] **Step 6: Commit.**

---

### Task 5: Docs

**Files:**
- Modify: `docs/public/operate/portal.md` (the Artifacts page)
- Modify: whichever Library-facing doc exists (find it; if none, say so in your report rather than inventing a home)

- [ ] **Step 1:** Document the page: what it lists, that labels are editable by the owner and by their agents, that the filter is one label at a time and lives in the URL, and that creating from the portal mints a generated output which the Library indexes.
- [ ] **Step 2:** State the `labels` vs `worker.labels` distinction once, where a reader will hit it.
- [ ] **Step 3:** `go test .` (root guards include the docs gates). Commit.
