// The predefined views: which surfaces the portal designs by hand, and where
// they live.
//
// ===========================================================================
// WHY THESE FIVE AND NOT A GENERIC VIEW FOR EVERY CONCEPT
// ===========================================================================
// Every concept in the tree already renders, generically, in the concept
// browser -- that is memql#3316 + #3317 + #3318, and it is the property the
// whole epic protects. These five are the ones an OPERATOR LIVES IN: the
// people in their organisation, the agents doing work, the customers they
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
// This module is DATA ONLY -- ids, addresses, titles and prose. Each view's
// composition lives in its own module, because the five genuinely differ:
// People carries two populations, Deployments carries a control panel and row
// sets that are not concept rows at all, Audit is an append-only log.

// A view's group in the nav rail. Two groups, not one flat list: the
// predefined views are SURFACES an operator works in, the concept browser is
// the SUBSTRATE underneath them, and collapsing that distinction is how a nav
// becomes nine undifferentiated tabs.
export type ViewGroup = "operate";

export interface ViewDefinition {
  // The URL slug. Short, lower-case, and chosen here -- never a concept id.
  readonly id: string;
  // The nav label. What the operator calls the population, not what the
  // schema calls it: "People", not "user".
  readonly label: string;
  readonly group: ViewGroup;
  // The concept whose rows this view is primarily about. A view may read
  // OTHER concepts too (People also reads sessions); this is the one its
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
}

export const VIEWS: readonly ViewDefinition[] = [
  {
    id: "people",
    label: "People",
    group: "operate",
    conceptId: "v1:identity:user",
    title: "People",
    blurb:
      "Everyone who can sign in to this cluster, the role each of them holds, " +
      "and the sessions currently open in their name.",
  },
  {
    id: "agents",
    label: "Agents",
    group: "operate",
    conceptId: "v1:agents:agent",
    title: "Agents",
    blurb:
      "The agents this cluster runs, what each one is for, and the standing " +
      "authorizations people have granted them.",
  },
  {
    id: "customers",
    label: "Customers",
    group: "operate",
    conceptId: "v1:identity:account",
    title: "Customers",
    blurb:
      "The businesses you run memQL for. An account is your record of a " +
      "customer -- it holds no credential and nothing signs in as one.",
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
  },
];

export function viewById(id: string): ViewDefinition | undefined {
  return VIEWS.find((view) => view.id === id);
}
