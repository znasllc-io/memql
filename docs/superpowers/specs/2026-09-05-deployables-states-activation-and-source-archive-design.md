# Deployables: one vocabulary, activation, and a source archive that frees its names -- Design

- **Date:** 2026-09-05
- **Status:** built in the same session it was written, from the owner's
  walkthrough of the app on a live cluster. Every item below is a defect the
  owner reported or a decision they stated; the vocabulary table is the one
  thing here that is a recommendation rather than a transcription, and it is
  held in one module so a word can be changed without touching a flow.
- **Scope:** `clients/os/src/apps/deployables/**`, `component/packages`,
  `component/memql` (one write guard, two read builtins), `dsl/platform`.
- **Extends:** [2026-09-04-deployables-recomposed-design.md](2026-09-04-deployables-recomposed-design.md)
  (the bar, the four views, the delete rung). Its D1 -- a standalone
  deployable's archive keeps the name and Delete releases it -- stands.

---

## What the owner found

Walking the compose flow on a real cluster, in order:

1. **Where it lives seeded the address with the app's name** (`storefront`,
   `web`). The Generate button existed; the seed did not use it.
2. **Skip did not deactivate.** Skipping `web` left it "off" on the list, but
   clicking the row turned it back on with no page and no confirmation, and
   the next click started a deploy.
3. **The flow for one skipped app asked about every app.** Opened for `web`,
   Where it lives rendered `storefront` too, with a Deploy/Skip pill on each,
   and the bar read "2 of 2 apps". The scope reached the wire (every other app
   was sent `skip: true`) and never reached the form.
4. **Three words for one state.** After Deploy the bar read `Deployed`, then
   `Published 1 app`, then `It is not serving yet`. Deploy sounded final,
   Published sounded live, and neither was.
5. **Archiving a source did not free its names.** The cascade archived each
   site (`status: archived`); the uniqueness probe reads `deleted` alone. A
   re-added source could not take the addresses the archived one still held.
6. **A source could not be archived while an app was live**, and the owner
   wants it to be -- with a warning that names what will be torn down.
7. **Every confirmation asked for the full hostname.** `storefront.memql.<domain>`
   typed out, for every archive and delete.
8. **The same source could be added twice** (same repository, same ref).
9. **A taken name was discovered at the end of the flow**, on Deploy, rather
   than while typing or generating it.

---

## Decisions

**D1. One vocabulary, in one module.** `words.ts` holds every state word and
its one-clause meaning; the bar, the list, the source page, the rail and the
compose flow read it. The words:

| Engine | Word | Meaning |
|---|---|---|
| declared, on the source's off-list, no site | **Inactive** | skipped when the source was deployed; nothing built, no address |
| declared, not on the off-list, no site | **Not deployed** | the source declares it; deploying it is what asks for an address |
| `draft`, placeholder bundle | **Not deployed** | nothing has been deployed here yet |
| `draft`, real bundle | **Built** | its files are in place at the address; not live until you say |
| `live` | **Live** | serving at the address |
| `disabled` | **Offline** | taken offline on purpose; visitors get "temporarily unavailable" |
| `archived` | **Archived** | filed away; answers nothing; the name is still held |
| `deleted`, domains coming down | **Deleting** | releasing the address |

The verbs: **Analyze**; **Deploy** (and *Deploy the update*, *Redeploy*,
*Retry the deploy*), which builds the source and puts its files in place and
never changes whether the app is live; **Go live** / **Take offline**;
**Activate** / **Deactivate** for a source's app; **Archive** / **Restore** for
a source or a standalone; **Delete** (and *Discard* for a draft) for a
standalone; **Cancel** for a run; **Skip** at the gate.

`Publish`, `Unpublish`, `Published`, `Unpublished`, `Make it live`, `Paused`
and `off` are gone from every surface. The enum values do not move.

**D2. A source's app is never archived; it is deactivated.** Deactivate is one
capability, `packageDeactivateDeployable`: release the app's custom domains,
delete its site (the name is free at that write), disarm the source's
auto-deploy when this was its last app, and put the name on the source's
off-list. Offered from every state the app can be in, live included, behind a
typed confirmation that says what goes offline. Activate is the inverse's
first half: the app comes off the off-list and the compose flow opens for it,
scoped to it, asking only where it should live.

**D3. Archiving a source deactivates every app it produced, then archives the
source.** No refusal while an app is live: the confirmation names the live
addresses and says they go offline the moment it is confirmed. `packageRestore`
still does not cascade; the apps come back inactive, to be activated and
deployed at fresh addresses. `package_has_active_deployables` is retired.

**D4. A standalone deployable keeps the recomposition's D1.** Archive holds the
name and is reversible; Delete releases it. A source's apps are reproducible
from the source, a standalone's bundle is its only copy, and that is why the
two archives differ.

**D5. Skip is deactivate.** An app skipped at the gate lands on the off-list
exactly as before; what changes is what a click on it does: it opens the
compose flow for that app with a notice at the top saying it is inactive and a
bar offering Cancel and Activate. Nothing writes until Activate.

**D6. A click never acts.** A declared row that is not inactive opens the
same flow with Analyze on the bar rather than starting the analysis from the
list. A parked run scoped to one app reopens the flow scoped to that app.

**D7. The address is checked while it is typed, and a generated one is
checked before it lands.** Two engine-native read builtins, `siteHostnameCheck`
and `customDomainCheck`, run the same shape and uniqueness rules the write
guards run and answer `{available, problem}` without naming who holds the
name. The Where-it-lives stop calls them on a short debounce and shows
"checking", "available" or the server's sentence; Generate draws again until
a free name comes back; Deploy is out of reach until every address checked
out. The write guards are unchanged and still decide.

**D8. The same source cannot be added twice.** A write guard beside the
hostname policy refuses a `v1:platform:package` create (or a source edit)
naming a repository and ref another ACTIVE package already tracks, cluster-wide
and normalised (scheme, `.git`, trailing slash, case; an empty ref is the
default branch). Archived packages do not count, so a source can be archived
and added again. The compose Source stop asks the same question off the
packages feed the app already holds (`duplicateSource`), reading `""` and the
probe's default branch as one ref, and says which source already tracks it
before Analyze.

**D9. A confirmation is the thing's name.** A standalone types the address
label (`storefront`, not `storefront.memql.<domain>`); the server accepts the
label or the whole hostname. A source's app types the app's name. A source
types its own name, as before.

**D10. The compose flow ends on Built, with Go live on the bar.** The first
deploy leaves the site `draft` with a real bundle, deliberately (a stranger's
code should not go live the moment it builds). The bar says so in the new
words and offers Go live beside Done, so the two-step is one screen.

---

## What was verified

- The uniqueness probe (`liveSiteIdsForHostname`) excludes `deleted` only; the
  archive cascade wrote `status` only. Read to the last line.
- The delete-path guard refuses `systemOwned` and nothing about status, so a
  cascade may delete a live site at the write path.
- `sitesForPackage` carries `isNotDeleted`, so a deactivated app's next deploy
  creates a fresh site rather than finding the deleted one.
- The compose flow's `only` reached `placementAddresses` and the title, and not
  `apps`.
- `createPackage` runs no cross-row check of any kind.

---

## The rendered pass

DESIGN.md makes rendered screenshots the acceptance for a surface change, so
the real `DeployablesApp` was mounted over the suite's own fake connection in
a throwaway Vite harness (the recipe the recomposition's visual QA records:
the connection module swapped on its RESOLVED path, `vi.fn` stubbed so the
suite's fixtures drive the browser, the role ladder seeded) and driven in a
real Chrome at 1500 x 1000, both themes. The harness was deleted afterwards.

| Scene | What was checked |
|---|---|
| The list | the source group with `storefront` at a generated address and `web` reading `inactive`; standalones beneath |
| An inactive app, opened | the notice at the top, the Source stop answered, every later stop pending, the bar reading Inactive with Cancel and **Activate** |
| Where it lives, for one app | What it is and Where it lives name `web` alone; no Deploy/Skip pill; a generated address with "-- free" beneath it; the bar's "deploying puts its files in place; going live is the step after" |
| A taken name | typed `web`: the policy's own sentence in amber, Deploy absent, the bar saying there is more to answer |
| The finished flow | every stop settled, Live reading "Built. In place at ..., not live yet.", the bar reading **Built** with Go live beside Done |
| A built app's page | the bar reading Built, with Deactivate and Go live |
| A live app's page | Live, serving at the address, Take offline and Redeploy |
| An offline app, deactivating | the confirmation naming the app and the address, asking for the app's NAME, Deactivate held until it is typed |
| The source page | apps chipped `live` and `inactive`, the inactive caption, and the archive confirmation naming the live address in bold and asking for the source's name |
| An offline standalone, archiving | the confirmation saying the name stays and Delete releases it, asking for the label `shop` |
| Light theme | the placement stop and the source archive, legible in the porcelain set |

One defect the pass caught that the suite had not: the source archive
confirmation named the apps by hostname; it names them by their manifest names
now, which is what the sentence beside it calls them.
