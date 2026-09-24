# The browser QA harness

`clients/os/DESIGN.md`: **"the acceptance for any surface change under these
rules is rendered screenshots, both modes, empty and populated -- not the
diff."** jsdom performs no layout, resolves no custom property and never puts a
value beside its own label, so the suite can be entirely green over a surface
whose columns collide, whose action bar sits below the fold, or whose sentence
disagrees with the number in it.

This is that pass, for the storefront's Store surface (epic memql#5530) and
for the two Deployables lists and the add wizard that sits behind them.

## Running it

```bash
cd clients/os
npx vite --config qa/vite.config.ts --port 5199 --strictPort
```

Then capture. A one-shot headless screenshot is enough for any view that needs
no click, and every view here is reachable by URL for that reason:

```bash
google-chrome --headless=new --disable-gpu --no-sandbox --hide-scrollbars \
  --window-size=1400,900 --virtual-time-budget=12000 \
  --screenshot=store.png "http://localhost:5199/?view=store&mode=dark"
```

`view` is one of `fleet-healthy`, `fleet-expiring`, `fleet-expired`,
`fleet-skewed`, `fleet-silent` (the machine detail's connection health, epic
memql#5327 -- read `fleet-healthy` FIRST, because the design is that these
warnings are absent almost always, and a capture of an unbroken machine is
what says the page has not become a wall of advisories), or `overview`,
`store`, `quiet`, `picker`, `readonly`, `hidden`
(the Store surface), or `list`, `sources`, `list-empty`, `sources-empty`,
`connected`, `connected-empty`, `source-chooser`, `github-owner`, `github-member`, `settings-no-app`,
`settings-no-app-member`, `settings-app` (the Deployables app, whole), or
`origins-silent`, `origins-mixed`, `origins-reporting` (Data origins); `mode`
is `dark` or `light`. **Take at least one narrow capture** (`820,760`): two of
the first three real defects this harness found were invisible at 1400x900.

`&open=1` opens every `<details>` on the page once the reads have landed. A
one-shot capture cannot click, and a facts list -- the densest thing on a
machine detail, and the place a long value runs past its own label -- sits
behind a shut `<details>`. Both defects the `fleet-*` views found were in
content that needed it.

The list views seed ONE OF EVERYTHING: a deployable of every origin (a named
source, an uploaded zip, a Library zip, CI, built in, none) and a source in
every state (review needed, update available, current, nothing deployed,
archived). A development cluster has two built-in deployables and no source,
so until these views existed neither list had been seen with the rows it was
designed for. `connected` is the same app with a GitHub account connected.

The `github-*` and `settings-*` views are the cluster's GitHub App in each
reading a surface has of it: a cluster with none seen by somebody who may
register one (`github-owner`, `settings-no-app`) and by somebody who may not
(`github-member`, `settings-no-app-member`), and one registered from the
product with an account connected through it (`settings-app`). For the two
`github-*` views press + and choose "A repository". Their first rendered pass
found what the suite could not: the group's own sentence still opening with
"Connect GitHub" directly above the part saying it cannot work, and a bare
Remove that did not say of what.

The three `origins-*` views are the connector-coverage band (issue
memql#5574), and the middle one is the one that matters. `origins-silent` is
the production state the issue describes -- eight declared concepts and nothing
reported, which before the band was a page of eight individually-correct em
dashes saying nothing about the connector. `origins-mixed` puts a silent
connector BESIDE a healthy one, because the question the band exists to answer
is whether the one line worth finding is findable, and a page with one line on
it cannot answer that. `origins-reporting` is a cluster with nothing wrong,
where the band has to be quiet enough that nobody learns to scroll past it.

They mount over `test/cluster/harness.tsx` rather than the Deployables fake --
two fixture harnesses rather than one widened one, so each stays the SUITE's
and a screenshot cannot disagree with what those tests assert.

Their first rendered pass found what 3,474 green cases could not: the
per-connector ERROR COUNT was drawn in error ink, so red on a connector whose
concepts had all reported outshouted the warn on a connector that had reported
NOTHING -- the eye landed on the lesser reading first. The count is now in the
quiet voice and the error itself stays in its own row, which is where the
sentence explaining it already lived.

## What it is, and what it is not

It mounts the **real components** over the **suite's own** fixture connection
(`test/deployables/harness.tsx`), so a screenshot cannot disagree with what the
tests assert. `vitest`'s `vi` is shimmed for the same reason: a second copy of
the fake is a second thing to keep in step.

**It does not click.** The Store pane is reached in the product by clicking the
workspace's Store slot, which needs React's synthetic event system and so a
real driver. Each view is rendered directly here, in the wrapper the page gives
it. These captures judge LAYOUT, COLOUR, COPY and DENSITY; the navigation that
reaches them is `test/deployables/store.test.tsx`'s.

The Deployables views mount the WHOLE APP inside the window's own body (the
trail row, then the content, as `chrome/WindowFrame` composes them), so a
headless capture shows the list and a real browser can go on from it: a
source's page, the add wizard, the Repository step as a picker. A wizard needs
that bounded body -- its floor is only at the bottom of something that has one.

## Three things it gets wrong by default, all silent

1. **The connection swap needs `enforce: "pre"`, on the RESOLVED path.** Vite's
   own resolver is a `pre` plugin, so a plain `resolveId` never sees the id; and
   `src/live/connection` is imported both as `"../../../live/connection"` and as
   `"./connection"`, so any `endsWith` match on the specifier catches one and
   misses the other. Missing one means the app holds a second, empty seam and
   every live list says "Not connected to the cluster".

2. **`test/seededAccess.ts` reads `dsl/rbac/seeds.memql` with `node:fs`, at
   module load.** In a browser that is an unresolved import rather than a thrown
   error, so the page is blank and `window.onerror` is silent. The config
   inlines the seeds text on the Node side and leaves the module's own parse
   alone -- a browser copy of "which role holds what" would be a second copy of
   the policy in a second language.

3. **The providers are the suite's.** `withSession` installs the session, the
   shell provider and the role's seeded capability set together; `useOs` throws
   outside its provider, and without the role ladder the whole app renders
   read-only with nothing saying why.

## What it found

On the Store surface, three defects, all invisible to 3,344 green cases: a pane
that painted 800px of dead space beside a 600px stripe (DESIGN.md rule 9), a
sentence that disagreed with its own number ("1 of the 3 scopes ... are not
granted"), and a choice label with nested parentheses.

On the two lists, the first time they were rendered with real rows:

- **Five sources seeded, three listed.** The Sources list was a filter over the
  deployables fold, where a source exists only if it has a row -- so a source
  that had made nothing was nowhere, a search trimmed what a source was said to
  have made, and one archived app filed its active source under archived.
- **A ragged middle column.** Each row is its own grid and its state track was
  `auto`, so where a deployable came from started at a different x on every
  line (five different x down a list of eight). Invisible on a cluster whose
  rows all say "Live".
- **The rail lit the wrong step.** With a repository chosen the stage said
  "Repository" and the rail's accent was on "Review".
- "uploaded zip" on one tab and "Uploaded zip" on the other, for the same thing.


## All-app record lists

The `accounts` and `accounts-empty` views compare the Accounts registry with
`list` (Deployables). `origins-mixed` exercises records with separate actions
and absent measurements. Check light and dark modes at wide and narrow widths.
The full app inventory and inspected exceptions are in [list-coverage.md](list-coverage.md).

## GitHub repository creation

Use `connected` for a populated repository picker and `connected-empty` for a
connected GitHub account whose completed read returns no repositories. In either
view, open Add deployable and choose A repository. Check desktop and narrow
layouts with both `mode=light` and `mode=dark`. These use deterministic fixture
connections and do not mint credentials or modify a real account.

`source-chooser` supplies multiple GitHub identities, personal/org bindings and repositories. Open Add a deployable, choose A repository, then Add source. Select GitHub account, Continue, select organization/personal account, Continue, select repository, Continue. Configuration holds MemQL ownership afterward. Confirm each stage shows only its own actions, top-left list refresh, a selected-row cue and a footer Continue/Back; the progress rail must identify the visible step. Organization Continue saves access; Analyze creates the repository. Check per-source removal and cancellation, account cue placement, repository/ref clearing on Source change, keyboard navigation, desktop/narrow layouts and both themes. `source-management` and `repository-management` open the management pages; `source-settings` checks the shared Settings surface. All three have purpose subtitles and no creation control. `source-empty` directs creation through Add deployable without another CTA. Confirm the wizard still offers plus-style Add source and Add GitHub account, and newly saved bindings appear in the live list.

## Analysis progress

`analysis-pending`, `analysis-failed`, and `analysis-review` exercise the real
add-deployable wizard over simulated source requests. Choose A repository,
@octocat, acme, and field-notes, then Analyze. Configuration should remain
current with the shared busy action bar and elapsed time; a completed report
opens Review without a deployment broadcast. Failure offers Retry. No fixture
contacts GitHub or starts a real deployment.
