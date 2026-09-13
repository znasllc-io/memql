import { useCallback, useEffect, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../../live/connection";
import { flatten } from "../../../kit/rows";
import { notifyGrantWritten } from "../../../system/roles";
import {
  grantFromRow as roleGrantFromRow,
  groupFromRow,
  membershipFromRow,
  personFromRow,
  personName,
  roleFromRow,
  type GrantRow as RoleGrantRow,
  type GroupRow,
  type PersonRow,
  type RoleRow,
} from "../../users/rows";
import { grantFromRow, type AccessGrant, type GrantRefusal, type Subject } from "./model";

// SETTINGS > ACCESS, THE READS AND THE TWO WRITES (epic memql#5289, task
// memql#5307).
//
// ===========================================================================
// ON-DEMAND READS, RE-READ AFTER EVERY WRITE
// ===========================================================================
// Grant rows are cluster-owner tier and never broadcast (design D11), so
// there is no feed to subscribe to and a LiveCollection would render
// "Loading from the cluster" forever. Every read here is a plain call, made
// when the subject or the resource changes and again after a write -- the
// write's reply is the engine's decision, and the re-read is what shows it
// took. The roster and the catalog are read once per connection; the
// subject's memberships and grants are read per subject.
//
// A write also tells the session scope (`notifyGrantWritten`), so the
// viewer's OWN effective set is re-read: a grant to a group the viewer is
// in changes what they may open, and the launcher must follow.

/** The roster and the catalog: who there is to grant to, and what roles hold. */
export interface AccessRoster {
  people: PersonRow[];
  groups: GroupRow[];
  roles: RoleRow[];
  catalog: RoleGrantRow[];
  state: "loading" | "ready" | "error";
  error: string;
  reload: () => void;
}

export function useAccessRoster(): AccessRoster {
  const connection = useOsConnection();
  const [people, setPeople] = useState<PersonRow[]>([]);
  const [groups, setGroups] = useState<GroupRow[]>([]);
  const [roles, setRoles] = useState<RoleRow[]>([]);
  const [catalog, setCatalog] = useState<RoleGrantRow[]>([]);
  const [state, setState] = useState<"loading" | "ready" | "error">("loading");
  const [error, setError] = useState("");
  const [nonce, setNonce] = useState(0);
  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null) return;
    const controller = new AbortController();
    let live = true;
    void (async () => {
      try {
        // TOGETHER, so the matrix never draws people against an empty
        // catalog: a subject with every cell dashed is a confident picture of
        // a person holding nothing.
        const [peopleResult, groupResult, roleResult, catalogResult] = await Promise.all([
          query.searchUsers({}, { signal: controller.signal }),
          query.groupsAll({ includeArchived: false }, { signal: controller.signal }),
          query.activeRoles({}, { signal: controller.signal }),
          query.activeCapabilities({}, { signal: controller.signal }),
        ]);
        if (!live) return;
        setPeople(
          (peopleResult.rows() as Row[])
            .map(personFromRow)
            .filter((p) => p.id !== "")
            .sort((a, b) => personName(a).localeCompare(personName(b))),
        );
        setGroups(
          (groupResult.rows() as Row[])
            .map(groupFromRow)
            .filter((g) => g.id !== "")
            .sort((a, b) => a.name.localeCompare(b.name)),
        );
        setRoles((roleResult.rows() as Row[]).map(roleFromRow).filter((r) => r.slug !== ""));
        setCatalog((catalogResult.rows() as Row[]).map(roleGrantFromRow).filter((g) => g.roleSlug !== ""));
        setState("ready");
        setError("");
      } catch (err: unknown) {
        if (!live) return;
        setState("error");
        setError(err instanceof Error ? err.message : String(err));
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
  }, [connection, nonce]);

  return { people, groups, roles, catalog, state, error, reload };
}

/** What one subject holds: their own grants and, for a person, every grant of every group they are in. */
export interface SubjectGrants {
  own: AccessGrant[];
  /** The subject's groups (a person); the group ids the group-level grants came from. */
  groupIds: string[];
  groupLevel: AccessGrant[];
  state: "idle" | "loading" | "ready" | "error";
  error: string;
  reload: () => void;
}

export function useSubjectGrants(subject: Subject | null): SubjectGrants {
  const connection = useOsConnection();
  const [own, setOwn] = useState<AccessGrant[]>([]);
  const [groupIds, setGroupIds] = useState<string[]>([]);
  const [groupLevel, setGroupLevel] = useState<AccessGrant[]>([]);
  const [state, setState] = useState<"idle" | "loading" | "ready" | "error">("idle");
  const [error, setError] = useState("");
  const [nonce, setNonce] = useState(0);
  const reload = useCallback(() => setNonce((n) => n + 1), []);

  const kind = subject?.kind ?? "user";
  const id = subject?.id ?? "";

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null || id === "") {
      setState("idle");
      setOwn([]);
      setGroupIds([]);
      setGroupLevel([]);
      return;
    }
    const controller = new AbortController();
    let live = true;
    setState("loading");
    void (async () => {
      try {
        const ownResult = await query.grantsForSubject({ subjectKind: kind, subjectId: id }, { signal: controller.signal });
        const ownGrants = (ownResult.rows() as Row[]).map(grantFromRow).filter((g) => g.id !== "");
        let ids: string[] = [];
        let viaGroups: AccessGrant[] = [];
        if (kind === "user") {
          const memberships = await query.groupsForUser({ userId: id, includeRemoved: false }, { signal: controller.signal });
          ids = (memberships.rows() as Row[])
            .map(membershipFromRow)
            .filter((m) => m.status === "active" && m.groupId !== "")
            .map((m) => m.groupId);
          const perGroup = await Promise.all(
            ids.map((groupId) => query.grantsForSubject({ subjectKind: "group", subjectId: groupId }, { signal: controller.signal })),
          );
          viaGroups = perGroup.flatMap((r) => (r.rows() as Row[]).map(grantFromRow).filter((g) => g.id !== ""));
        }
        if (!live) return;
        setOwn(ownGrants);
        setGroupIds(ids);
        setGroupLevel(viaGroups);
        setState("ready");
        setError("");
      } catch (err: unknown) {
        if (!live) return;
        setState("error");
        setError(err instanceof Error ? err.message : String(err));
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
  }, [connection, kind, id, nonce]);

  return { own, groupIds, groupLevel, state, error, reload };
}

/** Who holds one resource by grant, for the by-app view. */
export interface ResourceGrants {
  grants: AccessGrant[];
  state: "idle" | "loading" | "ready" | "error";
  error: string;
  reload: () => void;
}

export function useResourceGrants(resource: string): ResourceGrants {
  const connection = useOsConnection();
  const [grants, setGrants] = useState<AccessGrant[]>([]);
  const [state, setState] = useState<"idle" | "loading" | "ready" | "error">("idle");
  const [error, setError] = useState("");
  const [nonce, setNonce] = useState(0);
  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    const query = connection?.query ?? null;
    if (query === null || resource === "") {
      setState("idle");
      setGrants([]);
      return;
    }
    const controller = new AbortController();
    let live = true;
    setState("loading");
    void (async () => {
      try {
        const result = await query.grantsForResource({ resourceType: resource }, { signal: controller.signal });
        if (!live) return;
        setGrants((result.rows() as Row[]).map(grantFromRow).filter((g) => g.id !== "" && g.active));
        setState("ready");
        setError("");
      } catch (err: unknown) {
        if (!live) return;
        setState("error");
        setError(err instanceof Error ? err.message : String(err));
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
  }, [connection, resource, nonce]);

  return { grants, state, error, reload };
}

/** The two writes, with one busy flag and the refusal beside the control that asked. */
export interface GrantWrites {
  busy: boolean;
  /** The last refusal, keyed to the resource it was about so it renders beside that row. */
  refusal: (GrantRefusal & { resource: string }) | null;
  set: (subject: Subject, verb: string, resource: string, effect: "allow" | "deny") => Promise<boolean>;
  revoke: (grantId: string, resource: string) => Promise<boolean>;
  clear: () => void;
}

export function useGrantWrites(onWritten: () => void): GrantWrites {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<(GrantRefusal & { resource: string }) | null>(null);

  const decide = useCallback(
    async (resource: string, call: () => Promise<{ rows: () => Row[] }>): Promise<boolean> => {
      setBusy(true);
      setRefusal(null);
      try {
        const result = await call();
        const row = result.rows()[0];
        const flat = row === undefined ? {} : flatten(row);
        if (flat["ok"] === true) {
          onWritten();
          // The viewer's own set may have moved: a grant to a group they are
          // in, or a deny on themselves written by nobody (refused) -- the
          // session scope re-reads and decides.
          notifyGrantWritten();
          return true;
        }
        setRefusal({
          resource,
          code: typeof flat["code"] === "string" ? flat["code"] : "",
          message: typeof flat["message"] === "string" ? flat["message"] : "The cluster refused this without saying why.",
        });
        return false;
      } catch (err: unknown) {
        setRefusal({ resource, code: "", message: err instanceof Error ? err.message : String(err) });
        return false;
      } finally {
        setBusy(false);
      }
    },
    [onWritten],
  );

  const set = useCallback<GrantWrites["set"]>(
    (subject, verb, resource, effect) => {
      const query = connection?.query ?? null;
      if (query === null) {
        setRefusal({ resource, code: "", message: "Not connected to the cluster, so nothing was written." });
        return Promise.resolve(false);
      }
      return decide(resource, () =>
        query.grantSet({ subjectKind: subject.kind, subjectId: subject.id, verb, resourceType: resource, effect }),
      );
    },
    [connection, decide],
  );

  const revoke = useCallback<GrantWrites["revoke"]>(
    (grantId, resource) => {
      const query = connection?.query ?? null;
      if (query === null) {
        setRefusal({ resource, code: "", message: "Not connected to the cluster, so nothing was written." });
        return Promise.resolve(false);
      }
      return decide(resource, () => query.grantRevoke({ grantId }));
    },
    [connection, decide],
  );

  const clear = useCallback(() => setRefusal(null), []);

  return useMemo(() => ({ busy, refusal, set, revoke, clear }), [busy, refusal, set, revoke, clear]);
}
