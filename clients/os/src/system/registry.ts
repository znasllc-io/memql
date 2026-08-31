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
