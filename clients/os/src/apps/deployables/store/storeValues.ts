import { bareShortId } from "@znasllc-io/memql-sdk-core/client";

import { boundStoreId, previewStoreId, type SiteRow } from "../rows";
import { settingsFingerprint, settingsKeyProblem, settingsRows, toSettingsMap, type SettingRow } from "../settings-editor";

// A storefront's PER-STORE settings, the arithmetic (memql#5602).
//
// `v1:platform:site.storeSettings` maps a BARE store id to that store's own
// {key: value} settings. The edge merges the entry of the store the in-force
// binding names over the site's `settings` -- Production's binding, or on the
// Testing destination the preview binding it substitutes -- so a value that
// belongs to one store (a Customer Account API client, a wholesale adapter)
// travels with whichever store the edge chose. A key a store does not set
// keeps the site's value.
//
// PURE, and separate from the panel, for `settings-editor.ts`' reason: which
// stores a storefront shows, what a save sends and when a change made
// elsewhere is a conflict are claims about functions, and a claim asserted
// through render() is asserted through three layers that can each fail for
// unrelated reasons.
//
// ===========================================================================
// THE SAVE REPLACES THE WHOLE MAP, SO THE DRAFT IS AN OVERLAY
// ===========================================================================
// updateSiteStoreSettings writes the map it is given and nothing else. So a
// save is the STORED map with the person's edits laid over it, store by store:
// a store they did not touch is sent exactly as it is stored now -- including
// the entry of a store this storefront was bound to before, which the engine
// keeps so that pointing a binding back restores its values. Sending only the
// stores on screen would erase those, silently.
//
// The overlay is also what makes a change made somewhere else safe. A store
// nobody touched here follows the row by construction; a store somebody DID
// touch remembers what it was when they started (`basis`), and only a change
// to THAT store, disagreeing with their edit, is a conflict worth stopping a
// save for (Fleet's sharing dialog, memql#5659, is the precedent).

/**
 * How many stores `storeSettings` may name, mirroring
 * component/memql/platform_site_settings_guard.go's
 * `siteStoreSettingsMaxStores`. Mirrored for a keystroke-rate answer; the
 * engine's refusal is the copy that decides.
 */
export const STORE_SETTINGS_MAX_STORES = 16;

/** The stored map: a bare store id to that store's own settings. */
export type StoreSettings = Readonly<Record<string, Readonly<Record<string, string>>>>;

/** Which of the storefront's two websites a store supplies. */
export type StoreDestination = "production" | "testing";

/** One store a binding names, and the websites it supplies. */
export interface StoreTarget {
  storeId: string;
  destinations: StoreDestination[];
}

/**
 * The stores this storefront's bindings name, each ONCE, Production's first.
 *
 * ONE ENTRY FOR A STORE BOTH WEBSITES USE, because the values are keyed by
 * store, not by website: both destinations bound to one store are served that
 * store's values, so showing it twice would be two editors for one entry.
 *
 * BARE IDS, because the guard refuses the canonical `v1:shopify:store:<id>`
 * as a key and the edge looks the entry up by the bare id. A binding is
 * written bare by every path that writes one; this holds the line if one is
 * not.
 */
export function storeTargets(site: SiteRow): StoreTarget[] {
  const out: StoreTarget[] = [];
  const add = (raw: string, destination: StoreDestination) => {
    const storeId = bareShortId(raw.trim()).trim();
    if (storeId === "") return;
    const held = out.find((t) => t.storeId === storeId);
    if (held !== undefined) held.destinations.push(destination);
    else out.push({ storeId, destinations: [destination] });
  };
  add(boundStoreId(site), "production");
  add(previewStoreId(site), "testing");
  return out;
}

/**
 * The stores whose values are KEPT although no binding names them -- a store
 * this storefront used before. Never served; returned if the store is
 * connected again. Sorted, so the list holds still.
 */
export function keptStoreIds(stored: StoreSettings, targets: readonly StoreTarget[]): string[] {
  return Object.keys(stored)
    .filter((id) => !targets.some((t) => t.storeId === id))
    .sort();
}

/** What a person has changed, over the stored map. */
export interface StoreValuesDraft {
  /** The rows of every store somebody edited. A store absent here shows what is stored. */
  edits: Readonly<Record<string, readonly SettingRow[]>>;
  /** Kept stores (no binding names them) marked to be dropped on save. */
  removed: readonly string[];
  /** Each touched store's stored values when it was first touched, as a fingerprint. */
  basis: Readonly<Record<string, string>>;
}

export const EMPTY_STORE_DRAFT: StoreValuesDraft = Object.freeze({ edits: {}, removed: [], basis: {} });

/** One store's stored values, never undefined. */
function storedFor(stored: StoreSettings, storeId: string): Readonly<Record<string, string>> {
  return stored[storeId] ?? {};
}

/** The rows the panel shows for a store: the person's, else what is stored. */
export function rowsForStore(draft: StoreValuesDraft, stored: StoreSettings, storeId: string): readonly SettingRow[] {
  return draft.edits[storeId] ?? settingsRows({ ...storedFor(stored, storeId) });
}

/** Whether the person has touched anything at all. */
export function draftTouched(draft: StoreValuesDraft): boolean {
  return Object.keys(draft.edits).length > 0 || draft.removed.length > 0;
}

/** The basis a store keeps from its FIRST touch: a later edit is measured from the same row. */
function withBasis(draft: StoreValuesDraft, stored: StoreSettings, storeId: string): Record<string, string> {
  return storeId in draft.basis
    ? { ...draft.basis }
    : { ...draft.basis, [storeId]: settingsFingerprint({ ...storedFor(stored, storeId) }) };
}

/** Replace one store's rows. */
export function editStore(
  draft: StoreValuesDraft,
  stored: StoreSettings,
  storeId: string,
  rows: readonly SettingRow[],
): StoreValuesDraft {
  return { ...draft, edits: { ...draft.edits, [storeId]: rows }, basis: withBasis(draft, stored, storeId) };
}

/** Mark a kept store to be dropped on save, or take the mark back. */
export function setKeptRemoved(
  draft: StoreValuesDraft,
  stored: StoreSettings,
  storeId: string,
  removed: boolean,
): StoreValuesDraft {
  const others = draft.removed.filter((id) => id !== storeId);
  return {
    ...draft,
    removed: removed ? [...others, storeId] : others,
    basis: withBasis(draft, stored, storeId),
  };
}

/**
 * The whole map a save sends: every stored store, with the person's edits laid
 * over it and the kept stores they removed left out.
 *
 * AN EMPTY ENTRY IS NOT SENT. A store with no values serves nothing, and an
 * empty object would still count against the sixteen stores a deployable may
 * keep. A row with a blank name is dropped (`toSettingsMap`): it is somebody
 * part-way through adding one.
 */
export function storeSettingsFromDraft(draft: StoreValuesDraft, stored: StoreSettings): Record<string, Record<string, string>> {
  const ids = new Set([...Object.keys(stored), ...Object.keys(draft.edits)]);
  const out: Record<string, Record<string, string>> = {};
  for (const id of [...ids].sort()) {
    if (draft.removed.includes(id)) continue;
    const values = toSettingsMap(rowsForStore(draft, stored, id));
    if (Object.keys(values).length > 0) out[id] = values;
  }
  return out;
}

/**
 * A canonical string for a whole map, for comparing two of them.
 *
 * SORTED at both levels and BLIND TO EMPTY ENTRIES: the stored map arrives in
 * whatever order the wire carried and the draft's map is rebuilt sorted, so a
 * raw comparison would report a change nobody made -- a Save enabled on an
 * untouched panel.
 */
export function storeSettingsFingerprint(map: StoreSettings): string {
  return JSON.stringify(
    Object.keys(map)
      .filter((id) => Object.keys(map[id] ?? {}).length > 0)
      .sort()
      .map((id) => [id, settingsFingerprint({ ...(map[id] ?? {}) })]),
  );
}

/** Whether a save would change anything. */
export function storeValuesDirty(draft: StoreValuesDraft, stored: StoreSettings): boolean {
  return storeSettingsFingerprint(storeSettingsFromDraft(draft, stored)) !== storeSettingsFingerprint(stored);
}

/** What the draft says about one touched store, as the map entry a save would send. */
function draftEntryFingerprint(draft: StoreValuesDraft, stored: StoreSettings, storeId: string): string {
  if (draft.removed.includes(storeId)) return settingsFingerprint({});
  return settingsFingerprint(toSettingsMap(rowsForStore(draft, stored, storeId)));
}

/** The touched stores whose stored values moved since they were touched. */
function movedStores(draft: StoreValuesDraft, stored: StoreSettings): string[] {
  return Object.keys(draft.basis).filter((id) => settingsFingerprint({ ...storedFor(stored, id) }) !== draft.basis[id]);
}

/**
 * Whether a change made somewhere else collides with the person's edit: a
 * store they touched has changed since, and what they have says something
 * different from what it changed to. Save waits for them to choose.
 *
 * A change to a store they did NOT touch is no conflict -- a save sends that
 * store exactly as it now is -- and a change TO what the draft already says
 * (their own save landing) is none either.
 */
export function storeValuesConflict(draft: StoreValuesDraft, stored: StoreSettings): boolean {
  return movedStores(draft, stored).some(
    (id) => draftEntryFingerprint(draft, stored, id) !== settingsFingerprint({ ...storedFor(stored, id) }),
  );
}

/**
 * Keep the edit: each moved store takes its new stored values as its basis, so
 * the conflict is answered and Save -- which still sends the edit -- replaces
 * the change knowingly.
 */
export function keepDraftOverStored(draft: StoreValuesDraft, stored: StoreSettings): StoreValuesDraft {
  const basis = { ...draft.basis };
  for (const id of movedStores(draft, stored)) basis[id] = settingsFingerprint({ ...storedFor(stored, id) });
  return { ...draft, basis };
}

/** Take the new values: drop the edit of every store that changed elsewhere, and only those. */
export function takeStoredWhereMoved(draft: StoreValuesDraft, stored: StoreSettings): StoreValuesDraft {
  const moved = new Set(movedStores(draft, stored));
  return forget(draft, moved);
}

/**
 * Let go of every touched store whose draft now says exactly what is stored --
 * the person's own save landing, or somebody else making the same change.
 * Nothing is left to choose there, and holding the edit would freeze that
 * store against the row from then on.
 */
export function settleDraft(draft: StoreValuesDraft, stored: StoreSettings): StoreValuesDraft {
  const settled = new Set(
    Object.keys(draft.basis).filter(
      (id) => draftEntryFingerprint(draft, stored, id) === settingsFingerprint({ ...storedFor(stored, id) }),
    ),
  );
  return settled.size === 0 ? draft : forget(draft, settled);
}

function forget(draft: StoreValuesDraft, ids: ReadonlySet<string>): StoreValuesDraft {
  const edits: Record<string, readonly SettingRow[]> = {};
  for (const [id, rows] of Object.entries(draft.edits)) if (!ids.has(id)) edits[id] = rows;
  const basis: Record<string, string> = {};
  for (const [id, fp] of Object.entries(draft.basis)) if (!ids.has(id)) basis[id] = fp;
  const removed = draft.removed.filter((id) => !ids.has(id));
  return Object.keys(edits).length === 0 && removed.length === 0 && Object.keys(basis).length === 0
    ? EMPTY_STORE_DRAFT
    : { edits, removed, basis };
}

/**
 * What is wrong with each of one store's rows, in the words the person needs,
 * or "" -- `settings`' own key rules (a store's values are held to exactly
 * them). The value caps and the `Ref` refusal are the engine's, for the
 * reason the App values editor gives.
 */
export function storeRowProblems(rows: readonly SettingRow[]): string[] {
  return rows.map((row) => settingsKeyProblem(row.key, rows));
}

/**
 * The sixteen-store rule, before the click, with its remedy: the kept stores
 * are listed beside this, each removable.
 */
export function storeCountProblem(map: StoreSettings): string {
  const count = Object.keys(map).filter((id) => Object.keys(map[id] ?? {}).length > 0).length;
  if (count <= STORE_SETTINGS_MAX_STORES) return "";
  return `Values would be kept for ${count} stores, and a storefront keeps at most ${STORE_SETTINGS_MAX_STORES}. Remove the values kept for stores this storefront no longer uses.`;
}
