import { useState } from "react";

import { Button, CopyValue } from "../kit";
import { EntryLayout } from "../kit/EntryLayout";
import { AddKeyWizard } from "../apps/settings/apikeys/AddKeyWizard";
import { useVendorKeys } from "../apps/settings/apikeys/useVendorKeys";
import { Mark } from "./Mark";

// THE PAGE A PERSON WITH NO MODEL ACCESS GETS (design record
// 2026-11-24-desktop-access, section 4). CoreGate draws it in place of
// everything when `desktopAccess` holds the person: not a cluster owner or
// developer, and nothing set up.
//
// THE GATE'S COLUMN, WITH LESS IN IT: the mark, a title, one sentence, then
// what the person can do. Two shapes for "you cannot come in yet" would read
// as two different kinds of problem.
//
// THE REFERENCE IS WHAT AN ADMINISTRATOR SEARCHES IN LOGS. It is shown only
// when the engine wrote its line before answering; a held answer whose line
// could not be stored carries none, and then the page neither shows one nor
// asks the person to hand one over, so it never shows a reference that Logs
// cannot find. The kit's copy control copies it whole.
//
// ADDING A KEY HAPPENS HERE, in the entry frame, not in a Settings window: a
// window would need the desk drawn under it, and the desk is exactly what a
// held person does not get. The wizard is the one Settings > API keys runs, so
// the key a person adds here is the same key, sealed the same way. Its way out
// comes back to this page; a key added asks again (`onAdded`), and the page
// lifts by itself if the key counts.

export function AccessHeld({ reference, canAddKey, onSignOut, onAdded }: {
  /** "" when the hold line could not be written: no reference line. */
  reference: string;
  /** The person holds `execute app:settings/apikeys`. */
  canAddKey: boolean;
  onSignOut(): void;
  /** A key was added on the page (or may have been): ask again. */
  onAdded(): void;
}) {
  const [adding, setAdding] = useState(false);

  if (adding && canAddKey) {
    return (
      <AddingKey
        onCancel={() => setAdding(false)}
        onAdded={() => {
          setAdding(false);
          onAdded();
        }}
      />
    );
  }

  return (
    <EntryLayout><main className="os-core-gate" data-os-core-gate="access">
      <div className="os-core-gate-column">
        <span className="os-core-gate-mark" aria-hidden>
          <Mark className="os-ask-mark" />
        </span>
        <h1 className="os-core-gate-head">Your access is not set up yet</h1>
        <p className="os-core-gate-body">
          {reference === ""
            ? "MemQL needs a model it can use for you. Ask your administrator to set one up."
            : "MemQL needs a model it can use for you. Ask your administrator to set one up, and give them this reference."}
        </p>
        {reference === "" ? null : (
          <div className="os-access-reference">
            <CopyValue value={reference} label="reference" displayValue={<span className="os-mono">{reference}</span>} />
          </div>
        )}
        {/* True because the page asks again on its own (useDesktopAccess):
            nothing an administrator changes reaches this person's stream. */}
        <p className="os-core-gate-note">This page opens by itself once it is set up.</p>
        {canAddKey ? <Button tone="primary" onClick={() => setAdding(true)}>Add an API key</Button> : null}
        {/* The way out, under its own hairline: an escape, never one of the acts. */}
        <div className="os-core-gate-out">
          <Button onClick={onSignOut}>Sign out</Button>
        </div>
      </div>
    </main></EntryLayout>
  );
}

/**
 * The add wizard, in the entry frame. Its own component so the person's keys
 * are read only while a key is being added, for the names a new one may not
 * repeat.
 */
function AddingKey({ onCancel, onAdded }: { onCancel: () => void; onAdded: () => void }) {
  const keys = useVendorKeys();
  return (
    <EntryLayout>
      <AddKeyWizard keys={keys} taken={keys.mine.map((card) => card.label)} onCancel={onCancel} onAdded={() => onAdded()} from={null} />
    </EntryLayout>
  );
}
