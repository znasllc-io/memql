import { accessAdmits, appById, type OsRegistry } from "./registry";

/** Derived from the same declarations that draw the shell. Renaming a tab
 * changes the agent's catalog too. */
export function navigationCatalog(registry: OsRegistry) {
  return registry.apps.map(app => ({ id: app.id, name: app.name, requires: app.requires,
    sections: (app.sections ?? []).map(({ id, name, requires }) => ({ id, name, ...(requires ? { requires } : {}) })),
    records: app.records ?? [],
  }));
}

export function navigationTarget(registry: OsRegistry, appId: string, args: Record<string, unknown>) {
  const app = appById(registry, appId);
  if (!app || !accessAdmits(app.requires)) return null;
  const recordSection = app.records?.find(record => typeof args[record.idField] === "string" && args[record.idField])?.section;
  const sectionId = typeof args.section === "string" && args.section ? args.section : recordSection ?? app.sections?.[0]?.id;
  const section = app.sections?.find(item => item.id === sectionId);
  if (sectionId && (!section || !accessAdmits(section.requires))) return null;
  const payload: Record<string, unknown> = {};
  for (const record of app.records ?? []) {
    const value = args[record.idField];
    if (record.section === sectionId && typeof value === "string" && value) payload[record.idField] = value;
  }
  return { app: app.id, section: sectionId, payload };
}
