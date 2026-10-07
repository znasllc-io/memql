import type { Result } from "@znasllc-io/memql-sdk-core/client";

export interface Source { id: string; publisher: string }
export interface Component { name: string; version: string; repository: string; commit: string }
export interface Release { candidateId: string; catalogDigest: string; components: Component[] }
export interface Page { sourceId: string; publisher: string; releases: Release[]; nextCursor?: string }

function object(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("The cluster returned an invalid update record.");
  return value as Record<string, unknown>;
}
function text(value: unknown): string {
  if (typeof value !== "string" || !value || value.length > 2048) throw new Error("The cluster returned an incomplete update record.");
  return value;
}
function array(value: unknown, limit: number): unknown[] {
  if (!Array.isArray(value) || value.length > limit) throw new Error("The cluster returned an invalid update collection.");
  return value;
}
function digest(value: unknown): string {
  const s = text(value);
  if (!/^sha256:[a-f0-9]{64}$/.test(s)) throw new Error("The cluster returned an invalid release identity.");
  return s;
}
function components(value: unknown): Component[] {
  const seen = new Set<string>();
  return array(value, 64).map(entry => {
    const c = object(entry), name = text(c.name);
    if (seen.has(name)) throw new Error("The cluster returned duplicate release components.");
    seen.add(name);
    return { name, version: text(c.version), repository: text(c.repository), commit: text(c.commit) };
  });
}
export function sources(result: Result): Source[] {
  const seen = new Set<string>();
  return array(object(result.single()).sources, 16).map(entry => {
    const s = object(entry), id = text(s.id);
    if (seen.has(id)) throw new Error("The cluster returned duplicate publishers.");
    seen.add(id);
    return { id, publisher: text(s.publisher) };
  });
}
export function page(result: Result, source: Source): Page {
  const p = object(result.single());
  if (p.sourceId !== source.id || p.publisher !== source.publisher) throw new Error("The cluster returned updates from a different publisher.");
  const seen = new Set<string>();
  return { sourceId: source.id, publisher: source.publisher, nextCursor: p.nextCursor === undefined ? undefined : text(p.nextCursor), releases: array(p.releases, 10).map(entry => {
    const r = object(entry), candidateId = digest(r.candidateId);
    if (seen.has(candidateId)) throw new Error("The cluster returned duplicate releases.");
    seen.add(candidateId);
    return { candidateId, catalogDigest: digest(r.catalogDigest), components: components(r.components) };
  }) };
}
export function detail(result: Result, source: Source, selected: Release): Release {
  const r = object(result.single()), release = object(r.release);
  if (r.sourceId !== source.id || r.candidateId !== selected.candidateId || r.catalogDigest !== selected.catalogDigest || release.candidateId !== selected.candidateId || release.publisher !== source.publisher) throw new Error("This release changed. Return to the publisher and check again.");
  return { candidateId: selected.candidateId, catalogDigest: selected.catalogDigest, components: components(release.components) };
}
export function title(release: Release): string { return release.components.map(c => `${c.name} ${c.version}`).join(" · "); }
