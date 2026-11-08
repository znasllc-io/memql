import { useCallback, useMemo, useRef, useState } from "react";

import { Button, Caption, Notice, RecordList, RecordListSkeleton, RecordRow, Subhead } from "../../kit";
import { AddButton } from "../../kit/AddButton";
import { SearchPicker } from "../../kit/SearchPicker";
import type { OsRegistry } from "../../system/registry";
import { holds } from "../../system/roles";
import {
  accessResources,
  appsToAdd,
  bareId,
  grantRefusalCopy,
  groupApps,
  type AccessGrant,
  type AccessResource,
  type GrantRefusal,
  type Subject,
} from "../settings/access/model";
import { useEffectiveAccess, useGrantWrites, useSubjectGrants } from "../settings/access/useAccess";
import type { Refusal } from "./refusals";
import type { GroupRow } from "./rows";
import type { RoleCatalog } from "./useRoles";

// THE APPS AN ORGANIZATION'S PEOPLE CAN OPEN, set where the organization is:
// its group's page (access clarity, 2026-11-06).
//
// The rows are the ENGINE'S answer for the group (`effectiveAccessForSubject`:
// one row per grant it holds), drawn as its ALLOW grants on apps -- each app a
// row, the parts of it allowed beneath (access/model.ts, `groupApps`). A deny
// opens nothing through the group, so it stays on Settings > Access, where it
// can be lifted. "Remove" revokes the group's own grant on that pair, its id
// from `grantsForSubject`, since the engine's rows carry none; "Add an app"
// offers the catalog's apps the group does not already allow and grants
// `read app:<id>`. Both writes are Settings > Access's own (`useGrantWrites`),
// so the engine judges them as it judges every grant.
//
// WHO READS IT is whoever holds Settings > Access, as the button beside it
// (GroupPage). WHO ACTS is whoever writes grants, `update` on `principal`: for
// anybody else the acts are absent, never disabled (DESIGN.md rule 12). A
// member's role can still bar an app the group allows -- an organization role
// opens no cluster app -- which Settings > Access says person by person.

export function OrganizationApps({ group, catalog, registry }: { group: GroupRow; catalog: RoleCatalog; registry: OsRegistry }) {
  const subject = useMemo<Subject>(() => ({ kind: "group", id: bareId(group.id), name: group.name, role: "" }), [group.id, group.name]);
  const effective = useEffectiveAccess(subject);
  const grants = useSubjectGrants(subject);
  const reloadAnswers = effective.reload;
  const reloadGrants = grants.reload;
  const onWritten = useCallback(() => {
    reloadAnswers();
    reloadGrants();
  }, [reloadAnswers, reloadGrants]);
  const writes = useGrantWrites(onWritten);
  const [adding, setAdding] = useState(false);
  // The picker closes back onto the Add control it was opened from, so a
  // keyboard is never left on a field that has gone.
  const addControl = useRef<HTMLSpanElement>(null);
  const stopAdding = useCallback(() => {
    setAdding(false);
    addControl.current?.querySelector<HTMLButtonElement>("button")?.focus();
  }, []);

  const resources = useMemo(
    () => accessResources(registry, catalog.grants, effective.rows.map((r) => r.resource)),
    [registry, catalog.grants, effective.rows],
  );
  const apps = useMemo(() => groupApps(effective.rows, resources, grants.own, subject), [effective.rows, resources, grants.own, subject]);
  const addable = useMemo(() => appsToAdd(effective.rows, resources), [effective.rows, resources]);

  const ready = effective.state === "ready";
  const canWrite = ready && holds("update", "principal");
  const busy = writes.busy || effective.pending || grants.pending;

  const remove = (grant: AccessGrant | null, resource: AccessResource) =>
    canWrite && grant !== null ? (
      <Button disabled={busy} onClick={() => void writes.revoke(grant.id, resource.resource)} ariaLabel={`Remove ${resource.label} from ${group.name}`}>
        Remove
      </Button>
    ) : undefined;

  return (
    <>
      <Subhead actions={canWrite && addable.length > 0 ? <span ref={addControl}><AddButton label="Add an app" disabled={busy} onClick={() => setAdding(true)} /></span> : undefined}>
        Apps its people can open
      </Subhead>
      {effective.state === "refused" ? (
        // IN PLACE OF THE LIST, and with no acts: nothing here is known.
        <ReadRefusal refusal={effective.refusal} />
      ) : !ready ? (
        <RecordListSkeleton label="Reading the apps this group opens." rows={2} />
      ) : (
        <>
          {adding && canWrite ? (
            <SearchPicker
              id={`group-apps-${subject.id}`}
              label="An app to add"
              listLabel="Apps to add"
              choices={addable.map((door) => ({ id: door.resource, name: door.label }))}
              emptyText="No app by that name, or the group already opens it."
              autoFocus
              onDismiss={stopAdding}
              onChoose={(choice) => {
                stopAdding();
                const door = addable.find((r) => r.resource === choice.id);
                if (door !== undefined) void writes.set(subject, door.verb, door.resource, "allow");
              }}
            />
          ) : null}
          {grants.state === "error" ? <Notice tone="warn" sentence="This group's grants could not be read, so none can be removed here." detail={grants.error} /> : null}
          {apps.length === 0 ? (
            <Caption>No app is opened through this group yet.</Caption>
          ) : (
            <RecordList as="ul" density="compact" className="os-access-list" label="Apps its people can open">
              {apps.flatMap(({ door, grant, parts }) => [
                <li key={door.resource}>
                  <RecordRow name={door.label} actions={remove(grant, door)} />
                </li>,
                ...parts.map(({ part, grant: partGrant }) => (
                  <li key={part.resource} data-part="">
                    <RecordRow name={part.label} actions={remove(partGrant, part)} />
                  </li>
                )),
              ])}
            </RecordList>
          )}
        </>
      )}
      {writes.refusal !== null ? <WriteRefusal refusal={writes.refusal} /> : null}
    </>
  );
}

/** The engine's refusal to say what the group grants, in place of the list. */
function ReadRefusal({ refusal }: { refusal: Refusal | null }) {
  const detail = refusal === null || refusal.detail === "" ? undefined : refusal.code === "" ? refusal.detail : `${refusal.code}: ${refusal.detail}`;
  return (
    <Notice
      tone="warn"
      sentence={refusal !== null && refusal.title !== "" ? refusal.title : "What this group grants could not be read."}
      next={refusal === null || refusal.next === "" ? undefined : refusal.next}
      detail={detail}
    />
  );
}

/** A refused grant write, in the words Settings > Access gives its code. */
function WriteRefusal({ refusal }: { refusal: GrantRefusal }) {
  const copy = grantRefusalCopy(refusal.code);
  return (
    <Notice
      tone="warn"
      sentence={copy === null ? "The cluster refused this." : copy.title}
      next={copy === null || copy.next === "" ? undefined : copy.next}
      detail={refusal.code === "" ? refusal.message : `${refusal.code}: ${refusal.message}`}
    />
  );
}
