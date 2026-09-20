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
   count or scope note).

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
   half-width column.

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
