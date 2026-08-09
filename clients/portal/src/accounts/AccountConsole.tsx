import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";
import type { Concept, Row } from "@znasllc-io/memql-sdk-core/client";

import { ViewElement } from "../views/ViewElement";
import { ACCOUNT_TOKEN_CONCEPT, accountTokenRows } from "./rows";
import {
  EMPTY_DRAFT,
  useAccountConsole,
  type AccountDraft,
  type AccountConsoleState,
} from "./useAccountConsole";

// Managing the customers an operator serves (memql#3322): create, edit,
// archive, and issue or revoke their credentials.
//
// ===========================================================================
// WHY THIS LIVES IN src/accounts/ AND NOT IN src/views/
// ===========================================================================
// The Customers VIEW (memql#3319) reads a population and lays it out, and
// portal_view_composition_test.go holds it to that: no row markup, no
// iteration. Management is a different job -- forms, a confirmation, a
// one-time secret -- and pretending otherwise would either bend the guard or
// smuggle a form into a module whose whole contract is "I only compose
// elements".
//
// So the view renders exactly one thing from here, and the credential LIST
// still goes through the shared element library rather than a hand-drawn
// table: a token list is a row set, and row sets are what the library is for.
//
// ===========================================================================
// NOTHING HERE IS AN AUTHORIZATION GATE
// ===========================================================================
// `canManage` hides controls a reader's write would be refused for anyway --
// a courtesy, not enforcement. The real gates are the engine's: the coarse
// write capability check, and @rowAuthz(owner="ownerUserId") on
// v1:identity:account, which makes the engine AND the owner predicate into
// every read and refuse every update whose target the caller does not own
// (component/memql/rowauthz_enforce.go). A caller who bypasses this UI
// entirely gets the same answer.

export function AccountConsole({
  concept,
  rows,
  selectedRowId,
}: {
  concept: Concept;
  rows: readonly Row[];
  selectedRowId: string;
}): ReactNode {
  const console = useAccountConsole(rows, selectedRowId);
  const [creating, setCreating] = useState(false);

  if (!console.canManage) {
    return (
      <p className="text-sm text-muted">
        Your role is <span className="font-medium text-fg">{console.role || "unknown"}</span>, which
        can read customers but not change them. Managing customers needs writer, admin or owner.
      </p>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <Outcome message={console.message} error={console.error} />
      {console.minted ? (
        <MintedToken
          plainToken={console.minted.plainToken}
          subjectUserId={console.minted.subjectUserId}
          onDismiss={console.dismissMinted}
        />
      ) : null}

      {creating ? (
        <AccountForm
          heading="New customer"
          submitLabel="Create customer"
          initial={EMPTY_DRAFT}
          busy={console.busy}
          onSubmit={(draft) => {
            console.create(draft);
            setCreating(false);
          }}
          onCancel={() => setCreating(false)}
        />
      ) : (
        <div>
          <button
            type="button"
            className="rounded border border-line px-3 py-1.5 text-sm font-medium hover:bg-raised"
            onClick={() => setCreating(true)}
            disabled={console.busy}
          >
            New customer
          </button>
        </div>
      )}

      {console.selected ? (
        <SelectedCustomer selected={console.selected} state={console} concept={concept} />
      ) : (
        <p className="text-sm text-muted">
          Select a customer above to edit it, archive it, or manage its credentials.
        </p>
      )}
    </div>
  );
}

// The selected customer's edit form, archive control and credential panel.
function SelectedCustomer({
  selected,
  state,
  concept,
}: {
  selected: NonNullable<AccountConsoleState["selected"]>;
  state: AccountConsoleState;
  concept: Concept;
}): ReactNode {
  const [editing, setEditing] = useState(false);

  // Leave edit mode when the operator picks a different customer. Without
  // this, an open form silently re-points at the new selection and the next
  // save writes edits to a row the operator was not looking at.
  useEffect(() => {
    setEditing(false);
  }, [selected.id]);

  const archived = selected.status === "archived";

  return (
    <div className="flex flex-col gap-4 rounded border border-line bg-surface p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h3 className="text-sm font-semibold">{selected.name || selected.id}</h3>
          <p className="font-mono text-xs break-all text-subtle">{selected.id}</p>
        </div>
        {!editing ? (
          <div className="flex gap-2">
            <button
              type="button"
              className="rounded border border-line px-3 py-1.5 text-sm hover:bg-raised"
              onClick={() => setEditing(true)}
              disabled={state.busy}
            >
              Edit
            </button>
            <button
              type="button"
              className="rounded border border-line px-3 py-1.5 text-sm hover:bg-raised disabled:opacity-50"
              onClick={() => state.archive()}
              disabled={state.busy || archived}
              // Archiving is reversible-ish and non-destructive -- the row is
              // kept and a write is an append onto the same id -- so it does
              // not warrant a type-to-confirm. Revoking a credential does not
              // either: it takes effect immediately and re-issuing is cheap.
              title={archived ? "Already archived" : "Archive this customer"}
            >
              {archived ? "Archived" : "Archive"}
            </button>
          </div>
        ) : null}
      </div>

      {editing ? (
        <AccountForm
          heading="Edit customer"
          submitLabel="Save changes"
          initial={{
            name: selected.name,
            description: selected.description,
            primaryContactName: selected.primaryContactName,
            primaryContactEmail: selected.primaryContactEmail,
            externalRef: selected.externalRef,
          }}
          busy={state.busy}
          onSubmit={(draft) => {
            state.update(draft);
            setEditing(false);
          }}
          onCancel={() => setEditing(false)}
        />
      ) : null}

      <TokenPanel state={state} concept={concept} />
    </div>
  );
}

function TokenPanel({
  state,
  concept,
}: {
  state: AccountConsoleState;
  concept: Concept;
}): ReactNode {
  const [label, setLabel] = useState("");

  return (
    <div className="flex flex-col gap-3 border-t border-line pt-4">
      <div>
        <h4 className="text-sm font-semibold">Credentials</h4>
        <p className="mt-1 text-xs text-muted">
          Issued to you, on behalf of this customer. Nothing signs in as a customer — the
          credential’s subject is your own user, and the customer is a binding on it. Revoked
          credentials are kept in the list rather than removed, so an audit can see they existed.
        </p>
      </div>

      <form
        className="flex flex-wrap items-end gap-2"
        onSubmit={(event: FormEvent) => {
          event.preventDefault();
          state.mint(label.trim());
          setLabel("");
        }}
      >
        <label className="flex min-w-0 flex-1 flex-col gap-1">
          <span className="text-xs font-medium text-muted">Label</span>
          <input
            className="w-full rounded border border-line bg-bg px-2 py-1.5 text-sm"
            value={label}
            onChange={(event) => setLabel(event.target.value)}
            placeholder="Nightly export job"
          />
        </label>
        <button
          type="submit"
          className="rounded border border-line px-3 py-1.5 text-sm font-medium hover:bg-raised disabled:opacity-50"
          disabled={state.busy}
        >
          Issue credential
        </button>
      </form>

      {state.tokensError ? (
        <p className="text-sm text-fg">Could not read credentials: {state.tokensError}</p>
      ) : state.tokensLoading ? (
        <p className="text-sm text-muted">Reading credentials…</p>
      ) : (
        <>
          <ViewElement
            element={TABLE_ELEMENT}
            rows={accountTokenRows(state.tokens)}
            concept={ACCOUNT_TOKEN_CONCEPT}
            options={{ sort: { field: "issued", direction: "desc" } }}
          />
          <RevokeControl state={state} />
        </>
      )}
      {/* concept is threaded through so the panel sits under the same
          descriptor the page was built from; the credential list has its own
          ConceptLike because a token is not an account row. */}
      <span className="sr-only">{concept.id}</span>
    </div>
  );
}

// Revoking is a separate control rather than a per-row button because the
// element library renders rows and does not host actions inside them -- the
// same reason the view cannot hand-draw a table. The operator names the
// credential to revoke; the list above is what they read it from.
function RevokeControl({ state }: { state: AccountConsoleState }): ReactNode {
  const [identityId, setIdentityId] = useState("");
  const live = state.tokens.filter((token) => token.active).length;

  if (live === 0) {
    return (
      <p className="text-xs text-muted">
        No live credentials for this customer.
      </p>
    );
  }

  return (
    <form
      className="flex flex-wrap items-end gap-2"
      onSubmit={(event: FormEvent) => {
        event.preventDefault();
        if (identityId.trim() === "") return;
        state.revoke(identityId.trim());
        setIdentityId("");
      }}
    >
      <label className="flex min-w-0 flex-1 flex-col gap-1">
        <span className="text-xs font-medium text-muted">Revoke by credential id</span>
        <input
          className="w-full rounded border border-line bg-bg px-2 py-1.5 font-mono text-xs"
          value={identityId}
          onChange={(event) => setIdentityId(event.target.value)}
          placeholder="paste an id from the list above"
        />
      </label>
      <button
        type="submit"
        className="rounded border border-line px-3 py-1.5 text-sm hover:bg-raised disabled:opacity-50"
        disabled={state.busy}
      >
        Revoke
      </button>
    </form>
  );
}

// The one-time plaintext.
//
// Shown once, held in component state and nowhere else -- not storage, not a
// URL, not a row. Only the SHA-256 hash was persisted server-side, so this is
// genuinely the only moment the value exists outside the mint reply, and the
// copy says so in those words rather than a softer "keep it safe".
function MintedToken({
  plainToken,
  subjectUserId,
  onDismiss,
}: {
  plainToken: string;
  subjectUserId: string;
  onDismiss: () => void;
}): ReactNode {
  return (
    <div className="flex flex-col gap-2 rounded border border-accent bg-accent-subtle p-4">
      <h4 className="text-sm font-semibold">Copy this credential now</h4>
      <p className="text-xs">
        You will not see it again. Only its hash was stored, so it cannot be shown a second time —
        if you lose it, revoke it and issue another.
      </p>
      <code className="block rounded border border-line bg-bg px-2 py-1.5 font-mono text-xs break-all select-all">
        {plainToken}
      </code>
      <p className="text-xs text-muted">
        Authenticates as <span className="font-mono">{subjectUserId}</span> — your user, bound to
        this customer.
      </p>
      <div>
        <button
          type="button"
          className="rounded border border-line px-3 py-1.5 text-sm hover:bg-raised"
          onClick={onDismiss}
        >
          I have copied it
        </button>
      </div>
    </div>
  );
}

function Outcome({ message, error }: { message: string; error: string }): ReactNode {
  if (error !== "") {
    return (
      <p className="rounded border border-line bg-surface px-3 py-2 text-sm">
        <span className="font-medium">That did not work.</span> {error}
      </p>
    );
  }
  if (message !== "") {
    return <p className="text-sm text-muted">{message}</p>;
  }
  return null;
}

// One form for create and edit. The fields are the account concept's own
// caller-writable payload -- ownerUserId and status are absent because neither
// is a caller's to set: ownerUserId is @serverSet and stamped from the actor,
// and status moves through archiveAccount.
function AccountForm({
  heading,
  submitLabel,
  initial,
  busy,
  onSubmit,
  onCancel,
}: {
  heading: string;
  submitLabel: string;
  initial: AccountDraft;
  busy: boolean;
  onSubmit: (draft: AccountDraft) => void;
  onCancel: () => void;
}): ReactNode {
  const [draft, setDraft] = useState<AccountDraft>(initial);

  function field(key: keyof AccountDraft, value: string): void {
    setDraft((current) => ({ ...current, [key]: value }));
  }

  return (
    <form
      className="flex flex-col gap-3 rounded border border-line bg-surface p-4"
      onSubmit={(event: FormEvent) => {
        event.preventDefault();
        if (draft.name.trim() === "") return;
        onSubmit({ ...draft, name: draft.name.trim() });
      }}
    >
      <h3 className="text-sm font-semibold">{heading}</h3>

      <Field label="Name" required>
        <input
          className="w-full rounded border border-line bg-bg px-2 py-1.5 text-sm"
          value={draft.name}
          onChange={(event) => field("name", event.target.value)}
          placeholder="Northwind Trading"
        />
      </Field>
      <Field label="What you do for them">
        <input
          className="w-full rounded border border-line bg-bg px-2 py-1.5 text-sm"
          value={draft.description}
          onChange={(event) => field("description", event.target.value)}
          placeholder="Runs their storefront search"
        />
      </Field>
      <Field label="Primary contact">
        <input
          className="w-full rounded border border-line bg-bg px-2 py-1.5 text-sm"
          value={draft.primaryContactName}
          onChange={(event) => field("primaryContactName", event.target.value)}
          placeholder="Ada Fournier"
        />
      </Field>
      <Field label="Contact email">
        <input
          type="email"
          className="w-full rounded border border-line bg-bg px-2 py-1.5 text-sm"
          value={draft.primaryContactEmail}
          onChange={(event) => field("primaryContactEmail", event.target.value)}
          placeholder="ada@northwind.example"
        />
      </Field>
      <Field label="Your reference for them">
        <input
          className="w-full rounded border border-line bg-bg px-2 py-1.5 text-sm"
          value={draft.externalRef}
          onChange={(event) => field("externalRef", event.target.value)}
          placeholder="CRM-4471"
        />
      </Field>

      <div className="flex gap-2">
        <button
          type="submit"
          className="rounded border border-line px-3 py-1.5 text-sm font-medium hover:bg-raised disabled:opacity-50"
          disabled={busy || draft.name.trim() === ""}
        >
          {submitLabel}
        </button>
        <button
          type="button"
          className="rounded px-3 py-1.5 text-sm text-muted hover:text-fg"
          onClick={onCancel}
          disabled={busy}
        >
          Cancel
        </button>
      </div>
    </form>
  );
}

function Field({
  label,
  required = false,
  children,
}: {
  label: string;
  required?: boolean;
  children: ReactNode;
}): ReactNode {
  return (
    <label className="flex flex-col gap-1">
      <span className="text-xs font-medium text-muted">
        {label}
        {required ? <span className="text-subtle"> (required)</span> : null}
      </span>
      {children}
    </label>
  );
}
