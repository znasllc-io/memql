// The app + widget registries (spec H). Static: the foundation is not a
// plugin loader -- runtime app delivery is a later question, deliberately
// unanswered here. Manifests are data; the components they name mount
// inside WindowFrame / WidgetFrame.

import type { ComponentType } from "react";

import type { RoleRequirement } from "./roles";
import { roleAdmits } from "./roles";

export interface OsAppSection {
  id: string;
  name: string;
  roles?: RoleRequirement;
}

export interface OsAppProps {
  /** Current section id ("" when the app declares no sections). */
  sectionId: string;
  /** Navigate the window to another of the app's sections. */
  navigate: (sectionId: string) => void;
  /** Augment the window's Ask context ("app:<id>" is always present). */
  askContext: (tag: string) => void;
  /**
   * A standing open instruction (epic memql#4842, #4845): opaque payload the
   * opener handed to `openApp`, delivered whether this window is fresh or
   * was already open. An app that acts on one calls `consumeIntent` with the
   * SAME id -- consumption is id-matched so acting on a stale render can
   * never eat a newer instruction. Apps that ignore both props are
   * unaffected.
   */
  intent?: { id: string; payload: Record<string, unknown> };
  /** Optional so a harness constructing props by hand stays valid. */
  consumeIntent?: (intentId: string) => void;
}

export interface OsAppManifest {
  id: string;
  name: string;
  /** Lucide icon component (kept as a plain component type -- no coupling). */
  icon: ComponentType<{ size?: number | string; "aria-hidden"?: boolean }>;
  roles?: RoleRequirement;
  sections?: OsAppSection[];
  /**
   * Section id the title-bar gear jumps to. REQUIRED on every app
   * (memql#4743): the owner's rule is that each app carries its own
   * settings, reachable the same way everywhere, so the gear is never a
   * button some windows happen not to have.
   *
   * It must name a section this manifest declares -- a gear pointing at an
   * id `sectionsForRole` will not return navigates the window nowhere, and
   * that failure is silent. `settingsSectionProblem` is the check;
   * `test/system/settingsContract.test.ts` runs it over the real registry.
   */
  settingsSection: string;
  /**
   * A DOCK FIXTURE is always in the dock and cannot be taken out of it
   * (memql#4784). The Bin is the only one, and it is the reason the flag
   * exists rather than a general capability: a trash can that a person can
   * unpin is one they can lose, and then archiving becomes a thing with no
   * visible destination.
   *
   * A fixture is deliberately NOT a pin. Pins live in `DesktopStore` and roam
   * with the desktop; a fixture is a property of the SHELL, so it is here on
   * the manifest, it is never written to storage, and no upgrade path or
   * corrupt document can leave somebody without one. `dockOrder` excludes it
   * from the pin strip and the dock renders it in its own slot; the context
   * menu offers no pin or unpin for it, because neither would do anything.
   */
  dockFixture?: boolean;
  component: ComponentType<OsAppProps>;
}

export interface OsWidgetManifest {
  id: string;
  name: string;
  icon: ComponentType<{ size?: number | string; "aria-hidden"?: boolean }>;
  roles?: RoleRequirement;
  /** Size in desktop grid cells. */
  size: { w: number; h: number };
  component: ComponentType;
}

export interface OsRegistry {
  apps: OsAppManifest[];
  widgets: OsWidgetManifest[];
}

export function appById(registry: OsRegistry, id: string): OsAppManifest | undefined {
  return registry.apps.find((a) => a.id === id);
}

/** The always-docked apps the actor may see, in registry order. */
export function fixturesForRole(registry: OsRegistry, actorRole: string): OsAppManifest[] {
  return registry.apps.filter((a) => a.dockFixture === true && roleAdmits(actorRole, a.roles));
}

/** Whether an app is a dock fixture -- what the pin menu and the pin strip
 *  both ask before offering anything. */
export function isDockFixture(registry: OsRegistry, appId: string): boolean {
  return appById(registry, appId)?.dockFixture === true;
}

export function widgetById(registry: OsRegistry, id: string): OsWidgetManifest | undefined {
  return registry.widgets.find((w) => w.id === id);
}

/** Apps the actor may see, in registry order. */
export function appsForRole(registry: OsRegistry, actorRole: string): OsAppManifest[] {
  return registry.apps.filter((a) => roleAdmits(actorRole, a.roles));
}

export function widgetsForRole(registry: OsRegistry, actorRole: string): OsWidgetManifest[] {
  return registry.widgets.filter((w) => roleAdmits(actorRole, w.roles));
}

/** Sections of an app the actor may open; the first is the default. */
export function sectionsForRole(app: OsAppManifest, actorRole: string): OsAppSection[] {
  return (app.sections ?? []).filter((s) => roleAdmits(actorRole, s.roles));
}

/**
 * The one launch admission check: an app the actor cannot see cannot be
 * opened by id either (dock, launcher and deep entry all go through this).
 */
export function canOpen(registry: OsRegistry, actorRole: string, appId: string): boolean {
  const app = appById(registry, appId);
  return !!app && roleAdmits(actorRole, app.roles);
}

/**
 * The settings-section contract, as a function rather than a test assertion
 * so the apps index can report the same defect it gates on.
 *
 * Returns null when the manifest is well-formed, otherwise the sentence to
 * show. It checks the DECLARED sections, never the role-admitted ones: a
 * gear target gated above the viewer is a legitimate manifest (the window
 * simply shows no gear for them), while a gear target that exists for
 * nobody is a bug in every session.
 */
export function settingsSectionProblem(app: OsAppManifest): string | null {
  const target = app.settingsSection.trim();
  if (target === "") return `${app.id}: settingsSection is empty`;
  const sections = app.sections ?? [];
  if (!sections.some((s) => s.id === target)) {
    const declared = sections.map((s) => s.id).join(", ") || "none";
    return `${app.id}: settingsSection "${target}" names no declared section (declared: ${declared})`;
  }
  return null;
}
