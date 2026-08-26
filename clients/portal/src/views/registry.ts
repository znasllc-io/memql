import type { PageManifest } from "../pages/manifest";

// The predefined views: which surfaces the portal designs by hand, and where
// they live.
//
// ===========================================================================
// WHY THESE FIVE AND NOT A GENERIC VIEW FOR EVERY CONCEPT
// ===========================================================================
// Every concept in the tree already renders, generically, in the concept
// browser -- that is memql#3316 + #3317 + #3318, and it is the property the
// whole epic protects. These five are the ones an OPERATOR LIVES IN: the
// users in their organisation, the agents doing work, the accounts they
// serve, what is deployed, and what happened. A generic rendering is the
// correct answer for a concept nobody has thought about; it is the wrong
// answer for the five screens that decide what the product feels like.
//
// ===========================================================================
// WHAT A PREDEFINED VIEW IS ALLOWED TO BE
// ===========================================================================
// A LAYOUT CHOICE OVER THE SAME ELEMENTS. Each view composes elements from
// sdk/ts-viewkit and reads each concept's @displayCard through the fitness
// profile exactly as the browser does; the only thing it does not do is ask a
// person or a model which arrangement to use. It may NOT hand-render a row.
// The moment a view reaches past the element library and emits its own row
// markup, the library stops being the thing that makes a new concept work for
// free -- so the rule is enforced mechanically by
// portal_view_composition_test.go in the repo root, not by review.
//
// If a view needs something no element provides, the fix is a new ELEMENT (as
// the proportion rail was, for these views' header band), not a special case
// here.
//
// ===========================================================================
// THE LAYOUT GRAMMAR: reading, then shape, then roll
// ===========================================================================
// All five views are the same three bands in the same order, because an
// operator arriving at any of them asks the same three questions in the same
// order:
//
//   1. how many are there?          -> stat tiles
//   2. how does that divide?        -> the proportion rail (+ a board where
//                                      the split is itself worth enumerating)
//   3. which ones, specifically?    -> a table, a timeline or a board
//
// Fixing the order means learning one screen teaches five. The bands are NOT
// numbered: they are three simultaneous readings of one population, not a
// sequence, and numbering them would assert an order of operations that does
// not exist.
//
// ===========================================================================
// A PREDEFINED VIEW IS DATA, INCLUDING ITS COMPOSITION (epic memql#4661)
// ===========================================================================
// This module was DATA ONLY -- ids, addresses, titles and prose -- and each
// view's composition lived in its own React module, "because the five
// genuinely differ". They do differ, and the differences turned out to be
// expressible: Users carries two populations (two SECTIONS), Deployments
// carries a control panel and row sets that are not concept rows (one
// WIDGET), Audit is an append-only log (a timeline in the roll band).
//
// What the five body modules cost while they existed is what makes this worth
// doing: a designed page could not be regenerated, could not carry a version
// strip, and improved only when somebody edited its module. Composed views got
// element personality, layouts and roles for free; the five screens an
// operator actually lives in did not.
//
// So each entry below now carries a `seed` -- a page manifest, the same value
// a composed view stores in a row -- and ViewPage renders it through the
// composer's own path. The five modules are gone.
//
// THE SEEDS ARE THE "ORIGINAL" VERSION. They need no graph row: a person who
// has never regenerated a page has no override, and the absence of a row is
// not a missing setting, it is the answer.

// A view's group in the nav rail.
//
// "operate" is the DATA an operator works in; the rail renders that set as the
// Views group's BUILT-IN sub-section. "nexus" is a surface about ONE GOAL of
// yours rather than a population, which is a different kind of thing and files
// under its own caption -- see the NEXUS constant in app/AppShell.tsx.
//
// A group is a rail PLACEMENT, not a URL. Every view here is addressed at
// /views/:id whichever caption it renders under, so moving a row between
// groups breaks no link and no bookmark.
export type ViewGroup = "operate" | "nexus";

export interface ViewDefinition {
  // The URL slug. Short, lower-case, and chosen here -- never a concept id.
  readonly id: string;
  // The nav label. IT NAMES THE CONCEPT (owner decision, 2026-08-25).
  //
  // This REVERSES the rule that stood here before -- "what the operator calls
  // the population, not what the schema calls it: 'People', not 'user'" -- and
  // the old argument is deleted rather than left standing beside its
  // replacement, because a reversed decision whose case is still written down
  // reads as the live one to the next person.
  //
  // What the reversal buys: one noun per population across the whole product.
  // A console whose rail says "People", whose concept browser says
  // `v1:identity:user` and whose audit rows say `user` asks the reader to
  // carry a translation table between three surfaces of one product, and the
  // table is invisible -- so the first time it matters is the moment somebody
  // cannot find the population they are already looking at. The label is the
  // concept's own noun, pluralised: Users, Accounts.
  //
  // What it does NOT license is renaming a CONCEPT to suit a label, or
  // dropping the prose that explains a concept whose name is not
  // self-evident: the Accounts blurb still has to say what an account is,
  // because the word alone does not.
  readonly label: string;
  readonly group: ViewGroup;
  // The concept whose rows this view is primarily about. A view may read
  // OTHER concepts too (Users also reads sessions); this is the one its
  // header, its walk and its row-detail address are keyed on.
  readonly conceptId: string;
  // The page heading. Usually the label; separate because a heading can
  // afford words a nav rail cannot.
  readonly title: string;
  // One line under the heading saying what this population is and why an
  // operator is looking at it. Shown verbatim -- it is interface copy, not a
  // description of the schema (the schema's own description is one click away
  // in the concept browser).
  readonly blurb: string;
  // The view's composition, as data. See the header: this is what used to be
  // a React module per view.
  readonly seed: PageManifest;
}

// The page id an override row is keyed on. Derived rather than stored so a
// view and its page id cannot drift apart -- and because "views.users" is
// mechanical, while a hand-written second copy of it would be a typo waiting
// to orphan somebody's regeneration.
export function viewPageId(id: string): string {
  return `views.${id}`;
}

const SESSION_CONCEPT_ID = "v1:identity:authSession";
const AUTHORIZATION_CONCEPT_ID = "v1:agents:agentAuthorization";

export const VIEWS: readonly ViewDefinition[] = [
  {
    id: "users",
    label: "Users",
    group: "operate",
    conceptId: "v1:identity:user",
    title: "Users",
    blurb:
      "Everyone who can sign in to this cluster, the role each of them holds, " +
      "and the sessions currently open in their name.",
    seed: {
      pageId: "views.users",
      title: "Users",
      blurb:
        "Everyone who can sign in to this cluster, the role each of them holds, " +
        "and the sessions currently open in their name.",
      sections: [
        {
          conceptId: "v1:identity:user",
          arrangement: {
            conceptId: "v1:identity:user",
            layout: "dashboard",
            elements: [
              // `metric: []` DECLINES the slot rather than leaving it unbound:
              // the strip is asked for a row count and no totals, because
              // summing revocationEpoch is a true number and a meaningless
              // one (memql#3319).
              { element: "statTile", band: "reading", bindings: { metric: [] } },
              {
                element: "chart.proportion",
                band: "shape",
                title: "By role",
                bindings: { category: ["role"], value: [] },
              },
              {
                element: "table",
                band: "roll",
                title: "Everyone",
                options: { sortField: "lastSeenAt", sortDir: "desc" },
              },
              // The invite flow: what the page LETS YOU DO, beside what it
              // shows. Supporting rather than standard -- a control is never
              // the thing a page is about.
              //
              // In the ROLL band rather than the reading, deliberately: who is
              // on their way IN is an administrative addendum to the
              // population (memql#4272), and a dashboard's reading band is
              // the top of the page. An operator came to look at users.
              {
                element: "widget",
                band: "roll",
                role: "supporting",
                title: "Invited",
                options: { widgetId: "invitePerson" },
              },
            ],
          },
          // Users without the users on it is a different page.
          required: [{ element: "table", band: "roll" }],
        },
        {
          // A SECOND SECTION, not a second walk hidden inside a body module.
          // This is the whole reason Users needed hand-written code: it reads
          // two populations, and an arrangement can now say so.
          conceptId: SESSION_CONCEPT_ID,
          // NO section title: the one band in it carries the caption instead.
          // A section heading plus a band caption over a single element is the
          // same words twice, and the band caption is the one that survives if
          // somebody adds a second element beside it.
          arrangement: {
            conceptId: SESSION_CONCEPT_ID,
            elements: [
              { element: "rowList", band: "roll", title: "Open sessions" },
            ],
          },
        },
      ],
    },
  },
  {
    id: "agents",
    label: "Agents",
    group: "nexus",
    conceptId: "v1:agents:agent",
    title: "Agents",
    blurb:
      "The agents this cluster runs, what each one is for, and the standing " +
      "authorizations people have granted them.",
    seed: {
      pageId: "views.agents",
      title: "Agents",
      blurb:
        "The agents this cluster runs, what each one is for, and the standing " +
        "authorizations people have granted them.",
      sections: [
        {
          conceptId: "v1:agents:agent",
          arrangement: {
            conceptId: "v1:agents:agent",
            layout: "dashboard",
            elements: [
              { element: "statTile", band: "reading", bindings: { metric: [] } },
              {
                element: "chart.proportion",
                band: "shape",
                title: "By kind",
                bindings: { category: ["kind"], value: [] },
              },
              {
                element: "table",
                band: "roll",
                title: "The fleet",
                options: { sortField: "name", sortDir: "asc" },
              },
            ],
          },
          required: [{ element: "table", band: "roll" }],
        },
        {
          conceptId: AUTHORIZATION_CONCEPT_ID,
          arrangement: {
            conceptId: AUTHORIZATION_CONCEPT_ID,
            elements: [
              {
                element: "kanban",
                band: "roll",
                title: "Standing authorizations",
                bindings: {
                  group: ["action"],
                  label: ["agentId"],
                  detail: ["planKind"],
                },
              },
            ],
          },
        },
      ],
    },
  },
  {
    id: "accounts",
    label: "Accounts",
    group: "operate",
    conceptId: "v1:identity:account",
    title: "Accounts",
    blurb:
      "The businesses you run MemQL for. An account is your record of a " +
      "customer -- it holds no credential and nothing signs in as one.",
    seed: {
      pageId: "views.accounts",
      title: "Accounts",
      blurb:
        "The businesses you run MemQL for. An account is your record of a " +
        "customer -- it holds no credential and nothing signs in as one.",
      sections: [
        {
          conceptId: "v1:identity:account",
          arrangement: {
            conceptId: "v1:identity:account",
            layout: "dashboard",
            elements: [
              { element: "statTile", band: "reading", bindings: { metric: [] } },
              {
                element: "chart.proportion",
                band: "shape",
                title: "By lifecycle",
                // No `category` binding: the rail finds the concept's own
                // status field through the display card, which is the point
                // of declaring one.
                bindings: { value: [] },
              },
              {
                element: "table",
                band: "roll",
                title: "Every account",
                options: { sortField: "updatedAt", sortDir: "desc" },
              },
              {
                element: "widget",
                band: "reading",
                role: "supporting",
                title: "Manage",
                options: { widgetId: "accountConsole" },
              },
            ],
          },
          required: [
            { element: "table", band: "roll" },
            // The console is how an account is CREATED. A page that lists
            // accounts and cannot make one is a different page.
            { element: "widget", band: "reading", options: { widgetId: "accountConsole" } },
          ],
        },
      ],
    },
  },
  {
    id: "deployments",
    label: "Deployments",
    group: "operate",
    conceptId: "v1:cluster:deployment",
    title: "Deployments",
    blurb:
      "What is running where, what shipped before it, and whether the last " +
      "gate passed.",
    seed: {
      pageId: "views.deployments",
      title: "Deployments",
      blurb:
        "What is running where, what shipped before it, and whether the last " +
        "gate passed.",
      sections: [
        {
          conceptId: "v1:cluster:deployment",
          arrangement: {
            conceptId: "v1:cluster:deployment",
            elements: [
              // The live-cluster panel: the ops controls, the gate, the
              // images in force, the rollouts in flight and the release
              // list. All of it is about the CLUSTER rather than the
              // population, and none of its row sets are concept rows --
              // which is why it is one widget rather than four sections.
              {
                element: "widget",
                band: "reading",
                title: "Live",
                options: { widgetId: "deployControls" },
              },
              {
                element: "chart.proportion",
                band: "shape",
                title: "By outcome",
                bindings: { value: [] },
              },
              {
                element: "timeline",
                band: "roll",
                title: "History",
                // Deployments spends ROW CLICK on the deploy/rollback
                // selection, so reading a row goes through a trailing View
                // control instead. Without this the two gestures collide and
                // selecting a version to ship also opens a dialog over it.
                options: { rowAction: "view" },
                bindings: {
                  at: ["updatedAt"],
                  label: ["version"],
                  detail: ["provider"],
                  status: ["status"],
                },
              },
            ],
          },
          required: [
            { element: "timeline", band: "roll" },
            { element: "widget", band: "reading", options: { widgetId: "deployControls" } },
          ],
        },
      ],
    },
  },
  {
    id: "audit",
    label: "Audit",
    group: "operate",
    conceptId: "v1:identity:auditEvent",
    title: "Audit",
    blurb:
      "Every security-relevant thing that happened: who did it, to what, and " +
      "how it ended.",
    seed: {
      pageId: "views.audit",
      title: "Audit",
      blurb:
        "Every security-relevant thing that happened: who did it, to what, and " +
        "how it ended.",
      sections: [
        {
          conceptId: "v1:identity:auditEvent",
          arrangement: {
            conceptId: "v1:identity:auditEvent",
            layout: "dashboard",
            elements: [
              { element: "statTile", band: "reading", bindings: { metric: [] } },
              {
                element: "chart.proportion",
                band: "shape",
                title: "By outcome",
                bindings: { value: [] },
              },
              {
                element: "chart.proportion",
                band: "shape",
                title: "By category",
                bindings: { category: ["category"], value: [] },
              },
              {
                element: "timeline",
                band: "roll",
                title: "The trail",
                bindings: {
                  at: ["occurredAt"],
                  label: ["action"],
                  detail: ["actorEmail"],
                  status: ["outcome"],
                },
              },
            ],
          },
          // An audit page without the trail is not an audit page.
          required: [{ element: "timeline", band: "roll" }],
        },
      ],
    },
  },
];

export function viewById(id: string): ViewDefinition | undefined {
  return VIEWS.find((view) => view.id === id);
}
