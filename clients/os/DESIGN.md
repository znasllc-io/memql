# The MemQL OS interface language

[Supervised Visual Composition](SUPERVISED-VISUAL-COMPOSITION.md) extends this
language for tangible configuration and supervised assistance. Fleet is its
first approved implementation; broader app refactors require separate approval.

Twelve rules. Ten are owner-set (epic memql#4848); 11 and 12 came out of
epic memql#4937, and 11 is mostly a RATIFICATION -- Bin, Campaigns, Users,
Accounts and Training already complied, and Deployables was the violation. They exist because the apps drifted
into four section languages while every control came from one kit -- the
drift was in the LAYOUT GRAMMAR, not the vocabulary. Each rule names the kit
piece that encodes it; when a rule and a surface disagree, the surface is
wrong.

Themes sit ON TOP of these rules, never under them: a pack carries colour
and wallpaper values only (see `src/themes/`), so nothing here is themeable.

1. **Every section opens with the Head.** Title, at most one primary
   action, nothing else standing. No section renders a control strip before
   its content. Encoded by `kit` `Head` (its `meta` slot carries a quiet
   count or scope note). Counts come from the authorized filtered collection
   only after its read settles; unavailable is not zero.

2. **Filters are questions, not furniture.** Search and facet controls live
   behind one affordance on the Head line (`kit` `Refine`): collapsed by
   default, expanded while being asked, active constraints as removable
   chips beside it. A section never shows filter chrome over no content.

3. **Sort is not a button.** Ordering is quiet text on the list's scope
   line (`kit` `SortControl`): click swaps, the accessible name says what a
   click does. The default order stays an app-settings preference.

4. **Micro-preferences live in the app's Settings section.** Show revoked,
   show deactivated, include archived, default sort -- preferences, not
   toolbar chrome. The empty state points at the setting when hiding is why
   it is empty.

5. **One control line.** Inputs, selects, buttons and choice pills all
   stand `--os-control-h` tall at `--os-text-base`. Field-shaped controls
   share `--os-radius-xs`; the choice pill keeps its own radius because it
   is the shell's selection language, not a field. Selects drop UA chrome
   (`Select` draws its own currentColor chevron). Forms use `Field` --
   label above, control on the line; nothing invents a third field size.

6. **Actions are verbs, nouns are nouns.** An action is never styled as a
   data node it might sit beside -- and the strongest form of that is not to
   seat it among them at all. The Files rail is nouns (places and folders),
   so "New folder" lives on the Head's one Add control beside Upload, where
   "put something here" is asked once; as a muted, hairline-separated row
   between the Library tree and the Desktop place it still read as a folder
   you could open.

7. **Say it once.** A scope is named in one place. The rail highlights it,
   the Head names it, the list does not re-caption it, placeholders stay
   generic ("Search", not "Search your Library").

8. **One container language.** `Panel` + `Subhead` + `Field` is the
   grouping grammar. Settings groups keep their `fieldset` and `legend`
   SEMANTICS (a legend names its group to assistive tech), but the legend
   dresses as a Subhead and the legend-breaking-the-border box is gone --
   one look, not two. A deliberate MOMENT (the Accounts first-run card's
   eyebrow and headline) may keep its voice; chrome may not.

9. **Real estate belongs to content.** Lists take the window; forms take a
   readable measure; nothing paints half a window of dead space beside a
   half-width column. **A page starts at the same left edge as the list that
   opened it, and runs to the same right edge.** Deployables broke this for
   everything past its list -- a deployable's page in a centred 1080px column,
   its source and history in 760px, its settings in 900px -- so a click on a
   row moved the title 65px sideways at the default window size and 250px in a
   maximised one. The measure belongs to the THING (a sentence at ~80ch, a
   field at ~34rem), never to the page: capping the page to keep a text box
   from stretching is how the margins got painted empty.

10. **In-surface state is never a checkbox.** "Show archived" as a standing
    checkbox is the legacy tell this language exists to remove: archived
    things get a PLACE (the Bin app; the Files rail's Bin) or a settings
    preference (rule 4) -- never chrome that sits in front of content
    forever. Checkboxes belong to forms and settings, where they state a
    choice, not to browsing surfaces, where they would be furniture.

11. **A list and its detail never share a scroll column.** Beside the list
    with its own scroller (`.os-bin-list`), or replacing it with a quiet
    `<- <list name>` in the Head (`ComposePage`, `DeployablePage`) -- both are
    right, and which one depends on how tall the detail is. **Two `Head`s in
    one scroller is the tell that neither happened.** Deployables appended a
    whole page beneath the list it was selected from: 5,069px over 5.9
    viewports, two Heads, thirteen rails. A picture is not an exception --
    the Deploy map points at a deployable and links to its page rather than
    holding one.

12. **Acts follow the state, in one place.** A surface with a lifecycle
    carries one action bar on the window's bottom edge (`kit` `ActionBar`):
    the state in words, then the acts legal from that state, at most three,
    primary last. **An act that is not legal is ABSENT, never disabled** --
    a draft used to render an enabled Archive the server refuses, with no
    control anywhere that could reach the state that guard demands. Nothing
    that changes the thing's state lives anywhere else on the page: Pause at
    y=2412, Archive at 2499 and a cascade that archives a SIBLING at 885 were
    one surface, and the person had to know which was which.

## Applying them

- Build with the kit pieces; a surface needing a control the kit lacks
  promotes it on second use (`src/kit/controls.tsx` header) rather than
  respelling it locally.
- Judge at real size: the acceptance for any surface change under these
  rules is rendered screenshots, both modes, empty and populated -- not the
  diff. The audit that produced these rules was visual, and the drift it
  found had survived every code review.

## Record lists

App record collections use `RecordList` and `RecordRow`, the Deployables list
pattern. `LiveList` already provides the list container and arrival semantics.
Use the row's `actions` slot for independent controls, outside its opening
button, and `Subhead.meta` for subordinate collection counts. Static collections
can use `RecordList as="ul"` to retain list/listitem semantics. The repository
chooser uses this same anatomy, with selection state and grouped counts. See the
[coverage inventory](qa/list-coverage.md) for specialized grids, trees and
workflow surfaces that retain their interaction model.

## App overviews

Overview is the shared summary destination. Use `kit/Overview` for the header
and measured statistics, and `OverviewBreakdown` for a current distribution.
Each app supplies facts from its existing authorized reads; unavailable values
use `Figure` absence semantics instead of zero. Only draw activity history when
a real time series exists. Keep record lists and editors in their own sections.

Maps use `MapHeading` for title-aligned information help, `MapControls` for
bottom-right zoom and reset actions, and `usePanZoom` with wheel zoom disabled
and fitted reset enabled. Dragging pans; wheel scrolling belongs to the page.
Keep routine instructions inside the information control. Errors and stale-data
notices remain visible.

Fleet and Deployables adopt this composition first. Other apps can reuse these
components as their overview data is added, without changing their workflows.

### A tab is a different kind of thing; a subset is a filter

An app's section tabs are its NOUNS. Fleet's are machines, policies, the model
library; Deployables' are **deployables** and **sources**. A slice of one noun
-- standalone ones, the ones from a zip, the live ones -- is a question asked of
a list, and belongs behind `Refine` (rule 2), not on a tab.

Deployables used to draw both nouns in one list: a source was a row with its
apps indented beneath it, then a "Standalone" heading over the rest. A tree
inside a list, with two row types and an order that followed origin rather than
anything a person was looking for; the owner's word was "I really hate that
combined list". Two lists now, in one row language (`RecordRow`):

- **Deployables** is every deployable, flat. A row is there because it has an
  address of its own. Where it came from is a FACT ON THE ROW (the source's
  name, "Zip", "CI", "Built in") and a facet in Refine -- never a heading over
  it and an indent.
- **Sources** is the single configured repository catalog. Each row shows its
  GitHub identity, organization or personal target, repository and tracked
  branch, alongside the separate MemQL owning account. Credentials and saved
  installation bindings provide access metadata rather than separate list
  entries. There are no sibling Repositories or Accounts pages; Settings links
  to this catalog without repeating a credential roster. A source's detail
  holds its access, settings, apps and history. ZIP-backed apps remain in
  Deployables and retain their existing detail and lifecycle controls.
- **Every listed source is summarised by everything it made.** A
  source whose analysis was refused has made nothing and is still a source:
  this tab is where somebody looks for it to try again. A search is asked of
  the SOURCE (its name, where it lives, what it made) and never trims the
  summary of one it kept; archived is the source's own status, not one of its
  apps'. The list has its own fold for this (`foldSources`): read as a filter
  over the deployables list's answer it inherited that list's questions, and
  showed three of five seeded sources.
- **Remove source changes catalog visibility only.** It hides the configured
  repository from Sources and saved choices while preserving its grant,
  installation binding, package ID, automatic updates, deployables and history.
  Add deployable registers, reuses or restores the same authorized configuration
  atomically; it does not borrow a different identity's or MemQL account's
  history. Archive remains the distinct lifecycle operation. Only Add deployable
  offers Add source and GitHub connection setup.
- **What belongs to the source is said on the source, once.** A run parked at a
  source's gate is "Review needed" on that source's row and on its page's bar,
  with Review beside it -- not repeated on every deployable the source made.
- The trail keeps the way in: `Deployables > storefront`, or
  `Sources > acme/storefront > admin` when that is how the person got there.

One component draws both (`DeployablesSection` with a `root`), because every
page either list opens is the same page. Two instances, so drilling into a
source does not move the other tab off whatever it was showing.

### One trail row, and the window draws it

Every window carries exactly one trail row (`kit` `TrailRow`), drawn by the
window frame directly UNDER the section tabs. It is in the same place in every
app, and it is always there. **The order is the hierarchy:** the tabs choose
the section, and the trail is depth within it. Drawn above the tabs it read as
though `Deployables > MemQL OS` outranked the Deployables tab that is that
crumb's own parent. A window with a single destination has no tabs, and the
row sits under the window header.

**A heading publishes; it never draws.** Use `Head`'s `breadcrumbs` and `back`
props for local drill-downs exactly as before -- inside a window they are
PUBLISHED to the row rather than rendered beside the title. This is what makes
a second trail impossible rather than discouraged. It used to be a convention
each app had to honour, and Fleet showed two, because a `Head` handed an
explicit `breadcrumbs` prop drew them whether or not it was the window's
primary heading. Do not add a back button or a breadcrumb block anywhere in an
app body. Outside a window (a section rendered alone) there is no row, and
`Head` keeps its inline navigation so it still works.

The row reads, left to right: **Back, a hairline, the trail.**

- **Back is first, and the hairline is a boundary.** 21px separates Back from
  the first crumb (10, a 1px rule, 10) so a cursor aimed at one cannot land on
  the other. Left of the rule is history -- where you came from. Right of it is
  hierarchy -- where this page sits.
- **Back is always drawn, inert at an app's root.** Rule 12's "absent, never
  disabled" governs the ACTS of a lifecycle, where a disabled control promised
  something the server refused. Navigation runs the other way: a Back that
  appeared on the first drill-down would shove the trail 51px sideways under
  the cursor. The slot is fixed so the row stays a stable target.
- **The current page is never the part that is cut.** A trail wider than its
  window is pinned to its END: the beginning slides out of view and going back
  brings it home again. It is a real scroller with its bar hidden, not a clip,
  so a crumb reached by keyboard scrolls into view instead of taking focus
  while invisible. No single crumb may crowd the rest: ancestors cap at 28ch,
  the current page at 44ch, each with its full name on hover.
- **The current page is never a link**, whatever handler it carries: there is
  nowhere for it to go.

`PageNavigationProvider` still gives the trail to the first VISIBLE heading in
document order, so parked panes and secondary headings cannot claim it. A
page's own `back` wins; then the window's section origin; then the nearest
ancestor crumb that goes anywhere, so a page that names a clickable parent
never leaves Back dead.

A workspace toolbar above an inspector sets `navigation={false}` so the
inspector owns the page trail. Section breadcrumb jumps consume the intervening
history; peer tab navigation starts a fresh path. Existing forms retain their
own cancellation and completion callbacks through the shared back prop.

### One wizard for adding

Pressing an Add control runs a wizard (`kit` `Wizard`), whatever is being
added. There were three flows and they did not match: a deployable opened a
horizontal numbered stepper over an indented form, a machine opened a second
stepper numbered differently inside a bordered card, and a domain opened one
text field with its own button while the setup that followed lived on another
page. One component now, and its shape is the first-run gate's -- the orb, the
title, one sentence, then a column of steps -- because that was the one guided
surface in the shell that read as a single thought.

- **It takes the window** (rule 9). The first version stood in the gate's
  centred 38rem column: in a wide window that painted a third of the pane and
  left the rest empty, while the one thing on the page that wants width -- a
  DNS record's value -- wrapped inside it. Given room (`SPLIT_AT`, 760px of
  pane) the wizard is TWO PANES on the list page's own gutters: the steps on
  the left, still a vertical rail and now standing still, and the open step on
  the right with the rest of the width, headed by its name and what it asks.
  Without room it is one column with the open step's body beneath its line.
  That is a different place in the tree rather than a different style, so it
  is measured (`kit/useWide`), not left to a container query. **Width is for
  what needs it:** records, tables and commands take the pane; a sentence
  keeps a readable measure and a field keeps a field's width.
- **Side by side, the accented name is the step on the stage.** The mark says
  how far a step has got; the name says where the person is. They are the same
  step nearly always, and not when an answered step is still showing -- choose
  a repository and the stage went on saying "Repository" while the rail lit
  "Review". A stopped step keeps its own colour wherever the person is.
- **One question a step.** A step that asks three things is three steps. The
  add-a-deployable wizard's Source step used to be the kind of source, then
  two loose buttons (connect GitHub, or use a token), then a second stack of
  cards about updates, with a picker and three fields arriving in between --
  the owner's word was "overcrowded". It is two steps now: **Source** is the
  choice and nothing else, and the step after it is NAMED BY THE ANSWER
  (Repository, Zip, Your CI) and holds what that answer needs. Repository
  creation uses GitHub alone; it does not ask someone to choose a connection
  mechanism. A question that
  only makes sense once another is answered (what happens when something newer
  lands) waits until it is.
- **A step that is one choice is answered by choosing.** There is no Continue
  to press after the only thing on the page has been answered: the wizard
  moves on to the step the answer names, and the answered step folds to a line
  that can be opened again to choose differently.
- **A step's forward act is on the floor even when it leaves the wizard.**
  "Connect GitHub" is the Repository step's forward act, so it is the floor's
  button, not a button in the step. The step says what it needs (a first
  connection, a fresh one, nothing) because only the step has GitHub's own
  answer about the grant; the page draws the act.
- **A step knows before it offers.** An act that cannot work is not offered and
  then taken back. The Repository step used to offer Connect GitHub on every
  cluster and learn from the refusal that this one had no GitHub App -- then
  swap itself for a notice telling a cluster owner to ask an operator. It asks
  first now, with a read that writes nothing, and offers what can be done:
  Connect where there is an app, Set up GitHub where there is none and this
  person may register one, and for anybody else no act at all -- the step says
  who can set it up. "Not known" is not "no":
  when the question goes unanswered the old offer stands, and the refusal
  still lands in place.
- **The orb names the subject.** The gate wears the MemQL mark, because what is
  being set up there is the cluster. An add wears the thing being added: a
  globe, a machine, a rocket. Same circle, same theme tokens.
- **The title is the control's own words.** "Add a machine" opens "Add a
  machine". An act keeps its name through the whole flow, so a refusal says it
  "was not added".
- **Steps are the rail, vertical, at page scale** (`Rail` `scale="page"`), with
  the rail's closed state set: a check is done, a held ring is waiting on you,
  a pulse is the cluster working, a dimmed ring is not reached. Every step is
  one line -- its name, then its answer -- so a person can read back what they
  have said. One step is always open, and its body sits on the text edge with
  the rail's thread down its left side: there are two left edges on the page
  and no box. A step nobody has reached is its name and nothing else.
- **The floor is the action bar, and it says whose turn it is** (rule 12). The
  state in words on the left; on the right the way out, then the ONE forward
  act, primary last. **The forward act lives nowhere else** -- a step's body
  holds what is being answered, never the button that moves on. An act that is
  not legal yet is absent, and the words on the left say what is missing.
- **A wait is visible.** When it is the cluster's turn (`tone: "busy"`) there
  is no forward act, the bar's top hairline becomes a moving thread, the dot
  becomes a spinner, the state takes the accent, and `meta` measures the wait
  ("0:42", "checked 40s ago"). Waiting for a record somebody has not created
  yet is a wait and is drawn as one, not as a failure. Reduced motion keeps the
  held line and the held dot.
- **The floor has two verbs and one button.** *Cancel* undoes the whole thing:
  before anything is written that is simply going; after, it REMOVES what was
  made (the binding, the token), so it asks first, by name, in the bar
  (`confirm`). *Leave* goes and everything stays -- the cluster keeps working,
  and the thing's row opens the wizard again where it was left. *Done* when it
  is finished. **Only one of them is ever a button.** The act is the button;
  whatever stands beside it is a text action (`Act.text`) -- Cancel, Keep,
  Done beside Go live. Two buttons side by side ask to be weighed against each
  other, and a way out is not the same kind of thing as what happens next.
  While the cluster works there is no forward act, so Leave is the button and
  Cancel the text beside it. A flow with no act that undoes it (a deploy that
  has started) offers Leave and does not call it Cancel.
- **A wizard can be come back to.** Until the thing is finished its row opens
  THE SAME WIZARD, at whatever stage the cluster has walked it to -- never a
  second surface with its own look. There were two for a domain, and the owner
  found the seam within a minute of binding a real one: Leave, click the row,
  "now it looks completely different". What a wizard follows is the row the
  cluster sends, found by the thing's natural key (a hostname) as well as its
  id: the reply to an add and the graph row are two egress seams, and they do
  not promise to spell an id alike.
- **A step that can be done early can be opened early, and says so.** The DNS
  records are created in the same place as the ownership record and checked in
  the same pass, so they open while ownership is still waiting, and the line
  reads "Can be created now; checked together with ownership". A step that
  looks reachable for no stated reason reads as a bug.
- **Adding only.** Once the thing is finished it has a page of its own, and
  that page is where it is read and changed by whoever may. Nothing in a wizard
  edits.
