import { useState } from "react";

import { Button, Field, Input, Panel } from "../../kit";
import { AccountPicker } from "../accounts/AccountPicker";
import { organizationChosen, useDefaultOrganization } from "../accounts/organization";
import type { AccountRow } from "../accounts/rows";
import type { UsersActions } from "./actions";
import { RefusalLine } from "./PersonPage";

// New group: a SHORT FORM in place of the list, not a rail (design record, D3).
//
// A rail is for a composition whose answers depend on each other -- the Invite
// rail's address decides its groups, which decide its roles. A group is a name,
// a description and an organization, in any order, and drawing three
// independent fields as a numbered journey would be ceremony pretending to be
// guidance.

export function NewGroupForm({
  accounts,
  actions,
  onCancel,
  onCreated,
}: {
  accounts: readonly AccountRow[];
  actions: UsersActions;
  onCancel: () => void;
  /** The row arrives by broadcast; this only leaves the form. */
  onCreated: () => void;
}) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [accountId, setAccountId] = useState("");
  const defaultAccountId = useDefaultOrganization(accounts);
  const selectedAccountId = accountId || defaultAccountId;
  const ready = name.trim() !== "" && organizationChosen(accounts, selectedAccountId);

  const busy = actions.busyKey === `group:new:${name}`;

  // NO Subhead inside: the Head above already says "New group", and rule 7 is
  // that a scope is named in ONE place.
  return (
    <Panel label="A new group">
      <Field label="Name">
        <Input id="group-name" label="Name" value={name} onChange={setName} placeholder="Acme engineering" />
      </Field>
      <Field label="What it is for">
        <Input
          id="group-description"
          label="What it is for"
          value={description}
          onChange={setDescription}
          placeholder="Optional"
        />
      </Field>
      <Field label="Organization">
        <AccountPicker
          value={selectedAccountId}
          onChange={setAccountId}
          accounts={[...accounts]}
          id="group-account"
          label="Organization this group belongs to"
          required
        />
      </Field>

      <RefusalLine actions={actions} />

      <div className="os-form-actions">
        <Button onClick={onCancel}>Cancel</Button>
        <Button
          tone="primary"
          busy={busy}
          busyLabel="Creating..."
          disabled={!ready}
          onClick={() => {
            if (!ready) return;
            void actions.groupCreate(name.trim(), description.trim(), selectedAccountId).then((groupId) => {
              // NOTHING IS INSERTED LOCALLY. The row arrives on its own
              // broadcast with the arrival cue, which is what makes the new
              // group appear the same way it appears in everybody else's
              // window -- and what stops this one showing a group the cluster
              // refused to write.
              //
              // NULL is the refusal; an id and an unreadable reply are both
              // successes, and leaving the form up on the second would invite
              // a second group.
              if (groupId !== null) onCreated();
            });
          }}
        >
          Create group
        </Button>
      </div>
    </Panel>
  );
}
