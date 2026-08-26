import type { ReactNode } from "react";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

// The WIDGET registry (epic memql#4661, task memql#4674).
//
// ===========================================================================
// WHY THIS EXISTS
// ===========================================================================
// The arrangement system is meant to be the PAGE system -- every portal page
// is a layout plus elements, and is therefore regenerable, versioned and
// consistent. The thing that stopped that from being possible was never the
// data: it was the CONTROLS. A page that lets an operator add a machine, edit
// a routing policy or roll back a deploy has a form on it, and there was no
// way to say "a form goes here" in an arrangement. So every such page kept a
// hand-built layout, and every hand-built layout was a page regeneration could
// not touch.
//
// A widget is the answer: a registered interactive component that participates
// in an arrangement exactly like an element does.
//
// ===========================================================================
// THE REGISTRY IS CLOSED, AND THAT IS THE WHOLE SAFETY STORY
// ===========================================================================
// An arrangement NAMES a widget; it never defines one. sanitizeArrangement
// drops an entry whose widgetId is not in this map, so:
//
//   * a model regenerating a page may PLACE any of these and cannot invent a
//     control that does something else, or a control at all;
//   * a stored arrangement from a release that had a widget this build does
//     not is repaired rather than crashing;
//   * the set of things a page can DO is enumerable by reading one file.
//
// ===========================================================================
// WIDGETS WRAP, THEY DO NOT REWRITE
// ===========================================================================
// Every entry below adapts an existing component. The point of convergence is
// that a page stops owning its LAYOUT, not that its behaviour gets rewritten:
// the invite flow, the account console and the deploy controls keep their own
// tests, their own dialogs and their own live data exactly as they were.

// What a widget is handed. Deliberately narrow -- the population the section
// is about, and the selection -- because a widget that needed more than this
// would be a page pretending to be a control.
export interface WidgetProps {
  // The section's concept, when the section has one. A widget placed on a
  // section whose concept this cluster does not publish is not rendered at
  // all, so this is never a stale descriptor.
  concept: Concept;
  rows: readonly Row[];
  // "" when nothing is selected.
  selectedRowId: string;
  onSelect: (rowId: string) => void;
  // Re-run the section's walk. What a widget calls after a write, so the page
  // it sits on reflects what it just did.
  onChanged: () => void;
}

export interface WidgetDefinition {
  readonly id: string;
  // What this control is, for a composer's picker and for the prompt that
  // offers it to a model. One sentence, in the words a person would use.
  readonly summary: string;
  // The caption a page shows above it, when the arrangement does not name one.
  readonly title: string;
  readonly render: (props: WidgetProps) => ReactNode;
}

// STATIC IMPORTS, deliberately -- and the contrast with the SCENE registry
// beside it is the whole reasoning.
//
// A scene is lazy because three.js, fiber and drei are the portal's largest
// dependency by a wide margin, no other page uses them, and the registry that
// names every scene is reachable from every arranged page. A static import
// there would put the entire WebGL stack in the main bundle. That cost is
// large, measurable, and guarded by a test (nexusMap.test.tsx).
//
// A widget is a form. Its cost is a few kilobytes of code the portal's main
// bundle already carries, since routes are not code-split either. Lazy-loading
// it would buy nothing measurable and charge for it in two places that are
// real: a Suspense flash on every page that places one, and an await in every
// test that asserts a page rendered.
//
// So the rule is "lazy where the dependency is genuinely heavy AND isolated",
// not "lazy because a registry is a natural boundary". Should a widget ever
// pull in something the size of three.js, THAT widget becomes lazy and this
// comment gets a third paragraph.
import { AccountConsole } from "../accounts/AccountConsole";
import { AddMachineWidget } from "./AddMachineWidget";
import { DeployControlsWidget } from "./DeployControlsWidget";
import { InvitePersonWidget } from "./InvitePersonWidget";
import { ProfilePreferencesWidget } from "./ProfilePreferencesWidget";
import { RoutingPolicyWidget } from "./RoutingPolicyWidget";

export const WIDGETS: readonly WidgetDefinition[] = [
  {
    id: "accountConsole",
    title: "Manage",
    summary: "Create an account, edit one, and manage its credentials.",
    render: (props) => (
      <AccountConsole
        concept={props.concept}
        rows={props.rows}
        selectedRowId={props.selectedRowId}
      />
    ),
  },
  {
    id: "invitePerson",
    title: "Invite",
    summary: "Invite somebody to this cluster, and see who has not answered yet.",
    render: () => <InvitePersonWidget />,
  },
  {
    id: "deployControls",
    title: "Deploy",
    summary: "What is running, what shipped before it, and the deploy and rollback controls.",
    render: (props) => (
      <DeployControlsWidget
        concept={props.concept}
        rows={props.rows}
        selectedRowId={props.selectedRowId}
        onSelect={props.onSelect}
      />
    ),
  },
  {
    id: "addMachine",
    title: "Pair a machine",
    summary: "Pair a new machine to this fleet with a one-time code.",
    render: (props) => <AddMachineWidget onChanged={props.onChanged} />,
  },
  {
    id: "routingPolicyEditor",
    title: "Routing policy",
    summary: "Choose how work is spread across the machines in this fleet.",
    render: (props) => <RoutingPolicyWidget onChanged={props.onChanged} />,
  },
  {
    id: "profilePreferences",
    title: "Preferences",
    summary: "Your own settings on this cluster.",
    render: () => <ProfilePreferencesWidget />,
  },
];

export const WIDGET_IDS: readonly string[] = WIDGETS.map((w) => w.id);

export function widgetById(id: string): WidgetDefinition | undefined {
  return WIDGETS.find((widget) => widget.id === id);
}

// renderWidget is the one call site.
//
// An UNREGISTERED id renders nothing rather than a placeholder, and the
// placeholder is what it must not be: sanitizeArrangement has already dropped
// every entry naming a widget this build does not carry, so reaching here with
// an unknown id means the two disagree -- and a visible "unknown widget" box
// would put the disagreement on the page instead of leaving the page correct.
export function renderWidget(id: string, props: WidgetProps): ReactNode {
  return widgetById(id)?.render(props) ?? null;
}
