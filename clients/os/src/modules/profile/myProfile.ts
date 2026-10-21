import { useEffect, useSyncExternalStore } from "react";
import { rowString, type Connection, type Row } from "@znasllc-io/memql-sdk-core/client";

import { boolOr, flatten } from "../../kit/rows";
import { useOsConnection } from "../../live/connection";

// THE SIGNED-IN PERSON'S OWN ROW (memql#6220), read once per connection and
// shared by everything that draws them: the dock's avatar, Ask's "You" line
// and the Profile page.
//
// ONE COPY, NOT ONE READ PER AVATAR. The avatar renders in the dock and on
// every turn of an Ask conversation; a read per instance would be a dozen
// identical `currentUser` calls for one picture, and a photo changed in the
// Profile would reach the copy that wrote it and none of the others. So the
// row lives here, module-wide, and a write that lands patches it in place --
// `v1:identity:user` broadcasts creates only (usePeople.ts carries why), so no
// event would ever deliver the change.
//
// A NEW CONNECTION IS A NEW PERSON. Signing out and in as somebody else hands
// the shell a new Connection, and the cached row belongs to whoever was signed
// in before it, so the cache is keyed by the connection that read it.

export type SignInPolicy = "" | "any" | "passkey_only";

export interface MyProfile {
  userId: string;
  displayName: string;
  firstName: string;
  lastName: string;
  email: string;
  /** The cluster role's slug. The Profile shows the catalog's name for it. */
  role: string;
  phone: string;
  /** Job title (`primaryRole`): admin-curated directory data, not the auth role. */
  title: string;
  /** A data: URL, or "" for none. */
  avatar: string;
  signInPolicy: SignInPolicy;
  /** The computer-use kill switch. ABSENT means on: the concept's default is
   *  true and is never written at creation. */
  computerUseEnabled: boolean;
  /** IANA name, or "" for UTC. */
  timezone: string;
}

export function profileFromRow(raw: Row): MyProfile {
  const row = flatten(raw);
  const prefs = row["preferences"] && typeof row["preferences"] === "object" && !Array.isArray(row["preferences"])
    ? (row["preferences"] as Row)
    : {};
  const policy = rowString(row, "signInPolicy");
  return {
    userId: rowString(row, "id"),
    displayName: rowString(row, "displayName"),
    firstName: rowString(row, "firstName"),
    lastName: rowString(row, "lastName"),
    email: rowString(row, "primaryEmail"),
    role: rowString(row, "role"),
    phone: rowString(row, "phone"),
    title: rowString(row, "primaryRole"),
    avatar: rowString(row, "avatar"),
    signInPolicy: policy === "any" || policy === "passkey_only" ? policy : "",
    computerUseEnabled: boolOr(prefs, "computerUseEnabled", true),
    timezone: rowString(prefs, "timezone"),
  };
}

/** What to call the person: the display name, then the name parts, then the
 *  address. Never blank while there is anything to show. */
export function profileName(profile: Pick<MyProfile, "displayName" | "firstName" | "lastName" | "email">): string {
  const display = profile.displayName.trim();
  if (display !== "") return display;
  const parts = `${profile.firstName} ${profile.lastName}`.trim();
  return parts !== "" ? parts : profile.email.trim();
}

/** The one letter drawn when there is no photo. */
export function profileInitial(name: string): string {
  const first = Array.from(name.trim())[0];
  return first ? first.toUpperCase() : "?";
}

export type MyProfileState = "reading" | "ready" | "failed";

export interface MyProfileSnapshot {
  profile: MyProfile | null;
  state: MyProfileState;
  /** The engine's refusal, verbatim, when the read failed. */
  error: string;
}

const READING: MyProfileSnapshot = { profile: null, state: "reading", error: "" };

let snapshot: MyProfileSnapshot = READING;
let readFor: Connection | null = null;
let generation = 0;
const listeners = new Set<() => void>();

function publish(next: MyProfileSnapshot): void {
  snapshot = next;
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function current(): MyProfileSnapshot {
  return snapshot;
}

/** Read the row again, keeping what is on screen until the answer lands. */
export function rereadMyProfile(connection: Connection): Promise<void> {
  const mine = ++generation;
  readFor = connection;
  if (snapshot.profile === null) publish({ ...snapshot, state: "reading", error: "" });
  // Through a promise, so a connection that cannot make the read at all (a
  // half-built double, a client older than the query) lands as a failed read
  // rather than an exception thrown into whichever component asked first.
  return Promise.resolve()
    .then(() => connection.query.currentUser({}))
    .then((result) => {
      if (mine !== generation) return;
      const row = result.rows()[0];
      if (row === undefined) {
        publish({ profile: null, state: "failed", error: "The cluster returned no profile for this account." });
        return;
      }
      publish({ profile: profileFromRow(row), state: "ready", error: "" });
    })
    .catch((err: unknown) => {
      if (mine !== generation) return;
      publish({ ...snapshot, state: snapshot.profile === null ? "failed" : "ready", error: err instanceof Error ? err.message : String(err) });
    });
}

/** Read once per connection; a second caller shares the first one's read. */
export function ensureMyProfile(connection: Connection): void {
  if (readFor === connection) return;
  publish(READING);
  void rereadMyProfile(connection);
}

/** Apply a write the engine accepted, everywhere the person is drawn. */
export function patchMyProfile(patch: Partial<MyProfile>): void {
  if (snapshot.profile === null) return;
  publish({ ...snapshot, profile: { ...snapshot.profile, ...patch } });
}

/** The signed-in person's row, read on first use. */
export function useMyProfile(): MyProfileSnapshot & { reread: () => void } {
  const connection = useOsConnection();
  useEffect(() => {
    if (connection !== null) ensureMyProfile(connection);
  }, [connection]);
  const value = useSyncExternalStore(subscribe, current, current);
  return {
    ...value,
    reread: () => {
      if (connection !== null) void rereadMyProfile(connection);
    },
  };
}

/** Tests only: forget the cached person and every listener's view of them. */
export function resetMyProfileForTest(): void {
  snapshot = READING;
  readFor = null;
  generation++;
}
