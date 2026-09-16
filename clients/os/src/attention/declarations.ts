import type { FeatureChange } from "./model";

type AppDeclaration = { id: string; sections?: readonly { id: string }[]; attentionChanges?: readonly FeatureChange[] };

/** Structural checks only. Authors/reviewers judge meaning and dynamic reachability. */
export function attentionDeclarationErrors(apps: readonly AppDeclaration[]): string[] {
  const errors: string[] = [];
  const ids = new Set<string>();
  for (const app of apps) for (const item of app.attentionChanges ?? []) {
    const where = `${app.id}/${item.id}`;
    if (!/^[a-z0-9]+:[a-z0-9:._-]+$/.test(item.id) || !item.id.startsWith(`${app.id}:`)) errors.push(`${where}: use a stable ID prefixed with ${app.id}:`);
    if (ids.has(item.id)) errors.push(`${where}: duplicate attention ID; keep one declaration per capability`);
    ids.add(item.id);
    if (item.id.length > 300) errors.push(`${where}: ID exceeds the receipt's 300-character limit`);
    if (!item.revision.trim() || item.revision.length > 300) errors.push(`${where}: provide a meaningful revision of 1–300 characters`);
    if (!item.label.trim()) errors.push(`${where}: provide a label describing the change`);
    const sections = new Set(app.sections?.map(section => section.id));
    for (const section of [item.sectionId, ...(item.ancestors ?? [])]) if (!sections.has(section)) errors.push(`${where}: unknown section ${section}; wire the destination to a registered section`);
    if (item.target !== undefined && !item.target.trim()) errors.push(`${where}: omit target for a section destination, or provide the exact destination target`);
  }
  return errors;
}
