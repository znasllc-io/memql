// Run configurations: a named, filled argument form, persisted in the
// workspace as PLAIN EDITABLE TEXT.
//
// The format is JSON in a file the developer can open, diff, review and commit
// -- not extension state in a globalState blob. That is a requirement, not a
// convenience: a repository ships run configurations so a newcomer can execute
// the queries the codebase cares about, and an agent authors or edits one as
// text. Neither works if the data lives somewhere only the extension can
// reach.
//
// It follows that the file is UNTRUSTED INPUT. It comes out of a repository,
// which means it can be anything -- so every read validates rather than casts,
// and an entry that does not validate is DROPPED with the rest of the file
// still usable. A single malformed entry must not make a repo's whole run set
// disappear.
//
// And because a repository can ship one: NOTHING HERE AUTO-RUNS. Loading the
// file populates a tree; running is always a click. There is no run-on-open
// and no run-on-save anywhere in the extension.
//
// Deliberately free of `vscode` imports; file IO uses node:fs directly, the
// same shape clusters/file.ts uses. Tested under bare `node --test`.

import * as fs from "node:fs/promises";
import * as path from "node:path";

import { RUNNABLE_KINDS, type RunnableKind } from "../constructs/runnable.js";

/** The workspace-relative location. `.memql/` keeps it beside other tool config rather than in `.vscode/`, because it is not VS Code-specific data. */
export const RUN_CONFIG_RELATIVE_PATH = path.join(".memql", "runs.json");

export function runConfigPath(workspaceRoot: string): string {
  return path.join(workspaceRoot, RUN_CONFIG_RELATIVE_PATH);
}

export interface RunConfig {
  /** Unique within the file; the identity an update or delete resolves by. */
  name: string;
  kind: RunnableKind;
  /** The construct's declared name -- what the engine is called with. */
  construct: string;
  /**
   * Workspace-relative path of the .memql file that declares it. Optional
   * because a hand-authored configuration may omit it, and a run can still
   * proceed against the deployed construct; when present the extension opens
   * that file so the run injects the buffer.
   */
  file?: string;
  /** The filled argument values, keyed by declared arg name. */
  args: Record<string, unknown>;
}

export interface RunConfigFile {
  /** Schema version. Present so a future format change can be detected rather than mis-parsed. */
  version: number;
  runs: RunConfig[];
}

export const RUN_CONFIG_VERSION = 1;

export function emptyRunConfigFile(): RunConfigFile {
  return { version: RUN_CONFIG_VERSION, runs: [] };
}

export type ParseResult =
  | { ok: true; file: RunConfigFile; dropped: string[] }
  | { ok: false; error: string };

/**
 * parseRunConfigFile validates raw JSON text.
 *
 * A top-level failure (not JSON, not an object, `runs` not an array) is
 * reported as an error so the UI can say the file is broken -- silently
 * showing an empty list would look identical to "you have no run
 * configurations", and the developer would re-create the ones they already
 * have.
 *
 * A per-entry failure is DROPPED and named in `dropped`, so the rest of the
 * file still works and the UI can say which entries were ignored.
 */
export function parseRunConfigFile(raw: string): ParseResult {
  if (raw.trim() === "") return { ok: true, file: emptyRunConfigFile(), dropped: [] };

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    return { ok: false, error: `not valid JSON: ${err instanceof Error ? err.message : String(err)}` };
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { ok: false, error: "the top level must be a JSON object" };
  }
  const obj = parsed as Record<string, unknown>;
  if (obj.runs !== undefined && !Array.isArray(obj.runs)) {
    return { ok: false, error: '"runs" must be an array' };
  }

  const runs: RunConfig[] = [];
  const dropped: string[] = [];
  const seen = new Set<string>();
  for (const [index, entry] of (obj.runs ?? []).entries()) {
    const config = parseOne(entry);
    if (config === undefined) {
      dropped.push(`runs[${index}]`);
      continue;
    }
    // A duplicate name would make "run the one called X" ambiguous, and every
    // lookup here resolves the FIRST match -- so the loser would be visible in
    // the tree and unreachable by name. Dropping it says so.
    if (seen.has(config.name)) {
      dropped.push(`runs[${index}] (duplicate name "${config.name}")`);
      continue;
    }
    seen.add(config.name);
    runs.push(config);
  }

  const version = typeof obj.version === "number" ? obj.version : RUN_CONFIG_VERSION;
  return { ok: true, file: { version, runs }, dropped };
}

function parseOne(raw: unknown): RunConfig | undefined {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return undefined;
  const r = raw as Record<string, unknown>;
  const name = r.name;
  if (typeof name !== "string" || name.trim() === "") return undefined;
  const construct = r.construct;
  if (typeof construct !== "string" || construct === "") return undefined;
  const kind = r.kind;
  if (typeof kind !== "string" || !(RUNNABLE_KINDS as readonly string[]).includes(kind)) {
    return undefined;
  }
  // args must be an OBJECT. An array or a scalar here would sail through a
  // cast and then render as a call string with numeric argument names.
  const args = r.args;
  if (args !== undefined && (args === null || typeof args !== "object" || Array.isArray(args))) {
    return undefined;
  }
  const out: RunConfig = {
    name: name.trim(),
    kind: kind as RunnableKind,
    construct,
    args: (args as Record<string, unknown> | undefined) ?? {},
  };
  if (typeof r.file === "string" && r.file !== "") out.file = r.file;
  return out;
}

/**
 * serializeRunConfigFile renders the file.
 *
 * Two-space indent and a trailing newline because a human edits this and a
 * repository diffs it. Argument keys are SORTED so re-saving an unchanged
 * configuration produces no diff -- without that, the file churns on every
 * save according to whatever order the form happened to build its object in.
 */
export function serializeRunConfigFile(file: RunConfigFile): string {
  const normalised: RunConfigFile = {
    version: file.version,
    runs: file.runs.map((r) => {
      const args: Record<string, unknown> = {};
      for (const key of Object.keys(r.args).sort()) args[key] = r.args[key];
      const out: RunConfig = { name: r.name, kind: r.kind, construct: r.construct, args };
      if (r.file !== undefined) out.file = r.file;
      return out;
    }),
  };
  return `${JSON.stringify(normalised, null, 2)}\n`;
}

/** upsertRunConfig replaces the entry with the same name, or appends. Returns a NEW file value. */
export function upsertRunConfig(file: RunConfigFile, config: RunConfig): RunConfigFile {
  const runs = file.runs.slice();
  const at = runs.findIndex((r) => r.name === config.name);
  if (at >= 0) runs[at] = config;
  else runs.push(config);
  return { version: file.version, runs };
}

/** removeRunConfig drops the entry with the given name. Returns a NEW file value. */
export function removeRunConfig(file: RunConfigFile, name: string): RunConfigFile {
  return { version: file.version, runs: file.runs.filter((r) => r.name !== name) };
}

export type ReadRunConfigsResult =
  | { ok: true; file: RunConfigFile; dropped: string[] }
  | { ok: false; error: string };

/**
 * readRunConfigs loads the file, treating a MISSING one as empty.
 *
 * Missing is the overwhelmingly common state -- most workspaces ship no run
 * configurations -- so it is not an error, and the tree renders an empty list
 * rather than a failure row.
 */
export async function readRunConfigs(file: string): Promise<ReadRunConfigsResult> {
  let raw: string;
  try {
    raw = await fs.readFile(file, "utf8");
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") {
      return { ok: true, file: emptyRunConfigFile(), dropped: [] };
    }
    return { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
  const parsed = parseRunConfigFile(raw);
  if (!parsed.ok) return { ok: false, error: `${file}: ${parsed.error}` };
  return { ok: true, file: parsed.file, dropped: parsed.dropped };
}

/**
 * writeRunConfigs performs a READ-MODIFY-WRITE via `mutate`.
 *
 * The file is plain text the developer is invited to edit, so it can have
 * changed since the tree last read it -- including in the editor that is open
 * on it right now. Serialising a cached value would clobber that edit. This
 * mirrors how clusters/file.ts treats the cockpit-shared clusters.yaml, for
 * the same reason.
 *
 * A file that fails to PARSE is refused rather than overwritten: it is the
 * developer's text, and the one thing worse than not saving their new run
 * configuration is destroying the ten they already had.
 */
export async function writeRunConfigs(
  file: string,
  mutate: (current: RunConfigFile) => RunConfigFile,
): Promise<void> {
  const current = await readRunConfigs(file);
  if (!current.ok) {
    throw new Error(
      `${current.error} -- refusing to overwrite it. Fix the file by hand, then try again.`,
    );
  }
  const next = mutate(current.file);
  await fs.mkdir(path.dirname(file), { recursive: true });
  await fs.writeFile(file, serializeRunConfigFile(next), "utf8");
}
