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
  /** Section id the title-bar gear jumps to. */
  settingsSection?: string;
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
