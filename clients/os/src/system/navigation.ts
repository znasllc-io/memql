import { appDestination, relocatedSectionSource } from "./appDestinations";
import { canOpen, sectionsFor, appById, type OsRegistry } from "./registry";

/** Derived from the same declarations that draw the shell. Renaming a tab
 * changes the agent's catalog too. */
export function navigationCatalog(registry: OsRegistry) {
  return registry.apps.filter(app => app.listed !== false).map(app => ({ id: app.id, name: app.name, requires: app.requires,
    sections: (app.sections ?? []).map(({ id, name, requires }) => {
      const source = relocatedSectionSource(app.id, id, requires);
      return { id, name, ...(requires ? { requires } : {}), ...(source ? { sourceRequires: `app:${source}` } : {}) };
    }),
    records: app.records ?? [],
  }));
}

export function navigationTarget(registry: OsRegistry, appId: string, args: Record<string, unknown>) {
  const target = appDestination(appId, typeof args.section === "string" ? args.section : undefined);
  appId = target.appId;
  args = { ...args, section: target.sectionId };
  const app = appById(registry, appId);
  if (!app || !canOpen(registry, appId)) return null;
  const admitted = sectionsFor(app);
  const recordSection = app.records?.find(record => typeof args[record.idField] === "string" && args[record.idField])?.section;
  const sectionId = typeof args.section === "string" && args.section ? args.section : recordSection ?? admitted[0]?.id;
  if (sectionId && !admitted.some(item => item.id === sectionId)) return null;
  const payload: Record<string, unknown> = {};
  for (const record of app.records ?? []) {
    const value = args[record.idField];
    if (record.section === sectionId && typeof value === "string" && value) payload[record.idField] = value;
  }
  return { app: app.id, section: sectionId, payload };
}
