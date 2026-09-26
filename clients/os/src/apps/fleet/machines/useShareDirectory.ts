import { useCallback, useEffect, useState } from "react";
import type { Connection } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../../live/connection";
import { subjectKey } from "./sharing";

// Who the owner may lend a machine to (epic memql#5344, design section 4):
// the `fleetShareDirectory` builtin, read ON DEMAND.
//
// ===========================================================================
// NOT LIVE, AND READ ONCE PER OPENING
// ===========================================================================
// The directory is a virtual row computed per request from three concepts --
// users, groups, memberships -- so there is no event behind it to fold, and a
// collection over it would render "Loading" and then a list that never moves.
// It is read when the dialog OPENS, which is the one moment the answer is
// needed, and once: typing in the search narrows what was read rather than
// asking again.
//
// ===========================================================================
// A FAILED READ IS NOT AN EMPTY DIRECTORY
// ===========================================================================
// "Nobody shares a group with you yet" is a specific claim about the cluster,
// and a read that did not happen is not evidence for it. So the three states
// are kept apart all the way to the pixel: loading, failed (with the engine's
// words and a way to ask again), and an answer -- which may itself be empty.

/** Somebody the owner may pick. `detail` is an email, sent only to a caller
 *  at admin rank or above, who already sees it in Users. */
export interface DirectoryPerson {
  id: string;
  name: string;
  detail: string;
}

/** A group the owner may pick, with how many ACTIVE people are in it. */
export interface DirectoryGroup {
  id: string;
  name: string;
  members: number;
}

/**
 * A subject already on the machine's list.
 *
 * `known` false: no row names it any more (a deleted person or group) -- shown
 * as unknown and removable, never guessed at. `inDirectory` false: it is still
 * on the list but is no longer one this owner could pick (they left the
 * group) -- marked, removable, and sent back untouched on a save.
 */
export interface StoredSubject {
  id: string;
  name: string;
  known: boolean;
  inDirectory: boolean;
}

export interface ShareDirectory {
  /** The caller is at admin rank or above, so the directory is everyone. */
  everyone: boolean;
  people: DirectoryPerson[];
  groups: DirectoryGroup[];
  current: { people: StoredSubject[]; groups: StoredSubject[] };
}

export type DirectoryRead =
  | { state: "loading" }
  | { state: "ready"; directory: ShareDirectory }
  | { state: "failed"; error: string };

function objectAt(v: unknown): Record<string, unknown> | null {
  return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
}

function text(v: unknown): string {
  return typeof v === "string" ? v.trim() : "";
}

function entries(v: unknown): Record<string, unknown>[] {
  if (!Array.isArray(v)) return [];
  return v.map(objectAt).filter((e): e is Record<string, unknown> => e !== null && text(e["id"]) !== "");
}

/**
 * Read the directory row the builtin answers.
 *
 * NULL FOR A ROW THAT IS NOT ONE -- the engine always answers exactly one, so
 * an absent or malformed reply is a failed read, never an empty directory.
 *
 * `inDirectory` is the engine's own answer when it gives one, and is otherwise
 * derived from the offered lists it is computed from (offersPerson /
 * offersGroup), so a subject is never marked unavailable on a field that was
 * simply not sent.
 */
export function shareDirectoryFrom(row: unknown): ShareDirectory | null {
  const fields = objectAt(row);
  if (fields === null || !Array.isArray(fields["people"]) || !Array.isArray(fields["groups"])) return null;
  const people = entries(fields["people"]).map((p) => ({
    id: text(p["id"]),
    name: text(p["name"]) || "Unnamed person",
    detail: text(p["detail"]),
  }));
  const groups = entries(fields["groups"]).map((g) => {
    const members = Number(g["members"]);
    return {
      id: text(g["id"]),
      name: text(g["name"]) || "Unnamed group",
      members: Number.isFinite(members) && members > 0 ? Math.floor(members) : 0,
    };
  });
  const offered = new Set([
    ...people.map((p) => subjectKey("person", p.id)),
    ...groups.map((g) => subjectKey("group", g.id)),
  ]);
  const stored = (v: unknown, kind: "person" | "group"): StoredSubject[] =>
    entries(v).map((s) => {
      const id = text(s["id"]);
      const name = text(s["name"]);
      return {
        id,
        name,
        known: s["known"] !== false && name !== "",
        inDirectory:
          typeof s["inDirectory"] === "boolean" ? s["inDirectory"] : offered.has(subjectKey(kind, id)),
      };
    });
  const current = objectAt(fields["current"]);
  return {
    everyone: fields["everyone"] === true,
    people,
    groups,
    current: { people: stored(current?.["people"], "person"), groups: stored(current?.["groups"], "group") },
  };
}

function describe(err: unknown): string {
  const raw = err instanceof Error ? err.message : String(err);
  return raw.trim() === "" ? "The cluster refused and said nothing about why." : raw.trim();
}

/** One read of the directory for one of the caller's own machines. Rejects
 *  with the engine's words, or with a sentence of ours for a reply that is
 *  not a directory. */
export async function readShareDirectory(
  connection: Connection,
  registrationId: string,
  signal?: AbortSignal,
): Promise<ShareDirectory> {
  const result = await connection.query.fleetShareDirectory({ registrationId }, { signal });
  const directory = shareDirectoryFrom(result.rows()[0] ?? null);
  if (directory === null) throw new Error("The cluster answered without saying who you can share with.");
  return directory;
}

/**
 * The directory, read once when the component mounts -- which for the share
 * dialog is once per opening -- and again only when `retry` asks.
 */
export function useShareDirectory(registrationId: string): { read: DirectoryRead; retry: () => void } {
  const connection = useOsConnection();
  const [read, setRead] = useState<DirectoryRead>({ state: "loading" });
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    if (connection === null) {
      setRead({ state: "failed", error: "Not connected to the cluster, so nobody could be looked up." });
      return;
    }
    const controller = new AbortController();
    let cancelled = false;
    setRead({ state: "loading" });
    readShareDirectory(connection, registrationId, controller.signal).then(
      (directory) => {
        if (!cancelled) setRead({ state: "ready", directory });
      },
      (err: unknown) => {
        if (!cancelled) setRead({ state: "failed", error: describe(err) });
      },
    );
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [connection, registrationId, attempt]);

  const retry = useCallback(() => setAttempt((n) => n + 1), []);
  return { read, retry };
}
