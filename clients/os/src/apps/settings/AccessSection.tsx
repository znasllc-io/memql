import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { LocalTabs } from "../../kit/LocalTabs";
import type { OsAppProps } from "../../system/registry";
import { useCallback, useEffect, useMemo, useState } from "react";
import { Check } from "lucide-react";

import { Caption, RecordList, RecordRow, Field, Head, Notice, Panel, Select, Subhead } from "../../kit";
import { useSession } from "../../chrome/access";
import { useOs } from "../../chrome/state";
import { holds } from "../../system/roles";
import { personName, type GroupRow, type PersonRow } from "../users/rows";
import {
  accessResources,
  bareId,
  grantRefusalCopy,
  resolveAnswer,
  rolesHolding,
  type AccessGrant,
  type AccessResource,
  type Answer,
  type GrantRefusal,
  type Subject,
} from "./access/model";
import { useAccessRoster, useGrantWrites, useResourceGrants, useSubjectGrants } from "./access/useAccess";

// SETTINGS > ACCESS (epic memql#5289, task memql#5307; design section 4
// "Administration surface, minimal"; DESIGN.md).
//
// ===========================================================================
// WHAT THIS SCREEN IS FOR, AND WHAT IT IS NOT
// ===========================================================================
// Two people share a cluster and one of them wants to hand the other an app,
// or a part of one, without changing their role. This is where that is done
// and where the answer to "does Ada have Deployables?" is read: one subject
// at a time, every app and part in a column, each cell saying what the
// answer is AND where it came from -- the role, a group, or a grant by name.
// The by-app view is the same rows turned around: one resource, who holds it.
//
// Minimal by the owner's instruction: no drag and drop, no bulk edit, no
// invitations from here. The Users app is where people and groups are made.
//
// ===========================================================================
// THE ACTS FOLLOW THE STATE, AND ONLY THE ACT THAT CHANGES IT IS OFFERED
// ===========================================================================
// A row whose answer is inherited offers the one act that would flip it:
// Deny on a held app, Allow on one the role lacks. A row a grant decided
// offers the flip and "Use role", which revokes the grant. Never both Allow
// and Deny beside an answer that is already one of them -- a button that
// would write what already stands is a button somebody has to read past.
// Absent for a viewer whose role does not write grants (rule 12: an act
// that is not legal is absent, never disabled), and the caption says why.
//
// ===========================================================================
// THE REFUSAL SITS BESIDE THE ROW THAT ASKED
// ===========================================================================
// The engine's governance codes (integrations/rbac/grants.go) are printed
// under the row whose act was refused, with copy this build carries and the
// server's own sentence beneath. A refusal at the bottom of a twenty-row
// table is a refusal nobody connects to the click.
//
// ===========================================================================
// THE ONE SIGNATURE ELEMENT IS THE PROVENANCE
// ===========================================================================
// The Users app's role grid says whether a role holds a pair. This screen
// adds the one thing that grid cannot: where the answer came from, in words
// beside the mark. Everything else -- the table, the marks, the select, the
// notice -- is the kit's, so the screen reads as the Users grid's sibling
// rather than as a second permissions language.
//
// ===========================================================================
// EVERY "IS THIS YOU" COMPARISON IS MADE ON BARE IDS
// ===========================================================================
// Two id spellings reach this screen. Grant rows arrive bare (`grantFromRow`
// bare-ifies `subjectId` and `grantedBy`), and so does the roster -- but the
// session's own `access.userId` is whatever the token carried, which in a
// deployed cluster is routinely the canonical `v1:identity:user:...`. A raw
// `===` between the two forms is false for the one person it most needs to be
// true for, and it fails QUIETLY in both directions: Allow and Deny stay
// offered beside your own name (the engine then refuses `grant_self`, so the
// screen advertises an act the cluster will not perform), and "by you"
// degrades to your own display name as though a colleague had written it.
// `bareId` is applied on BOTH sides of every self/you decision here, the way
// `integrations/rbac/grants.go` compares with `sameUser` and Deployables'
// attribution compares with `bare`.

export const ACCESS_SECTION_RESOURCE = "app:settings/access";

type View = "subject" | "resource";

export function AccessSection({ intent, consumeIntent }: Pick<OsAppProps, "intent" | "consumeIntent"> = {}) {
  const { access, accessEpoch } = useSession();
  const { registry } = useOs();
  // BARE, ONCE, AT THE SOURCE: the session's spelling is the token's, and
  // every comparison below is against a bare grant or roster id.
  const viewerUserId = bareId(access?.userId ?? "");
  // WHETHER THE VIEWER WRITES GRANTS: `update` on `principal`, read off the
  // effective set (governance rule 1). Presentation -- the engine checks it
  // again on every write -- but a control that can only be refused is not
  // offered. `accessEpoch` is the dep, so this follows the set when it lands.
  const canWrite = useMemo(() => holds("update", "principal"), [accessEpoch]);

  const roster = useAccessRoster();
  const [view, setView] = useState<View>("subject");
  const [subjectKey, setSubjectKey] = useState("");
  const [resourceKey, setResourceKey] = useState("");
  useEffect(() => {
    const groupId = intent?.payload["groupId"];
    if (typeof groupId !== "string" || !groupId || !intent) return;
    setView("subject");
    setSubjectKey(`group:${groupId}`);
    consumeIntent?.(intent.id);
  }, [intent, consumeIntent]);

  const subject = useMemo<Subject | null>(() => subjectFromKey(subjectKey, roster.people, roster.groups), [subjectKey, roster.people, roster.groups]);
  const subjectGrants = useSubjectGrants(subject);
  const resources = useMemo(
    () => accessResources(registry, roster.catalog, [...subjectGrants.own, ...subjectGrants.groupLevel].map((g) => g.resource)),
    [registry, roster.catalog, subjectGrants.own, subjectGrants.groupLevel],
  );
  const resourceGrants = useResourceGrants(view === "resource" ? resourceKey : "");

  const onWritten = useCallback(() => {
    subjectGrants.reload();
    resourceGrants.reload();
  }, [subjectGrants, resourceGrants]);
  const writes = useGrantWrites(onWritten);

  const nameOf = useMemo(() => namer(roster.people, roster.groups, viewerUserId), [roster.people, roster.groups, viewerUserId]);
  // THE SUBJECT IS THE VIEWER: the one state in which no act is offered,
  // because rule 4 of the engine's governance (`grant_self`) refuses it.
  const viewingSelf = subject !== null && subject.kind === "user" && bareId(subject.id) === viewerUserId;

  return (
    <div className="os-settings">
      <Head title="Access" />
      <LocalTabs label="Access views" value={view} onChange={value => { writes.clear(); setView(value); }} options={[["subject", "By person or group"], ["resource", "By app"]]} />
      <Caption>
        Who may open which app, and which parts of one, over and above their role. A role decides first; a
        group the person is in can widen or narrow that; a grant to the person by name decides last.
      </Caption>

      {roster.state === "error" ? (
        <Notice tone="warn" sentence="The roster could not be read." detail={roster.error} />
      ) : null}

      {canWrite ? null : (
        <Caption>
          You can read who holds what. Writing a grant takes <span className="os-mono">update</span> on{" "}
          <span className="os-mono">principal</span>, which your role does not carry.
        </Caption>
      )}

      {view === "subject" ? (
        <Panel label="Access by person or group">
          <Field label="Person or group">
          <Select id="os-access-subject" label="Person or group" value={subjectKey} onChange={(next) => { writes.clear(); setSubjectKey(next); }}>
            <option value="">Choose a person or a group</option>
            {roster.groups.length === 0 ? null : (
              <optgroup label="Groups">
                {roster.groups.map((g) => (
                  <option key={g.id} value={`group:${g.id}`}>
                    {g.name}
                  </option>
                ))}
              </optgroup>
            )}
            {roster.people.length === 0 ? null : (
              <optgroup label="People">
                {roster.people.map((p) => (
                  <option key={p.id} value={`user:${p.id}`}>
                    {personName(p)}
                  </option>
                ))}
              </optgroup>
            )}
          </Select>
          </Field>

          {subject === null ? (roster.state === "loading" ? <RecordListSkeleton label="Reading the roster." /> : <Caption>{"Pick somebody to see what they may open, and where each answer comes from."}</Caption>) : (
            <>
              <Caption>{subjectSentence(subject, subjectGrants.groupIds, roster.groups, roster.roles)}</Caption>
              {subjectGrants.state === "error" ? (
                <Notice tone="warn" sentence="Their grants could not be read." detail={subjectGrants.error} />
              ) : null}
              <AccessMatrix
                subject={subject}
                resources={resources}
                catalog={roster.catalog}
                groupGrants={subjectGrants.groupLevel}
                ownGrants={subjectGrants.own}
                groupNameOf={(id) => roster.groups.find((g) => bareId(g.id) === bareId(id))?.name ?? "a group"}
                nameOf={nameOf}
                canWrite={canWrite && !viewingSelf}
                busy={writes.busy || subjectGrants.state === "loading"}
                refusal={writes.refusal}
                onAllow={(r) => void writes.set(subject, r.verb, r.resource, "allow")}
                onDeny={(r) => void writes.set(subject, r.verb, r.resource, "deny")}
                onRevoke={(grant, r) => void writes.revoke(grant.id, r.resource)}
              />
              {viewingSelf && canWrite ? (
                <Caption>This is you. Nobody grants to themselves; ask a colleague who holds the app.</Caption>
              ) : null}
            </>
          )}
        </Panel>
      ) : (
        <Panel label="Access by app">
          <Field label="App or part">
          <Select id="os-access-resource" label="App or part" value={resourceKey} onChange={(next) => { writes.clear(); setResourceKey(next); }}>
            <option value="">Choose an app or a part</option>
            {resources.map((r) => (
              <option key={r.resource} value={r.resource}>
                {r.part === "" ? r.label : `${appLabel(resources, r)}: ${r.label}`}
              </option>
            ))}
          </Select>
          </Field>

          {resourceKey === "" ? (
            <Caption>Pick an app or a part to see who holds it: the roles the seeds give it to, and everyone granted it by name.</Caption>
          ) : (
            <ResourceHolders
              resource={resources.find((r) => r.resource === resourceKey) ?? null}
              roles={rolesHolding(roster.roles, roster.catalog, verbOf(resources, resourceKey), resourceKey).map((r) => r.name || r.slug)}
              grants={resourceGrants.grants}
              state={resourceGrants.state}
              error={resourceGrants.error}
              nameOf={nameOf}
              viewerUserId={viewerUserId}
              canWrite={canWrite}
              busy={writes.busy}
              refusal={writes.refusal}
              onRevoke={(grant) => void writes.revoke(grant.id, resourceKey)}
            />
          )}
        </Panel>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// The matrix
// ---------------------------------------------------------------------------

function AccessMatrix({
  subject,
  resources,
  catalog,
  groupGrants,
  ownGrants,
  groupNameOf,
  nameOf,
  canWrite,
  busy,
  refusal,
  onAllow,
  onDeny,
  onRevoke,
}: {
  subject: Subject;
  resources: readonly AccessResource[];
  catalog: Parameters<typeof resolveAnswer>[3];
  groupGrants: readonly AccessGrant[];
  ownGrants: readonly AccessGrant[];
  groupNameOf: (groupId: string) => string;
  nameOf: (userId: string) => string;
  canWrite: boolean;
  busy: boolean;
  refusal: (GrantRefusal & { resource: string }) | null;
  onAllow: (resource: AccessResource) => void;
  onDeny: (resource: AccessResource) => void;
  onRevoke: (grant: AccessGrant, resource: AccessResource) => void;
}) {
  if (resources.length === 0) {
    return <Caption>The catalog names no app yet, so there is nothing to grant.</Caption>;
  }
  return (
    <table className="os-grid os-access-grid" aria-label={`What ${subject.name} may open`}>
      <thead>
        <tr>
          <th scope="col">
            <span className="os-visually-hidden">App or part</span>
          </th>
          <th scope="col">
            <span className="os-visually-hidden">Answer, and where it came from</span>
          </th>
          <th scope="col">
            <span className="os-visually-hidden">Change</span>
          </th>
        </tr>
      </thead>
      <tbody>
        {resources.map((r) => {
          const answer = resolveAnswer(subject, r.verb, r.resource, catalog, groupGrants, ownGrants);
          const state = cellState(answer);
          const refused = refusal !== null && refusal.resource === r.resource ? refusal : null;
          return [
            <tr key={r.resource} data-resource={r.resource} data-part={r.part === "" ? undefined : ""}>
              <th scope="row">
                {r.part === "" ? r.label : <span className="os-access-part">{r.label}</span>}
              </th>
              <td className="os-grid-cell os-access-answer" data-state={state} data-source={answer.source}>
                <span className="os-grid-mark os-access-mark" aria-hidden>
                  {answer.held ? <Check size={12} aria-hidden /> : null}
                </span>
                <span className="os-access-from">{answerSentence(subject, answer, groupNameOf, nameOf)}</span>
              </td>
              <td className="os-access-acts">
                {canWrite ? (
                  <>
                    {answer.source === "user" && answer.grant !== null ? (
                      <button type="button" className="os-link" disabled={busy} onClick={() => onRevoke(answer.grant!, r)} aria-label={`Use the role's answer for ${r.label}`}>
                        Use role
                      </button>
                    ) : null}
                    {answer.held ? (
                      <button type="button" className="os-link" disabled={busy} onClick={() => onDeny(r)} aria-label={`Deny ${r.label} to ${subject.name}`}>
                        Deny
                      </button>
                    ) : (
                      <button type="button" className="os-link" disabled={busy} onClick={() => onAllow(r)} aria-label={`Allow ${r.label} to ${subject.name}`}>
                        Allow
                      </button>
                    )}
                  </>
                ) : null}
              </td>
            </tr>,
            refused === null ? null : (
              <tr key={`${r.resource}:refusal`} className="os-access-refusal">
                <td colSpan={3}>
                  <RefusalNotice refusal={refused} />
                </td>
              </tr>
            ),
          ];
        })}
      </tbody>
    </table>
  );
}

/** The mark's drawn state: held or not, and whether a grant decided it. */
function cellState(answer: Answer): "on" | "off" | "grantedOn" | "grantedOff" {
  if (answer.source === "role") return answer.held ? "on" : "off";
  return answer.held ? "grantedOn" : "grantedOff";
}

/** The provenance, in words: what the answer is and who gave it. */
function answerSentence(
  subject: Subject,
  answer: Answer,
  groupNameOf: (groupId: string) => string,
  nameOf: (userId: string) => string,
): string {
  if (answer.source === "role") {
    // The mark says held or not; the scope line above says which role. What
    // is left to say here is only that the role decided it.
    if (subject.kind === "group") return answer.held ? "allowed" : "nothing of its own";
    return `${subject.role || "unknown"} role`;
  }
  const word = answer.held ? "allowed" : "denied";
  const by = answer.grant === null ? "" : nameOf(answer.grant.grantedBy);
  if (answer.source === "group") {
    const group = answer.grant === null ? "a group" : groupNameOf(answer.grant.subjectId);
    return `${word}, through ${group}${by === "" ? "" : ` by ${by}`}`;
  }
  return `${word}, by ${by === "" ? "a grant" : by}`;
}

// ---------------------------------------------------------------------------
// The by-app view
// ---------------------------------------------------------------------------

function ResourceHolders({
  resource,
  roles,
  grants,
  state,
  error,
  nameOf,
  viewerUserId,
  canWrite,
  busy,
  refusal,
  onRevoke,
}: {
  resource: AccessResource | null;
  roles: readonly string[];
  grants: readonly AccessGrant[];
  state: "idle" | "loading" | "ready" | "error";
  error: string;
  nameOf: (id: string) => string;
  /** The viewer, bare: the one holder whose grant offers no act. */
  viewerUserId: string;
  canWrite: boolean;
  busy: boolean;
  refusal: (GrantRefusal & { resource: string }) | null;
  onRevoke: (grant: AccessGrant) => void;
}) {
  if (resource === null) return null;
  const yours = grants.some((g) => namesTheViewer(g, viewerUserId));
  return (
    <div className="os-access-holders">
      <Subhead meta={state === "ready" && !error ? grants.length : undefined}>{resource.part === "" ? `Who may open ${resource.label}` : `Who holds ${resource.label}`}</Subhead>
      <p className="os-access-roles">
        {roles.length === 0 ? "No role holds this; only a grant by name can." : `By role: ${roles.join(", ")}.`}
      </p>
      {state === "error" ? <Notice tone="warn" sentence="The grants could not be read." detail={error} /> : null}
      {state === "ready" && grants.length === 0 ? (
        <Caption>Nobody holds it by name. The roles above are the whole answer.</Caption>
      ) : (
        <RecordList as="ul" label="Granted by name">
          {grants.map((grant) => {
            const who = holderLabel(grant, nameOf);
            return (
              <RecordRow key={grant.id} name={who} state={grant.effect === "allow" ? "allowed" : "denied"} tone={grant.effect === "allow" ? "accent" : "muted"}
                secondary={`${grant.subjectKind === "group" ? "a group" : "a person"}${nameOf(grant.grantedBy) === "" ? "" : `, by ${nameOf(grant.grantedBy)}`}`}
                actions={canWrite && !namesTheViewer(grant, viewerUserId) ? <button type="button" className="os-link" disabled={busy} onClick={() => onRevoke(grant)} aria-label={`Revoke ${grant.effect} for ${who}`}>Revoke</button> : undefined} />
            );
          })}
        </RecordList>
      )}
      {/* THE ABSENT ACT, EXPLAINED ONCE. Rule 12 takes the button away rather
          than disabling it, and a control that vanishes with no sentence
          reads as a rendering fault. Said here, under the list, rather than
          on the row: it is the same rule the by-person view states, and one
          sentence about a policy is not per-row news. */}
      {yours && canWrite ? (
        <Caption>One of these is yours. Nobody revokes their own grant; ask a colleague who holds the app.</Caption>
      ) : null}
      {refusal !== null && refusal.resource === resource.resource ? <RefusalNotice refusal={refusal} /> : null}
    </div>
  );
}

/**
 * Whether a grant names the viewer themselves.
 *
 * A USER grant only: a group the viewer belongs to is still theirs to revoke,
 * and the engine's rule 4 says the same -- `grant_self` is checked for
 * `SubjectKindUser` and nothing else.
 */
function namesTheViewer(grant: AccessGrant, viewerUserId: string): boolean {
  return viewerUserId !== "" && grant.subjectKind === "user" && bareId(grant.subjectId) === viewerUserId;
}

/**
 * Who a grant names, in words -- and NEVER the principal id.
 *
 * An opaque id is not a name: nobody can look one up, it tells a reader
 * nothing the row's own "a person" does not, and printing it here would be
 * the one place in this shell where an identifier stands in for somebody.
 * Deployables' attribution settled this already ("a name is offered, never
 * invented"); an unnamed holder says the roster did not name them, which is
 * the true thing and also the actionable one.
 */
function holderLabel(grant: AccessGrant, nameOf: (id: string) => string): string {
  return nameOf(grant.subjectId) || "Not on this roster";
}

// ---------------------------------------------------------------------------
// Shared pieces
// ---------------------------------------------------------------------------

function RefusalNotice({ refusal }: { refusal: GrantRefusal }) {
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

/**
 * The picker's `<kind>:<id>` value, resolved against the roster.
 *
 * The id is everything after the FIRST colon, because a roster row carrying a
 * canonical `v1:identity:user:...` id would otherwise be cut at its own second
 * segment and match nobody -- the picker would silently select nothing. The
 * subject's id is kept BARE, which is both what the grant rows store (so
 * `grantsForSubject` matches) and what the engine resolves on the way in.
 */
function subjectFromKey(key: string, people: readonly PersonRow[], groups: readonly GroupRow[]): Subject | null {
  const at = key.indexOf(":");
  if (at < 0) return null;
  const kind = key.slice(0, at);
  const id = bareId(key.slice(at + 1));
  if (id === "") return null;
  if (kind === "group") {
    const group = groups.find((g) => bareId(g.id) === id);
    return group === undefined ? null : { kind: "group", id, name: group.name, role: "" };
  }
  const person = people.find((p) => bareId(p.id) === id);
  return person === undefined ? null : { kind: "user", id, name: personName(person), role: person.role };
}

/** The scope line under the picker: the role, and the groups the answers can come through. */
function subjectSentence(
  subject: Subject,
  groupIds: readonly string[],
  groups: readonly GroupRow[],
  roles: readonly { slug: string; name: string; aliases: string[] }[],
): string {
  if (subject.kind === "group") {
    return `${subject.name} is a group. A grant here reaches everyone in it; it grants apps and nothing else.`;
  }
  const rung = roles.find((r) => r.slug === subject.role || r.aliases.includes(subject.role));
  const roleName = rung?.name ?? subject.role ?? "no role";
  const names = groupIds.map((id) => groups.find((g) => bareId(g.id) === bareId(id))?.name ?? "").filter((n) => n !== "");
  const inGroups = names.length === 0 ? "in no group" : `in ${names.join(", ")}`;
  return `${subject.name} holds the ${roleName} role and is ${inGroups}.`;
}

/**
 * A principal id to the name a person reads, "" when nothing names it.
 *
 * Bare on both sides -- the map's keys and the id asked about -- so a roster
 * row and a grant that spell the same person differently still meet. The
 * viewer is "you": the one name every session can give without a roster read,
 * and the whole point of the provenance line.
 */
function namer(people: readonly PersonRow[], groups: readonly GroupRow[], viewerUserId: string): (id: string) => string {
  const byId = new Map<string, string>();
  for (const p of people) byId.set(bareId(p.id), personName(p));
  for (const g of groups) byId.set(bareId(g.id), g.name);
  return (id: string) => {
    const who = bareId(id);
    if (who === "") return "";
    if (who === viewerUserId) return "you";
    return byId.get(who) ?? "";
  };
}

function appLabel(resources: readonly AccessResource[], r: AccessResource): string {
  return resources.find((x) => x.appId === r.appId && x.part === "")?.label ?? r.appId;
}

function verbOf(resources: readonly AccessResource[], resource: string): string {
  return resources.find((r) => r.resource === resource)?.verb ?? "read";
}
