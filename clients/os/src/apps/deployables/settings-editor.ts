// The runtime-settings editor's arithmetic (epic memql#4906, decision P7).
//
// PURE, and separate from the panel, for `rows.ts`' reason: what an editor
// DOES with a map -- how a row becomes a key, what makes a key unacceptable,
// what a save actually sends -- is a set of claims about functions, and a
// claim asserted through render() is asserted through three layers that can
// each fail for unrelated reasons.
//
// THE FORM IS MIRRORED FROM THE SERVER, AND ONLY THE FORM. The key's shape is
// checked here so somebody typing `api-base` learns it while they are typing.
// The two CAPS and the `Ref` refusal are deliberately not mirrored: a browser
// cannot know this cluster's configured limits, and the engine's refusal
// names the knob, which is the fact an operator needs. That split is the site
// hostname field's own precedent.

/**
 * The shape of a settings key, mirroring
 * component/memql/platform_site_settings_guard.go's `siteSettingsKeyForm`.
 *
 * A bundle reads `config.settings.<key>`, so a key is a JavaScript
 * identifier: a letter, then letters, digits or underscores, at most 64
 * characters.
 */
export const SETTINGS_KEY_FORM = /^[A-Za-z][A-Za-z0-9_]{0,63}$/;

/** One row of the editor. */
export interface SettingRow {
  /** The LIST KEY, stable for the life of the row and never the setting's
   *  name -- see the panel's own note on why. */
  id: string;
  key: string;
  value: string;
}

/**
 * The stored object as editor rows, in a STABLE ORDER.
 *
 * Sorted by key rather than left in object order: a JSON object's key order
 * is whatever the wire produced, so an unsorted list would reshuffle itself
 * when a save came back -- under the hands of the person who just saved it.
 */
export function settingsRows(settings: Record<string, string>): SettingRow[] {
  return Object.keys(settings)
    .sort()
    .map((key) => ({ id: key, key, value: settings[key] ?? "" }));
}

/**
 * The rows as the object a save sends.
 *
 * A ROW WITH A BLANK KEY IS DROPPED, not sent as `""`. An empty row is
 * somebody part-way through adding a setting, and sending it would make the
 * server refuse the whole save over a field they had not filled in yet. A
 * blank VALUE is kept, because an empty string is a value a bundle can
 * legitimately read.
 */
export function toSettingsMap(rows: readonly SettingRow[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const row of rows) {
    const key = row.key.trim();
    if (key === "") continue;
    out[key] = row.value;
  }
  return out;
}

/**
 * What is wrong with this row's key, in the words the person needs, or "".
 *
 * A blank key is NOT a problem: it is a row somebody has just added and has
 * not typed into, and flagging it would put an error under a field before it
 * was touched. It simply does not get saved.
 */
export function settingsKeyProblem(key: string, rows: readonly SettingRow[]): string {
  const trimmed = key.trim();
  if (trimmed === "") return "";
  if (!SETTINGS_KEY_FORM.test(trimmed)) {
    return "A setting's name is a letter followed by letters, digits or underscores, at most 64 characters -- an app reads it as config.settings.name.";
  }
  if (rows.filter((r) => r.key.trim() === trimmed).length > 1) {
    // WORTH CATCHING HERE rather than leaving to the server, and it is the
    // one rule the server structurally cannot state: two rows with one name
    // collapse into a single object key on the way out, so the save would
    // succeed and quietly keep whichever value came last.
    return `Two settings are both called ${trimmed}. Only one of them would be saved.`;
  }
  return "";
}

/**
 * A canonical string for a settings object, for comparing two of them.
 *
 * SORTED, because `JSON.stringify` preserves INSERTION order and the two
 * sides being compared are built differently: the stored map arrives from the
 * wire in whatever order the payload carried, and the draft is rebuilt from
 * rows this module sorts by key. A raw stringify comparison therefore reports
 * a difference whenever the wire's order is not alphabetical -- which shows
 * up as a Save button that is enabled on a form nobody has touched, and, more
 * quietly, as an edit that looks unsaved after it saved.
 */
export function settingsFingerprint(settings: Record<string, string>): string {
  return JSON.stringify(
    Object.keys(settings)
      .sort()
      .map((k) => [k, settings[k] ?? ""]),
  );
}
