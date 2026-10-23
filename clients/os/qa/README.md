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

Add `theme=graphite`, `theme=vellum`, or `theme=cobalt` to inspect each built-in
palette with `mode=light` or `mode=dark`. The harness uses the production theme
registry; Graphite is the default. `view=routing-routes` includes the shared
local tabs used by Fleet machine views and other app pages.

Fleet > Routing > Routes has a view per state: `routing-routes` (also
`-member` and `-old-cluster`), `routing-route-local` (also `-member`),
`routing-route-busy` (capacity reasons), `routing-route-mine`,
`routing-route-no-embedder`, `routing-auto` (also `-member`) and
`routing-new-route`, driven with `click=New route|fill:Route name=...|Continue|Local`.
The chain composer under Rules is `routing-chains`.

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
`origins-silent`, `origins-mixed`, `origins-reporting` (Data origins), or
`mesh-healthy`, `mesh-island`, `mesh-empty`, `mesh-node-bff`, `mesh-node-edge`,
`mesh-node-identity`, `mesh-node-unreported` (Cluster > Mesh), or
`sharing-owner`, `sharing-people`, `sharing-paused`, `sharing-both`,
`sharing-legacy-descriptor`, `sharing-everyone`,
`sharing-viewer`, `share-dialog`, `share-dialog-admin`, `share-dialog-empty`,
`share-dialog-refused` (a machine's Sharing view and its dialog), or
`nexus-procedure-evidence`, `nexus-procedure-overridden`,
`nexus-procedure-unlabelled`, `nexus-automations`, `nexus-automation-detail`,
`nexus-overview` (what an automation is for, epic memql#5414: a learned
procedure's Reuse panel with the evidence deciding, with a person's own label
over evidence that disagrees, and before anything was counted; the list with
every label on it; an authored automation's detail; the Overview's ratio),
or `pipeline-runs`, `pipeline-runs-empty`, `pipeline-runs-none`,
`pipeline-run-failed`, `pipeline-run-running`, `pipeline-run-queued`,
`pipeline-run-passed`, `pipeline-run-refused`, `pipeline-run-cancelled`,
`pipeline-run-rerun`, `pipeline-run-developer` (Pipelines > Runs and a run
page in each state, epic memql#5479; the seeds are `qa/pipelinesSeeds.ts`, and
`pipeline-run-failed` answers the Library content route with a Go test's last
lines so the log tail is judged with a real failure in it), or
`pipeline-source`, `pipeline-source-none`, `pipeline-map` (a source's checks,
a source with no pipeline, the Overview map's Checks nodes) and
`settings-pipelines-unset`, `-partial`, `-done`, `-unreported`
(Pipelines settings in each reading), or
`add-machine-describe`, `add-machine-waiting`, `add-machine-runtime`,
`add-machine-pulling`, `add-machine-failed`, `add-machine-refused`,
`add-machine-ready`, `add-machine-tools-ready` (Fleet's Add a machine in each
phase, from a fixture flow: every reading is a function of facts, so a capture
of a phase is exactly that phase), `setup-ownership` (taking ownership of a
fresh cluster) and `setup-gate` (the first-run inference choice, memql#6214);
`mode` is `dark`
or `light`. **Take at least one narrow capture** (`820,760`): two of
the first three real defects this harness found were invisible at 1400x900.

`&click=A|B` presses the control whose text or accessible name is A once the
reads have landed, then B a beat later: `pipeline-source&click=Pipeline
settings` is the pipeline page, `pipeline-source-none&click=Connect pipeline`
the connect rail, and `...|Continue` its Compute stop. `element.click()` is a
real click event, so React handles it like a person's.

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

The `mesh-*` views are Cluster > Mesh (epic memql#5338) over a sixteen-pod
cloud-shaped cluster. `mesh-island` is the one that matters: a product bff and
an edge that have heard nothing in three hours, a planner gone quiet, an mcp
on an older release that has not reported, and identity sending only -- every
state the section distinguishes, on one page. The `mesh-node-*` views are the
node page in each reading, including a bff's full patch bay (take it narrow as
well: under 560px the two columns stack) and identity's, which sends and hears
nothing.

Their rendered passes found what the green suite could not: the sentence named
the quiet planner in the same breath as the two deaf nodes, hid the deaf edge
behind "and 1 other", and broke a node id at its hyphen across two lines; a
node with no report was dated "Report written"; identity's panel was titled
"What it hears"; and a three-hour silence read "180 minutes".

The `sharing-*` views exercise the inline Sharing editor: saved audiences,
private machines, directory failures and non-owner access. `sharing-legacy-descriptor`
verifies that an older machine descriptor does not block the saved audience.
The `share-dialog*` fixture names are retained for their picker scenarios; they
now render the inline editor. `fleet-equipment-sharing-people` exercises direct
add/pause/resume/remove actions. `window.fleetQa.activity(n)` broadcasts changing
recipient counts, including while paused. Check the divider, keyboard search,
removing/re-adding the last recipient and refusal/retry at desktop and phone
widths in every theme and mode.

The `profile`, `profile-photo` and `profile-quiet` views are Identity >
Profile (memql#6220). `profile` is the control: no photo, a budget and a
personal storage allowance. `profile-photo` adds a photo, a phone and title,
storage past 90% and no budget. `profile-quiet` is a cluster that serves no
budget, an account with no personal limit, passkey-only sign-in and computer
use switched off. `&click=fill:Last%20name=Byron` shows Save appearing on a
change.

The `fleet-equipment-*` views open the whole Fleet app on Machines; add
`section=models`, `routing`, `apps`, `workbenches`, `logs` or `settings` to open
another Fleet section instead, for checks that span the app (such as the space
under each page title).

`fleet-devices` draws every machine kind side by side, each with the hardware a
machine of that kind reports; the caption under a drawing is the kind
`machines/device.ts` chose from that report, so a wrong choice shows as a caption
that does not match its picture. Add `device=<kind>` (`macbook`, `imac`,
`macMini`, `macStudio`, `macPro`, `gamingLaptop`, `laptop`, `tower`,
`workstation`, `server`, `desktop`) to a `fleet-equipment-*` view, with
`click=Studio%20Mac`, to see one on the Equipment page at desktop and phone widths.

The `feedback` view shows information, warning and error notices, an actionable
retry, domain errors and a removed-machine status. Check `theme=graphite`,
`cobalt` and `vellum`, light/dark modes, and desktop/phone widths. Tone rails
and message edges must stay straight; text wraps and keyboard retry works.

Their first rendered pass found what 447 green cases could not: the search box
drew about a hundred pixels tall, because `.os-input` is `flex: 1 1 12ch` and
the picker is a flex COLUMN, where that basis is the height; and a private
machine told its owner to edit `policy.yaml` for a share they had not chosen.
The review's pass found two more that no jsdom case can see: arrowing scrolled
the active option OUT of view (an unpositioned list, so `offsetTop` counted
from the dialog), and a refusal at the end of the scrolling body sat below the
fold at 1366x650 and 361x760. The current editor saves each action directly and
keeps failures beside the scrolling body with an explicit retry.

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
add-deployable wizard over simulated background runs. Open the analyzing acme
row: Configuration remains current, with the shared busy action bar, elapsed
time, Cancel and Leave. Leave returns to the analyzing row; reopening resumes
its state. The review/failure fixtures advance after opening, and Cancel emits
a terminal cancelled row. No fixture contacts GitHub or starts a real deployment.

## Static desktop wallpaper

`/wallpaper.html?theme=graphite&mode=dark` mounts the real Fold wallpaper and desk
numeral without a connection or saved preferences. Compare `graphite`, `vellum`
and `cobalt` in both `dark` and `light`. Add `&window=1` to inspect the pattern
behind the shell's window surface; also inspect a narrow viewport. All themes
share the approved Fold geometry (3.1% / 1.7% ground–ink mixes). There are no
nodes, lines, canvas or animation. The palette and numeral remain the theme's.

Email's full scene set uses `emailSeeds.ts` and `emailScenes.tsx`. Each renders
real components over the suite's fixture connection, with public example data.
The named scenes drive the same controls a person uses; `click=A|B` can override
the scene's interaction sequence. `fill:Label=value` fills a field, and the CSV
report scene supplies a browser File to the upload port. No scene sends email.

- Campaigns: `email-campaigns`, `email-campaigns-empty`, `email-campaign-add`,
  `email-campaign-add-waiting`, `email-campaign-add-review`, `email-campaign-draft`,
  `email-campaign-scheduled`, `email-campaign-sending`, `email-campaign-sent`,
  `email-campaign-repeating`, `email-campaign-edit`.
- Automations: `email-automations`, `email-automations-empty`,
  `email-automation-draft`, `email-automation-on`, `email-automation-add`,
  `email-automation-describe`, `email-rule`. Inspect the selected card's inspector,
  issue placement, proposal outline, counts and narrow vertical reading order.
- Audiences: `email-audiences`, `email-audiences-empty`, `email-audience`,
  `email-audience-add`, `email-people-report`, `email-signup-form`. Inspect the
  subscriber states, persistent import evidence and copyable signup code.
- Templates: `email-templates`, `email-templates-empty`, `email-template`,
  `email-template-plain`, `email-template-issues`, `email-template-create`,
  `email-layouts`, `email-layouts-none`, `email-layout`, `email-layout-builtin`.
  Inspect styled and plain previews, issues, publication actions and editor waits.
- Inbox: `email-inbox`, `email-inbox-empty`, `email-inbox-external`, `email-message`.
- Settings: `email-settings`, `email-settings-member`, `email-settings-single`,
  `email-sender`. The member fixture verifies that actions follow access.

Capture all scenes at 1400 and 520 pixels, light and dark. Check one heading,
one pinned action bar, readable copy and keyboard focus. Rendering fixtures do
not establish live cluster behavior; the component tests exercise subscriptions,
version order, omitted-payload re-reads and refusals separately.

Email's Overview: `email-overview` is a month of mail (five measured figures and
the campaigns by state), `email-overview-empty` is first use (real zeros, one
sentence and `Add a campaign` -- `&click=Add a campaign` follows it to the
wizard), `email-overview-reading` has every read still on its way (shapes, no
warning), `email-overview-refused` has three reads refused with nothing to show
(dashes and the engine's sentences, never a zero), `email-overview-unmeasured`
has a rate the engine could not state exactly (a dash with its reason beneath)
and `email-overview-untracked` has two rates with the same reason (said once,
under the first). `email-overview-floor` has an audience past the engine's count
window, so Subscriptions is a floor: `5,089 or more`, never an exact-looking sum.
Two scenes catch the page a few seconds after the cluster
stopped answering, with every figure still the last one: `email-overview-interrupted`
(the cluster refuses the refreshes: one notice carrying its sentence) and
`email-overview-reconnecting` (the connection dropped: the same notice, saying it
is reconnecting). Take the 520 capture too: the five figures fall to three and two.

Azure email setup: `azure-email-dns`, `azure-email-provisioning`, and
`azure-email-empty` use the shared Email Settings domain wizard. Open the
domain row (or Add sending domain on the empty view). Inspect both modes and
`width=narrow`: DNS names and values use the same copyable strips as Deployables
domain binding; the footer stays reachable and prior steps reopen. Provisioning
shows an elapsed wait and continues automatically using the fixture connection.
These views never contact Azure or change DNS.


## Materializer

The `materializer-*` scenes mount the real wizard, pages and lists. Capture at
1400×900 and 520×900 in light and dark. The 25 scenes cover populated, empty
and archived-only lists; the plus and Files entry points; mixed ingredients;
blank PDF and inference refusals; a retained folder refusal; making progress;
ready, archived, failed, described and deployable file pages; templates and
recipes, including their add/detail pages; and Settings. The `files-add-menu`
scene covers the Files entry point.

The October 12 review inspected all 100 Materializer captures, with an
independent dark-mode review. It removed the old composer styles, corrected
search input sizing, separated and labelled preferences, corrected back
labels, and made the current File step visible while making on narrow screens.
The integrated browser pass rechecked Settings, progress and description
refusal states after those corrections. There was no remaining accessory to
remove without losing navigation, filtering, status or an action. Captures
and the per-scene critique index stay outside this public repository.

These fixture scenes verify layout and content. They do not claim that an
Azure sender was provisioned or that a live model generated a document.

Azure send receipts: `azure-email-sends`, `azure-email-sends-pending`, and
`azure-email-sends-empty`. Choose **Our Studio**, open **Manage email for
Our Studio**, then **Check sends**.
These fixtures separate transport completion from timestamped recipient reports,
show missing reports as unconfirmed, and expose feedback readiness with empty
and populated lists. They never send email or provision Azure resources.

Nexus approvals: `nexus-approvals`, `nexus-approvals-empty`,
`nexus-approval-failure`, `nexus-approval-directory`, `nexus-approval-command`,
`nexus-approval-message`, `nexus-permissions`, `nexus-permissions-empty`, and
`nexus-permission` show the inbox, in-place decisions, and saved permissions.
`nexus-approval-discussion`, `nexus-approval-advice`,
`nexus-approval-advice-running`, `nexus-approval-guidance-recorded`, and
`nexus-approval-guidance-history` open the
real Ask sheet from a selected approval. Inspect both modes at desktop and
narrow widths, including the action bar, named computer scope, readable advice,
editable composer during investigation, and recorded-but-not-resumed guidance
with **Retry continuing** using the saved answer. The history scene shows
**View request** on persisted guidance so recovery remains reachable after reload.
These scenes use synthetic conversation and permission receipts; they never
approve production work, run commands, or call a model.

`/ask-routing.html` mounts the real Ask surface over a synthetic conversation
whose messages went to goals already running. `scene=labels` shows one
"To <goal> · <status>" line per goal under each message, across every status;
`split` says the reply before a split request's own answer (`state=quick`
answers it inline); `stop` puts "Stop this work? It can't be resumed." with
Keep and Stop on the goal's card (`state=pending` holds an answer in flight,
`state=error` shows a refusal that stays while the question waits); `many`
keeps a card that asks out of the "N tasks in progress" fold. Nothing is
routed, stopped or sent to a model. Inspect both modes at desktop and 820px
widths and with `narrow=1`, where Keep and Stop wrap beneath the question on
the card's right edge.

`/ask-routing.html?progress=1` shows a saved-work feedback discussion. Use
`state=sent`, `state=pending`, `state=error`, or `state=stopped` to review a
continued run, an in-flight answer, a retryable failure with the original draft
retained, and a run stopped at its overall limit. Inspect both modes and a
narrow viewport. These synthetic receipts never change a real run.
The same page keeps the general Ask states: `scene=empty`, `pending`, `quick`,
`error`, `format-error` and `model-error`.

`/ask-message-field.html` mounts the real Ask surface over a synthetic
conversation to review the message field. `scene=empty`, `multiline` (the
field grown upward), `attachments` (one landed upload, one halfway, one over
quota), `picker` (the Route menu open), `not-ready` (a personal route dim with
its reason), `apps` (Apps chosen, Where remains Auto), and `workbench`
(Workbench chosen, Apps disabled). Add `menu=where` or `menu=route` to open
either menu. Check disabled options by pointer and keyboard, unchanged choices
and draft, and stable descriptions before and after disabling. Add `widget=1`
for the floating widget's height and `narrow=1` for a narrow column. Nothing
uploads, routes or calls a model. Inspect both modes at desktop and 820px
widths.

## Role testing

Open `/role-testing.html?mode=light` or `mode=dark` for the actual role-testing
controls and session banner. `role=developer` omits Admin, `empty` shows no
active tests, `pending` holds the list read, and `error` exercises Retry. End
test removes the fixture session; Start deliberately refuses. This fixture
creates no identities, credentials or network writes. Check 390px and desktop
widths before live acceptance.

Navigation scroll: `/navigation-scroll.html?mode=light|dark&check=1` exercises
the shared provider in two independent windows with real browser layout and
the same `role="dialog"` boundary as the desktop window frame. It
checks short detail pages, Back, breadcrumbs, fresh/active tab resets, delayed loading,
and user cancellation of pending restoration. The document title reports the
result and `#scroll-checks` contains each assertion. Run at desktop/narrow widths
and with reduced motion. The fixture uses synthetic data and no cluster calls.

Fleet Activity scroll: `/?view=fleet-equipment-activity-scroll&mode=light|dark`
uses the actual Fleet workspace and its nested scroller with 40 synthetic
activity records. Open Studio Mac, choose Activity, scroll to Activity 40,
open it, then return through Back or the Activity breadcrumb. Both must return
to the same position. Clicking Activity again, or switching to Equipment and
then Activity, must instead start at the top. Scroll again, open another record,
and check that Back uses that new position. The QA
window carries Fleet's app identity and dialog role so its overflow and scroll
boundary match the desktop. Run in Safari as well as Chromium.

## Delivery and readable goal journeys

`delivery-overview`, `delivery-repositories`, and `delivery-changes` exercise
the dedicated Pipelines destinations using real app components and fixture
feeds. `nexus-readable`, `nexus-readable-waiting`, `nexus-readable-failed`, and
`nexus-readable-long` cover a recorded business goal, its actual reasons,
unknown usage and a 55-action history. Select an action with
`click=Review this month’s work, Done` and open evidence with
`click=Review this month’s work, Done|Technical details`. Inspect both modes at
1400px and 520px: the selected action is alongside the journey on wide windows
and above it on narrow windows. These are fixture render checks, not installed
cluster execution evidence.

Repository detail fixtures use `delivery-repository`, with
`click=Change requests|Request changes` for the bounded request form and
`click=Checks` for run summaries. `delivery-add-repository&click=Add repository`
opens the real left-step wizard. Installation states are `installation-prepared`,
`installation-applying`, `installation-complete`, `installation-restoring`,
`installation-restored`, `installation-empty` and `installation-unavailable`.
The complete fixture can explicitly select restoration, and the restoring fixture
finishes only after Check progress returns its fixture receipt. Both use the
real selection screen; they never contact a cluster or GitHub. The applying fixture has a healthy, synchronized
controller but deliberately remains applying: only the completed backend
receipt can say that installation was verified.
