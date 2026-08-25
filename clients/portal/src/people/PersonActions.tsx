import { useState, type FormEvent, type ReactNode } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  Band,
  Button,
  ConfirmDialog,
  DataText,
  Field,
  FormActions,
  FormRow,
  Select,
  TextInput,
} from "../ui";
import { useAdminWrites, type WriteState } from "../admin/useAdminConsole";
import { WriteOutcome } from "../admin/WriteOutcome";

// The per-person CHANGE surface.
//
// # Why this is not in src/admin/ any more (memql#4264)
//
// It used to be the bottom half of an /admin/people page that also listed
// everyone -- which meant the user population existed twice in the portal: once as the
// population under "Operate" and again as the change surface under
// "Administer", with the "By role" band rendering on both (and a third time on
// the admin overview). An operator looking for a person had two doors and no
// way to know which one they wanted.
//
// The LIST was always the view's job (People then, Users since memql#4526).
// What was genuinely only here is
// this: the four things an owner or admin does TO one person. So it moved to
// where a person is already selected -- the view's row detail -- and the
// duplicate surface went away.
//
// # Why it is not in src/views/ either
//
// portal_view_composition_test.go forbids row markup and iteration inside
// src/views/, and these forms iterate a role list and a TTL list. The guard is
// right to forbid that there: a designed view reaching past the element library
// is how the library stops being the thing that makes a new concept work for
// free. So the actions live in their own directory and ViewLayout renders them
// as an opaque child -- which is also what keeps them usable from anywhere else
// a person is in hand.

// The three changes an operator makes to one person, as three separate forms.
//
// Separate rather than one big save, because they are three different
// decisions with three different blast radii and three different audit
// actions: correcting a phone number is not the same act as making someone an
// owner, and a single "Save" button would let one be done by accident while
// intending the other. The retired console split them the same way, for the
// same reason.
function rowId(row: Row): string {
  return typeof row["id"] === "string" ? row["id"] : "";
}

function field(row: Row, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

// The entry point. Takes the row the surrounding surface already has, so it
// makes no read of its own -- the caller owns the population and this owns the
// verbs.
export function PersonActions({
  person,
  onChanged,
}: {
  person: Row;
  onChanged: () => void;
}): ReactNode {
  const writes = useAdminWrites();
  return (
    <>
      <PersonDetail person={person} writes={writes} onChanged={onChanged} />
      {/* The outcome of the last write, either way -- and the audit id it
          recorded, which is present on a REFUSAL too. An operator arguing
          about a denial needs its id, so this renders below the forms rather
          than inside whichever one fired. */}
      <WriteOutcome state={writes} />
    </>
  );
}

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
        meta={<DataText kind="id">{id}</DataText>}
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
            <EnrolmentForm person={person} writes={writes} />
            <RecoveryKeyForm person={person} writes={writes} />
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
        <Field label="Display name">
          <TextInput value={draft.displayName} onChange={(v) => setDraft({ ...draft, displayName: v })} />
        </Field>
        <Field label="First name">
          <TextInput value={draft.firstName} onChange={(v) => setDraft({ ...draft, firstName: v })} />
        </Field>
        <Field label="Last name">
          <TextInput value={draft.lastName} onChange={(v) => setDraft({ ...draft, lastName: v })} />
        </Field>
        <Field label="Phone">
          <TextInput value={draft.phone} onChange={(v) => setDraft({ ...draft, phone: v })} />
        </Field>
        <Field label="Job title">
          <TextInput value={draft.primaryRole} onChange={(v) => setDraft({ ...draft, primaryRole: v })} />
        </Field>
        <Field label="Gender">
          <TextInput value={draft.gender} onChange={(v) => setDraft({ ...draft, gender: v })} />
        </Field>
        <Field label="Birthdate">
          <TextInput value={draft.birthdate} onChange={(v) => setDraft({ ...draft, birthdate: v })} />
        </Field>
      </div>
      <div className="mt-4">
        <Button type="submit" busy={writes.busy} busyLabel="Working…">Save the profile</Button>
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
      <div className="mt-3">
        <FormRow>
          <Field label="Role">
            <Select ariaLabel="Cluster role" value={role} onChange={setRole}>
              {ROLES.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </Select>
          </Field>
          <FormActions>
            <Button type="submit" busy={writes.busy || role === current} busyLabel="Working…">
              Apply the role
            </Button>
          </FormActions>
        </FormRow>
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
  // Suspending is the dangerous branch, so it confirms in a dialog that
  // states what stops working; reinstating is the undo and submits directly.
  const [confirmingSuspend, setConfirmingSuspend] = useState(false);

  return (
    <form
      onSubmit={(event) => {
        event.preventDefault();
        if (suspended) {
          writes.run((client) => client.setUserSuspended(id, false, reason), onChanged);
        } else {
          setConfirmingSuspend(true);
        }
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
            <Button type="submit" busy={writes.busy} busyLabel="Working…">Reinstate the account</Button>
          </div>
        </>
      ) : (
        <>
          <p className="mt-1 text-xs text-subtle">
            Suspending stops this person signing in. Existing tokens are a
            separate matter — revoke them under Tokens.
          </p>
          <div className="mt-3">
            <FormRow>
              <Field label="Reason" grow>
                <TextInput value={reason} onChange={setReason} />
              </Field>
              <FormActions>
                <Button type="submit" tone="danger" busy={writes.busy} busyLabel="Working…">
                  Suspend the account
                </Button>
              </FormActions>
            </FormRow>
          </div>
        </>
      )}
      <ConfirmDialog
        open={confirmingSuspend}
        title="Suspend this account?"
        confirmLabel="Suspend the account"
        tone="danger"
        busy={writes.busy}
        onConfirm={() => {
          setConfirmingSuspend(false);
          writes.run((client) => client.setUserSuspended(id, true, reason), onChanged);
        }}
        onCancel={() => setConfirmingSuspend(false)}
      >
        {field(person, "displayName") || field(person, "primaryEmail") || id} will not be able to
        sign in until reinstated. Tokens they already hold are a separate matter — revoke those
        under Tokens.
      </ConfirmDialog>
    </form>
  );
}

// The TTLs an operator can pick, in seconds. A fixed list rather than a free
// number box: the server clamps anything above its ceiling, and offering a box
// that silently accepts "7 days" and issues fifteen minutes teaches the
// operator something false about the credential they just handed over.
const ENROLMENT_TTLS: ReadonlyArray<{ label: string; seconds: number }> = [
  { label: "15 minutes", seconds: 0 },
  { label: "1 hour", seconds: 3600 },
  { label: "8 hours", seconds: 8 * 3600 },
  { label: "24 hours", seconds: 24 * 3600 },
];

// Issue an enrolment link for this person (memql#3408).
//
// This is the surface that removes email from the critical path: a link that
// authorizes exactly one action -- register a passkey as this user -- so
// somebody with no credential and no reachable mailbox can still get their
// first one. It is deliberately not a "send" button: MemQL does not put the
// credential in a message it cannot see delivered. The operator copies the
// link and hands it over on whatever channel they already trust.
function EnrolmentForm({ person, writes }: { person: Row; writes: WriteState }): ReactNode {
  const id = rowId(person);
  const [seconds, setSeconds] = useState(0);
  // THE ONE-TIME PLAINTEXT. Held in component state and nowhere else: not in
  // storage, not in a URL, not in a row. Only the SHA-256 hash was persisted
  // server-side, so this really is the only moment the value exists outside
  // the reply.
  const [minted, setMinted] = useState("");
  const [keyed, setKeyed] = useState(id);
  if (keyed !== id) {
    // Picking a different person drops the previous link off the screen. A
    // credential for one account left visible under another person's heading
    // is the one way this control can mislead.
    setKeyed(id);
    setMinted("");
  }

  return (
    <form
      onSubmit={(event) => {
        event.preventDefault();
        setMinted("");
        writes.run(async (client) => {
          const result = await client.issueEnrolmentLink(id, seconds);
          setMinted(result.url);
          return result;
        });
      }}
      className="rounded-lg border border-line bg-surface p-4"
    >
      <h3 className="text-xs font-semibold tracking-wide text-muted uppercase">Enrolment link</h3>
      <p className="mt-1 text-xs text-subtle">
        A single-use link that lets this person set up a passkey — no email
        needed. It authorizes that one action and nothing else, and it is spent
        the moment the passkey is created.
      </p>
      <div className="mt-3">
        <FormRow>
          <Field label="Valid for">
            <Select
              ariaLabel="Enrolment link lifetime"
              value={String(seconds)}
              onChange={(next) => setSeconds(Number(next))}
            >
              {ENROLMENT_TTLS.map((option) => (
                <option key={option.seconds} value={String(option.seconds)}>
                  {option.label}
                </option>
              ))}
            </Select>
          </Field>
          <FormActions>
            <Button type="submit" busy={writes.busy} busyLabel="Working…">
              Issue a link
            </Button>
          </FormActions>
        </FormRow>
      </div>
      {minted === "" ? null : <MintedEnrolmentLink url={minted} onDismiss={() => setMinted("")} />}
    </form>
  );
}

// The one-time link. Shown once, held in component state and nowhere else.
// Copy follows the accounts console's MintedToken: it says you will not see it
// again in those words, rather than a softer "keep it safe".
function MintedEnrolmentLink({ url, onDismiss }: { url: string; onDismiss: () => void }): ReactNode {
  return (
    <div className="mt-3 flex flex-col gap-2 rounded border border-accent bg-accent-subtle p-3">
      <h4 className="text-sm font-semibold">Copy this link now</h4>
      <p className="text-xs">
        You will not see it again. Only its hash was stored, so it cannot be shown a second time —
        if you lose it, issue another. Treat it like a password: anyone holding it can add a
        passkey to this account.
      </p>
      <code className="block rounded border border-line bg-bg px-2 py-1.5 font-mono text-xs break-all select-all">
        {url}
      </code>
      <div>
        <Button onClick={onDismiss}>I have copied it</Button>
      </div>
    </div>
  );
}

// Rotate this owner's recovery key (memql#3970).
//
// SHOWN FOR OWNERS ONLY, because a recovery key exists only for an owner: it
// is the break-glass route into the account that can promote everybody else,
// and there is nothing for it to recover on a reader's account. Rendering the
// control on people who cannot have one would invite an operator to click it
// and get a refusal.
//
// WHAT IT IS FOR. The key is minted automatically and its plaintext is never
// logged, so an operator who lost the value they were shown -- or who never
// claimed it -- has a live break-glass credential nobody holds. That is not a
// lockout, but it is a recovery route that will not work when it is reached
// for. Rotating mints one they can actually store. It is also the response to
// a suspected leak, which is why it lives here rather than only in a CLI: the
// person who needs it most urgently is the one who can still sign in.
function RecoveryKeyForm({ person, writes }: { person: Row; writes: WriteState }): ReactNode {
  const id = rowId(person);
  // THE ONE-TIME PLAINTEXT, held in component state and nowhere else -- same
  // discipline as the enrolment link above, and the value is worth more.
  const [minted, setMinted] = useState("");
  const [keyed, setKeyed] = useState(id);
  if (keyed !== id) {
    setKeyed(id);
    setMinted("");
  }
  if (field(person, "role") !== "owner") {
    return null;
  }

  return (
    <form
      onSubmit={(event) => {
        event.preventDefault();
        setMinted("");
        writes.run(async (client) => {
          const result = await client.rotateRecoveryKey(id);
          setMinted(result.key);
          return result;
        });
      }}
      className="rounded-lg border border-line bg-surface p-4"
    >
      <h3 className="text-xs font-semibold tracking-wide text-muted uppercase">Recovery key</h3>
      <p className="mt-1 text-xs text-subtle">
        The break-glass credential for this owner. It sets up a passkey — and
        only while this account has no working way to sign in, so it is refused
        on any day it is not needed. Rotating retires the current key and shows
        you a new one.
      </p>
      <div className="mt-3">
        <Button type="submit" busy={writes.busy} busyLabel="Working…">Rotate the key</Button>
      </div>
      {minted === "" ? null : <MintedRecoveryKey value={minted} onDismiss={() => setMinted("")} />}
    </form>
  );
}

// The one-time key. Shown once, held in component state and nowhere else.
function MintedRecoveryKey({ value, onDismiss }: { value: string; onDismiss: () => void }): ReactNode {
  return (
    <div className="mt-3 flex flex-col gap-2 rounded border border-accent bg-accent-subtle p-3">
      <h4 className="text-sm font-semibold">Copy this key now</h4>
      <p className="text-xs">
        You will not see it again. Only its hash was stored, so it cannot be shown a second time —
        if you lose it, rotate again. Store it somewhere the cluster is NOT: a password manager, a
        safe. Anyone holding it can take over this account on a day this owner cannot sign in.
      </p>
      <code className="block rounded border border-line bg-bg px-2 py-1.5 font-mono text-xs break-all select-all">
        {value}
      </code>
      <div>
        <Button onClick={onDismiss}>I have stored it</Button>
      </div>
    </div>
  );
}

function reasonSuffix(person: Row): string {
  const reason = field(person, "suspendedReason");
  return reason === "" ? "" : `: ${reason}`;
}

