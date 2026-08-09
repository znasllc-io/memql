import { useState, type FormEvent, type ReactNode } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { PROPORTION_BAR_ELEMENT, TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useConcepts } from "../cluster/useConcepts";
import { ErrorMessage } from "../components/StatusMessage";
import { Band, MetaButton } from "../views/ViewLayout";
import { ViewElement } from "../views/ViewElement";
import { AdminFrame, Reading, Refused } from "./AdminLayout";
import { surfaceById } from "./urls";
import { useAdminAccess, useAdminWrites, usePeople, type WriteState } from "./useAdminConsole";
import { WriteOutcome } from "./WriteOutcome";

const PERSON_CONCEPT_ID = "v1:identity:user";

// The people an operator ADMINISTERS -- the absorbed /admin/users list and
// /admin/users/detail (memql#3324).
//
// ===========================================================================
// WHY THIS AND THE People VIEW BOTH EXIST
// ===========================================================================
// They answer different questions and deliberately do not merge.
//
// /views/people is the POPULATION: who is in the organisation, how the roles
// divide, who has a session open. It is one of the five predefined views, it
// composes only elements, and it is readable by anyone the cluster shows rows
// to. It has no controls, and adding them there would put an owner-only write
// inside a screen whose whole contract is "a layout choice over rows".
//
// This is the CHANGE surface: one person at a time, and the four things an
// owner or admin may do to them. The list here exists only to pick that
// person, which is why it is a bare table rather than a second designed view.
//
// ===========================================================================
// THE CONTROLS BELOW ARE NOT THE GATE
// ===========================================================================
// Every write goes through IdentityAdminClient and is refused server-side for
// a caller below owner/admin, in component/identity/adminops, against the role
// the stream interceptor verified -- with an `admin_auth_forbidden` audit
// event whose id comes back on the refusal. This page hides its controls from
// a caller it can already tell will be refused, which saves an operator a
// pointless round trip and teaches them who can; it decides nothing.
export function PeoplePage(): ReactNode {
  const surface = surfaceById("people");
  const { role, canAdminister, resolved } = useAdminAccess();
  const people = usePeople(canAdminister);
  const writes = useAdminWrites();
  const { concepts } = useConcepts();
  const personConcept = concepts.find((c) => c.id === PERSON_CONCEPT_ID);

  // The selection lives in the URL so an operator can hand a colleague a link
  // to the person they are talking about, exactly as the predefined views do
  // with their row detail.
  const [params, setParams] = useSearchParams();
  const selectedId = params.get("person") ?? "";
  const select = (rowId: string) => {
    const next = new URLSearchParams(params);
    if (rowId === "" || rowId === selectedId) next.delete("person");
    else next.set("person", rowId);
    setParams(next, { replace: true });
  };

  if (surface === undefined) return null;
  if (!canAdminister) {
    return (
      <AdminFrame surface={surface} role={role} resolved={resolved}>
        <Refused role={role} resolved={resolved} />
      </AdminFrame>
    );
  }

  // From the rows already read rather than a second query. `searchUsers`
  // returns deactivated people too (the console omits its `active` argument on
  // purpose), and the by-id read that would otherwise serve this -- `userById`
  // -- filters on isActiveRecord, so a suspended account would vanish from the
  // one screen that exists to reinstate it.
  const selected = people.data.find((row) => rowId(row) === selectedId);
  const suspended = people.data.filter((row) => row["active"] === false).length;
  const admins = people.data.filter(
    (row) => row["role"] === "owner" || row["role"] === "admin",
  ).length;

  return (
    <AdminFrame
      surface={surface}
      role={role}
      resolved={resolved}
      actions={<MetaButton onClick={people.reload}>Refresh</MetaButton>}
    >
      <Band>
        <div className="flex flex-wrap gap-2">
          <Reading
            label="People"
            value={people.loading ? "…" : String(people.data.length)}
            sub="active and suspended"
          />
          <Reading
            label="Owners and admins"
            value={people.loading ? "…" : String(admins)}
            sub="everyone who can reach this console"
          />
          <Reading
            label="Suspended"
            value={people.loading ? "…" : String(suspended)}
            sub="cannot sign in"
          />
        </div>
        {people.error === "" ? null : (
          <div className="mt-3">
            <ErrorMessage>Could not read the people list: {people.error}</ErrorMessage>
          </div>
        )}
        <WriteOutcome state={writes} />
      </Band>

      <Band title="By role" meta="Cluster-wide role, not per-partition grants">
        {personConcept === undefined ? (
          <p className="text-sm text-subtle">
            This node does not publish the user concept, so the role split cannot
            be drawn.
          </p>
        ) : (
          <ViewElement
            element={PROPORTION_BAR_ELEMENT}
            rows={people.data}
            concept={personConcept}
            options={{ bindings: { category: "role", value: [] } }}
          />
        )}
      </Band>

      <Band title="Everyone" meta="Pick a person to change them" panel>
        {personConcept === undefined ? (
          <p className="p-3 text-sm text-subtle">
            This node does not publish the user concept.
          </p>
        ) : people.data.length === 0 ? (
          <p className="p-3 text-sm text-subtle">
            {people.loading ? "Reading people…" : "Nobody can sign in to this cluster yet."}
          </p>
        ) : (
          <ViewElement
            element={TABLE_ELEMENT}
            rows={people.data}
            concept={personConcept}
            options={{
              ...(selectedId === "" ? {} : { selectedRowId: selectedId }),
              bindings: {
                column: ["displayName", "primaryEmail", "role", "active", "lastSeenAt"],
              },
              sort: { field: "displayName", direction: "asc" },
            }}
            onSelect={select}
          />
        )}
      </Band>

      {selected === undefined ? (
        <p className="text-sm text-subtle">
          Nobody is selected. The whole population, with the sessions open in
          their name, is in the{" "}
          <Link to="/views/people" className="underline hover:text-fg">
            People view
          </Link>
          .
        </p>
      ) : (
        <PersonDetail person={selected} writes={writes} onChanged={people.reload} />
      )}
    </AdminFrame>
  );
}

function rowId(row: Row): string {
  return typeof row["id"] === "string" ? row["id"] : "";
}

function field(row: Row, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

// The three changes an operator makes to one person, as three separate forms.
//
// Separate rather than one big save, because they are three different
// decisions with three different blast radii and three different audit
// actions: correcting a phone number is not the same act as making someone an
// owner, and a single "Save" button would let one be done by accident while
// intending the other. The retired console split them the same way, for the
// same reason.
function PersonDetail({
  person,
  writes,
  onChanged,
}: {
  person: Row;
  writes: WriteState;
  onChanged: () => void;
}): ReactNode {
  const id = rowId(person);
  const email = field(person, "primaryEmail");
  const isSuspended = person["active"] === false;

  return (
    <div className="flex flex-col gap-6">
      <Band
        title={field(person, "displayName") || email || id}
        meta={<span className="font-mono break-all">{id}</span>}
      >
        <div className="grid gap-4 lg:grid-cols-2">
          <ProfileForm person={person} writes={writes} onChanged={onChanged} />
          <div className="flex flex-col gap-4">
            <RoleForm person={person} writes={writes} onChanged={onChanged} />
            <SuspensionForm
              person={person}
              suspended={isSuspended}
              writes={writes}
              onChanged={onChanged}
            />
          </div>
        </div>
      </Band>
    </div>
  );
}

function ProfileForm({
  person,
  writes,
  onChanged,
}: {
  person: Row;
  writes: WriteState;
  onChanged: () => void;
}): ReactNode {
  const id = rowId(person);
  // Keyed on the person's id so picking a different row resets the inputs
  // rather than carrying one person's half-typed phone number onto another.
  const [draft, setDraft] = useState(() => profileDraft(person));
  const [keyed, setKeyed] = useState(id);
  if (keyed !== id) {
    setKeyed(id);
    setDraft(profileDraft(person));
  }

  const submit = (event: FormEvent) => {
    event.preventDefault();
    writes.run((client) => client.updateUserProfile({ userId: id, ...draft }), onChanged);
  };

  return (
    <form onSubmit={submit} className="rounded-lg border border-line bg-surface p-4">
      <h3 className="text-xs font-semibold tracking-wide text-muted uppercase">Profile</h3>
      <p className="mt-1 text-xs text-subtle">
        Sent whole on every save. Clearing a field clears it on the row — the
        write is a shallow merge, so a blank box means blank, not “leave it”.
      </p>
      <div className="mt-3 grid gap-3 sm:grid-cols-2">
        <Field label="Display name" value={draft.displayName} onChange={(v) => setDraft({ ...draft, displayName: v })} />
        <Field label="First name" value={draft.firstName} onChange={(v) => setDraft({ ...draft, firstName: v })} />
        <Field label="Last name" value={draft.lastName} onChange={(v) => setDraft({ ...draft, lastName: v })} />
        <Field label="Phone" value={draft.phone} onChange={(v) => setDraft({ ...draft, phone: v })} />
        <Field label="Job title" value={draft.primaryRole} onChange={(v) => setDraft({ ...draft, primaryRole: v })} />
        <Field label="Gender" value={draft.gender} onChange={(v) => setDraft({ ...draft, gender: v })} />
        <Field label="Birthdate" value={draft.birthdate} onChange={(v) => setDraft({ ...draft, birthdate: v })} />
      </div>
      <div className="mt-4">
        <SubmitButton busy={writes.busy}>Save the profile</SubmitButton>
      </div>
    </form>
  );
}

function profileDraft(person: Row) {
  return {
    displayName: field(person, "displayName"),
    firstName: field(person, "firstName"),
    lastName: field(person, "lastName"),
    phone: field(person, "phone"),
    // The concept's `primaryRole` is a JOB TITLE, not an authorization role.
    // Labelled as one here because two fields called "role" on one form, one of
    // which grants power and one of which does not, is how the wrong one gets
    // changed.
    primaryRole: field(person, "primaryRole"),
    gender: field(person, "gender"),
    birthdate: field(person, "birthdate"),
  };
}

// The cluster roles, in descending power. A fixed list rather than a free-text
// box: the server validates the set, and a select is honest where a typo would
// come back as INVALID_ARGUMENT after the operator thought they were done.
const ROLES = ["owner", "admin", "developer", "writer", "reader"] as const;

function RoleForm({
  person,
  writes,
  onChanged,
}: {
  person: Row;
  writes: WriteState;
  onChanged: () => void;
}): ReactNode {
  const id = rowId(person);
  const current = field(person, "role");
  const [role, setRole] = useState(current);
  const [keyed, setKeyed] = useState(id);
  if (keyed !== id) {
    setKeyed(id);
    setRole(current);
  }

  return (
    <form
      onSubmit={(event) => {
        event.preventDefault();
        writes.run((client) => client.setUserRole(id, role), onChanged);
      }}
      className="rounded-lg border border-line bg-surface p-4"
    >
      <h3 className="text-xs font-semibold tracking-wide text-muted uppercase">Cluster role</h3>
      <p className="mt-1 text-xs text-subtle">
        What this person may do everywhere. Owner and admin can reach this
        console; owner alone can roll a deployment back.
      </p>
      <div className="mt-3 flex flex-wrap items-end gap-2">
        <label className="flex flex-col gap-1 text-xs text-muted">
          Role
          <select
            aria-label="Cluster role"
            value={role}
            onChange={(event) => setRole(event.target.value)}
            className="rounded border border-line bg-surface px-2 py-1 text-sm text-fg"
          >
            {ROLES.map((name) => (
              <option key={name} value={name}>
                {name}
              </option>
            ))}
          </select>
        </label>
        <SubmitButton busy={writes.busy || role === current}>Apply the role</SubmitButton>
      </div>
    </form>
  );
}

function SuspensionForm({
  person,
  suspended,
  writes,
  onChanged,
}: {
  person: Row;
  suspended: boolean;
  writes: WriteState;
  onChanged: () => void;
}): ReactNode {
  const id = rowId(person);
  const [reason, setReason] = useState("");

  return (
    <form
      onSubmit={(event) => {
        event.preventDefault();
        writes.run((client) => client.setUserSuspended(id, !suspended, reason), onChanged);
      }}
      className="rounded-lg border border-line bg-surface p-4"
    >
      <h3 className="text-xs font-semibold tracking-wide text-muted uppercase">Access</h3>
      {suspended ? (
        <>
          <p className="mt-1 text-sm text-fg">
            This account is suspended{reasonSuffix(person)}. It cannot sign in.
          </p>
          <div className="mt-3">
            <SubmitButton busy={writes.busy}>Reinstate the account</SubmitButton>
          </div>
        </>
      ) : (
        <>
          <p className="mt-1 text-xs text-subtle">
            Suspending stops this person signing in. Existing tokens are a
            separate matter — revoke them under Tokens.
          </p>
          <div className="mt-3 flex flex-wrap items-end gap-2">
            <Field label="Reason" value={reason} onChange={setReason} />
            <SubmitButton busy={writes.busy} tone="danger">
              Suspend the account
            </SubmitButton>
          </div>
        </>
      )}
    </form>
  );
}

function reasonSuffix(person: Row): string {
  const reason = field(person, "suspendedReason");
  return reason === "" ? "" : `: ${reason}`;
}

function Field({
  label,
  value,
  onChange,
}: {
  label: string;
  value: string;
  onChange: (next: string) => void;
}): ReactNode {
  return (
    <label className="flex flex-col gap-1 text-xs text-muted">
      {label}
      <input
        type="text"
        value={value}
        onChange={(event) => onChange(event.target.value)}
        className="rounded border border-line bg-surface px-2 py-1 text-sm text-fg"
      />
    </label>
  );
}

// A submit button in the MetaButton idiom. Not MetaButton itself, because that
// one is `type="button"` with an onClick and these live inside forms, where
// submitting on Enter is the behaviour an operator expects.
export function SubmitButton({
  busy,
  tone = "quiet",
  children,
}: {
  busy: boolean;
  tone?: "quiet" | "danger";
  children: ReactNode;
}): ReactNode {
  return (
    <button
      type="submit"
      disabled={busy}
      className={
        "rounded border px-2.5 py-1 text-xs font-medium disabled:cursor-not-allowed disabled:opacity-40 " +
        (tone === "danger"
          ? "border-danger bg-danger-subtle text-fg hover:bg-danger hover:text-accent-fg"
          : "border-line bg-surface text-fg hover:bg-raised")
      }
    >
      {children}
    </button>
  );
}
