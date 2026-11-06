import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { LocalTabs } from "../../kit/LocalTabs";
import type { OsAppProps } from "../../system/registry";
import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";

import { Button, Caption, RecordList, RecordRow, Field, Head, Notice, Panel, Select, Subhead } from "../../kit";
import { useSession } from "../../chrome/access";
import { useOs } from "../../chrome/state";
import { holds } from "../../system/roles";
import type { Refusal } from "../users/refusals";
import { personName, type GroupRow, type PersonRow } from "../users/rows";
import {
  accessNames,
  accessResources,
  bareId,
  grantRefusalCopy,
  groupActs,
  groupAnswer,
  groupAnswerWords,
  personActs,
  personAnswers,
  personAnswerWords,
  rolesHolding,
  type AccessActs,
  type AccessGrant,
  type AccessNames,
  type AccessResource,
  type AccessRow,
  type GrantRefusal,
  type Subject,
} from "./access/model";
import { useAccessRoster, useEffectiveAccess, useGrantWrites, useResourceGrants, useSubjectGrants } from "./access/useAccess";

// SETTINGS > ACCESS (epic memql#5289, task memql#5307; design section 4
// "Administration surface, minimal"; access clarity, 2026-11-06; DESIGN.md).
//
// ===========================================================================
// WHAT THIS SCREEN IS FOR, AND WHAT IT IS NOT
// ===========================================================================
// Two people share a cluster and one of them wants to hand the other an app,
// or a part of one, without changing their role. This is where that is done
// and where the answer to "can Ada open Deployables?" is read: one subject at
// a time, every app and part in a list, each row saying the answer in words
// and where it came from -- the role, a group, a grant to the person, an
// organization role's limit, or nothing at all -- and, where the engine
// answers per organization, which organization. The answer is the ENGINE's
// (`effectiveAccessForSubject`, access/model.ts says why it is no longer
// restated here). The by-app view is the rows turned around: one resource,
// who holds it.
//
// Minimal by the owner's instruction: no drag and drop, no bulk edit, no
// invitations from here. The Users app is where people and groups are made.
//
// ===========================================================================
// THE ANSWER IS READ, THE ACT IS PRESSED, AND THE TWO NEVER SHARE A PLACE
// ===========================================================================
// The blue "Allow" this screen used to draw beside a faint mark read as the
// answer: an organization's moderator looked able to open every app. So the
// answer is words under the app's name -- "Can open in Acme, from the group
// Acme" -- and the acts are the kit's buttons in the row's own act column,
// each labelled with what it does.
//
// ===========================================================================
// ONLY AN ACT THAT CHANGES AN ANSWER IS OFFERED
// ===========================================================================
// Deny where something can be opened, Allow where a grant would open it,
// "Use role" over the person's own grant (for a group, "Remove"). None on a
// pair a role's limit bars, which no grant lifts; none that would write what
// already stands. Absent for a viewer whose role does not write grants
// (rule 12: an act that is not legal is absent, never disabled), and the
// caption says why. A grant to a person counts in every organization they
// are in, so a pair's acts are drawn once for the pair, never beside one
// organization's answer.
//
// ===========================================================================
// A REFUSED READ IS SAID, NEVER DRAWN AS AN EMPTY LIST
// ===========================================================================
// The engine refuses the read to a viewer below admin or holding an
// organization's role (an organization's own admin can open this screen), and
// a person who outranks the viewer. The refusal stands in place of the
// answers, and nothing about that person is drawn: no rows, no acts.
//
// ===========================================================================
// THE REFUSAL OF AN ACT SITS BESIDE THE ROW THAT ASKED
// ===========================================================================
// The engine's governance codes (integrations/rbac/grants.go) are printed
// under the row whose act was refused, with copy this build carries and the
// server's own sentence beneath. A refusal at the bottom of a twenty-row
// list is a refusal nobody connects to the click.
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
// screen advertises an act the cluster will not perform), and the by-app
// view's "by you" degrades to your own display name as though a colleague had
// written it.
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
  const effective = useEffectiveAccess(subject);
  const resources = useMemo(
    () => accessResources(registry, roster.catalog, [...effective.rows.map((r) => r.resource), ...subjectGrants.own.map((g) => g.resource)]),
    [registry, roster.catalog, effective.rows, subjectGrants.own],
  );
  const resourceGrants = useResourceGrants(view === "resource" ? resourceKey : "");

  const reloadEffective = effective.reload;
  const onWritten = useCallback(() => {
    reloadEffective();
    subjectGrants.reload();
    resourceGrants.reload();
  }, [reloadEffective, subjectGrants, resourceGrants]);
  const writes = useGrantWrites(onWritten);

  const nameOf = useMemo(() => namer(roster.people, roster.groups, viewerUserId), [roster.people, roster.groups, viewerUserId]);
  const names = useMemo(() => accessNames(roster.roles, roster.groups, resources), [roster.roles, roster.groups, resources]);
  // THE SUBJECT IS THE VIEWER: the one state in which no act is offered,
  // because rule 4 of the engine's governance (`grant_self`) refuses it.
  const viewingSelf = subject !== null && subject.kind === "user" && bareId(subject.id) === viewerUserId;
  const acting = canWrite && !viewingSelf;
  // A PERSON ANSWERED PER ORGANIZATION: their acts reach every one of those
  // organizations at once, which the page says once rather than on each row.
  const perOrganization = subject !== null && subject.kind === "user" && effective.rows.some((r) => r.accountId !== "" && r.resource.startsWith("app:"));

  return (
    // A LIST TAKES THE WINDOW (DESIGN.md rules 1 and 9): at the settings
    // measure the rows fall under the record list's narrow layout, and every
    // act drops onto a line of its own, away from the answer it changes.
    <div className="os-settings os-settings-wide os-access" data-os-dividers="none">
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
          Writing a grant takes <span className="os-mono">update</span> on <span className="os-mono">principal</span>, which
          your role does not carry.
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

          {subject === null ? (roster.state === "loading" ? <RecordListSkeleton label="Reading the roster." /> : <Caption>{"Pick somebody to see what they may open, and where each answer comes from."}</Caption>) : effective.state === "refused" ? (
            // IN PLACE OF THE ANSWERS, and in place of everything else about
            // them: the reads behind the scope line are refused to this
            // viewer too, and a line saying "in no group" would be a guess.
            <ReadRefusalNotice refusal={effective.refusal} />
          ) : effective.state !== "ready" ? (
            <RecordListSkeleton label="Reading what they can open." />
          ) : (
            <>
              <Caption>{subjectSentence(subject, subjectGrants.state === "ready" ? subjectGrants.groupIds : null, roster.groups, roster.roles)}</Caption>
              {subjectGrants.state === "error" ? (
                <Notice tone="warn" sentence="Their grants could not be read." detail={subjectGrants.error} />
              ) : null}
              {perOrganization && acting ? <Caption>A grant to a person counts in every organization they are in.</Caption> : null}
              <AccessAnswers
                subject={subject}
                resources={resources}
                rows={effective.rows}
                // THIS SUBJECT'S OWN GRANTS, OR NONE: the read is keyed to its
                // subject, so "Use role" and "Remove" are handed no grant id
                // until it has answered for them, and none after it failed.
                ownGrants={subjectGrants.own}
                names={names}
                canWrite={acting}
                busy={writes.busy || effective.pending || subjectGrants.pending}
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
// The answers
// ---------------------------------------------------------------------------

/** One row of the list: an app or a part, what the subject meets there, and what could change it. */
interface AnswerItem {
  resource: AccessResource;
  lines: { key: string; held: boolean; verdict: string; reason: string }[];
  acts: AccessActs;
}

/**
 * The subject's rows, in the order `accessResources` gives (each app's door,
 * then its parts and sections). A person's row is drawn where the engine
 * answered the pair, with one line per answer -- one, or one per
 * organization, sorted by the organization's name. A group's row is drawn for
 * every resource: its own grant, or none.
 */
function answerItems(
  subject: Subject,
  resources: readonly AccessResource[],
  rows: readonly AccessRow[],
  ownGrants: readonly AccessGrant[],
  names: AccessNames,
): AnswerItem[] {
  return resources.flatMap((resource): AnswerItem[] => {
    if (subject.kind === "group") {
      const answer = groupAnswer(rows, resource);
      return [{ resource, lines: [{ key: "group", held: answer === "allow", verdict: groupAnswerWords(answer), reason: "" }], acts: groupActs(answer, ownGrants, resource, subject) }];
    }
    const answers = personAnswers(rows, resource);
    if (answers.length === 0) return [];
    const lines = answers
      .map((a) => ({ key: a.accountId, held: a.canOpen, organization: a.accountId === "" ? "" : names.organization(a.accountId), ...personAnswerWords(a, names) }))
      .sort((a, b) => a.organization.localeCompare(b.organization));
    return [{ resource, lines, acts: personActs(answers, ownGrants, resource, subject) }];
  });
}

function AccessAnswers({
  subject,
  resources,
  rows,
  ownGrants,
  names,
  canWrite,
  busy,
  refusal,
  onAllow,
  onDeny,
  onRevoke,
}: {
  subject: Subject;
  resources: readonly AccessResource[];
  rows: readonly AccessRow[];
  ownGrants: readonly AccessGrant[];
  names: AccessNames;
  /** The viewer writes grants and is not looking at themselves. */
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
  const items = answerItems(subject, resources, rows, ownGrants, names);
  if (items.length === 0) {
    return <Caption>The cluster answered no app for them.</Caption>;
  }
  return (
    <RecordList as="ul" density="compact" className="os-access-list" label={subject.kind === "group" ? `What ${subject.name} grants` : `What ${subject.name} can open`}>
      {items.map(({ resource: r, lines, acts }) => {
        const refused = refusal !== null && refusal.resource === r.resource ? refusal : null;
        const buttons: ReactNode[] = [];
        if (canWrite && acts.own !== null) {
          const grant = acts.own;
          buttons.push(
            subject.kind === "group" ? (
              <Button key="revoke" disabled={busy} onClick={() => onRevoke(grant, r)} ariaLabel={`Remove ${subject.name}'s grant for ${r.label}`}>
                Remove
              </Button>
            ) : (
              <Button key="revoke" disabled={busy} onClick={() => onRevoke(grant, r)} ariaLabel={`Use the role's answer for ${r.label}`}>
                Use role
              </Button>
            ),
          );
        }
        if (canWrite && acts.allow) {
          buttons.push(
            <Button key="allow" disabled={busy} onClick={() => onAllow(r)} ariaLabel={`Allow ${r.label} to ${subject.name}`}>
              Allow
            </Button>,
          );
        }
        if (canWrite && acts.deny) {
          buttons.push(
            <Button key="deny" disabled={busy} onClick={() => onDeny(r)} ariaLabel={`Deny ${r.label} to ${subject.name}`}>
              Deny
            </Button>,
          );
        }
        return (
          <li key={r.resource} data-resource={r.resource} data-part={r.part === "" ? undefined : ""}>
            <RecordRow
              name={r.label}
              secondary={lines.map((line) => (
                <span key={line.key} className="os-access-answer" data-held={line.held ? "" : undefined}>
                  <span className="os-access-verdict">{line.verdict}</span>
                  {line.reason === "" ? null : `, ${line.reason}`}
                </span>
              ))}
              actions={buttons.length === 0 ? undefined : buttons}
            />
            {refused === null ? null : <RefusalNotice refusal={refused} />}
          </li>
        );
      })}
    </RecordList>
  );
}

/** The engine's refusal to answer for somebody, in place of their answers. */
function ReadRefusalNotice({ refusal }: { refusal: Refusal | null }) {
  const detail = refusal === null || refusal.detail === "" ? undefined : refusal.code === "" ? refusal.detail : `${refusal.code}: ${refusal.detail}`;
  return (
    <Notice
      tone="warn"
      sentence={refusal !== null && refusal.title !== "" ? refusal.title : "What they can open could not be read."}
      next={refusal === null || refusal.next === "" ? undefined : refusal.next}
      detail={detail}
    />
  );
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
                actions={canWrite && !namesTheViewer(grant, viewerUserId) ? <Button disabled={busy} onClick={() => onRevoke(grant)} ariaLabel={`Revoke ${grant.effect} for ${who}`}>Revoke</Button> : undefined} />
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

/**
 * The scope line under the picker: the role, and the groups the answers can
 * come through. `groupIds` is null while this person's groups are not known
 * (unread, or the read failed), and then the line names none: "in no group"
 * would be a guess, and another person's groups a wrong one.
 */
function subjectSentence(
  subject: Subject,
  groupIds: readonly string[] | null,
  groups: readonly GroupRow[],
  roles: readonly { slug: string; name: string; aliases: string[] }[],
): string {
  if (subject.kind === "group") {
    return `${subject.name} is a group. A grant here reaches everyone in it; it grants apps and nothing else.`;
  }
  const rung = roles.find((r) => r.slug === subject.role || r.aliases.includes(subject.role));
  const roleName = rung?.name ?? subject.role ?? "no role";
  if (groupIds === null) return `${subject.name} holds the ${roleName} role.`;
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
