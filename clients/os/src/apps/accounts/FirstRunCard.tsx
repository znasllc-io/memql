import { useState } from "react";

import { Button, Fact, Facts, Input, Notice } from "../../kit";
import { useSession } from "../../chrome/access";
import type { UpdateAccountState } from "./actions";
import type { AccountRow } from "./rows";

// The setup card, IN PLACE (D7).
//
// ===========================================================================
// NO MODAL, HERE OR ANYWHERE ELSE IN THE OS
// ===========================================================================
// The card stands in for this app's own surface and for nothing else. Opening
// Files, Fleet, Training or any other window on a cluster whose self account
// is unconfigured is unchanged -- no prompt, no banner, no badge. A first-run
// question that ambushes somebody in the middle of another task is a question
// they dismiss, and a dismissed question needs somewhere to be remembered,
// which is how an unconfigured row becomes a flag in a browser.
//
// This asks in the one place where the answer is the point. The gate is
// `configuredAt` on the row itself (see rows.ts `needsFirstRun`), so the
// answer lives in the cluster and the card is gone for everybody at once.

/**
 * The setup card for `v1:accounts:account:self`.
 *
 * PREPOPULATED FROM TWO PLACES, and the split is what each side can honestly
 * know. The boot filled in `domain` from MEMQL_DOMAIN -- the install's own
 * domain, which is the one fact about the owner's company a starting cluster
 * has. It could not know a name or a contact: on a fresh cluster there is no
 * user row yet, and the first person to sign in is whoever signs in first.
 *
 * So the contact prepopulates HERE, from the session of the person actually
 * reading the card. That is not a fallback for a missing seed -- it is the
 * only point at which the answer exists.
 *
 * `name` IS THE ONLY MUST. Everything else can be corrected later from the
 * ordinary detail view, and a setup card that insists on five fields is one
 * people fill with placeholders to get past.
 */
export function FirstRunCard({
  account,
  update,
  onSaved,
}: {
  account: AccountRow;
  update: UpdateAccountState;
  /** Called after the stamp lands, so the app can re-read and move on. */
  onSaved: () => void;
}) {
  const { access } = useSession();
  const [name, setName] = useState(() => (account.name.trim() === "My company" ? "" : account.name));
  // NO NAME PREFILL, and it is not an oversight. `ProfileAccess` carries
  // userId, primaryEmail and clusterRole -- there is no display name on it, so
  // there is nothing here to prefill FROM. Deriving one from the local part of
  // the email would be a guess rendered as an answer, in the field that names
  // a person.
  const [contactName, setContactName] = useState(() => account.primaryContactName);
  const [contactEmail, setContactEmail] = useState(
    () => account.primaryContactEmail || access?.primaryEmail || "",
  );

  const ready = name.trim() !== "";

  async function save() {
    if (!ready) return;
    const ok = await update.update(account.id, {
      name,
      primaryContactName: contactName,
      primaryContactEmail: contactEmail,
    });
    if (ok) onSaved();
  }

  return (
    <div className="os-app-stack">
      <section className="os-account-firstrun" aria-labelledby="os-account-firstrun-title">
        <p className="os-account-firstrun-eyebrow">Accounts</p>
        <h2 className="os-account-firstrun-title" id="os-account-firstrun-title">
          This instance is yours.
        </h2>
        <p className="os-account-firstrun-line">
          Accounts are the clients you do work for -- the companies whose sites this cluster hosts,
          whose files it stores, whose people it invites. Yours is the first one, and it is already
          here. It just needs a name.
        </p>

        <div className="os-account-firstrun-form">
          <div className="os-form-field">
            <label className="os-form-field-label" htmlFor="os-account-firstrun-name">
              What is your company called?
            </label>
            <Input
              id="os-account-firstrun-name"
              label="Your company's name"
              value={name}
              onChange={setName}
              placeholder="Acme Consulting"
              onEnter={save}
            />
          </div>

          <div className="os-form-field">
            <label className="os-form-field-label" htmlFor="os-account-firstrun-contact">
              Who should people reach?
            </label>
            <Input
              id="os-account-firstrun-contact"
              label="Primary contact name"
              value={contactName}
              onChange={setContactName}
              placeholder="Your name"
            />
          </div>

          <div className="os-form-field">
            <label className="os-form-field-label" htmlFor="os-account-firstrun-email">
              At what address?
            </label>
            <Input
              id="os-account-firstrun-email"
              label="Primary contact email"
              value={contactEmail}
              onChange={setContactEmail}
              placeholder="you@example.com"
            />
          </div>
        </div>

        {/* The domain is shown as a FACT, not a field. The install already
            knows it -- it is the domain this cluster serves -- and offering it
            as an input on a setup card invites somebody to change the one
            value here that was not a guess. The detail view edits it. */}
        <Facts>
          <Fact
            label="Domain"
            value={account.domain}
            mono
            title="Taken from this install's own domain. Correct it later from the client's profile if it is wrong."
          />
        </Facts>

        {update.error === "" ? null : (
          <Notice
            tone="error"
            sentence="This did not save."
            next="Nothing was written; the details below are still as you typed them."
            detail={update.error}
          />
        )}

        <div className="os-account-firstrun-actions">
          <Button tone="primary" onClick={save} disabled={!ready || update.busy}>
            {update.busy ? "Saving" : "Save and continue"}
          </Button>
          {ready ? null : (
            <span className="os-caption">A name is the one thing this needs.</span>
          )}
        </div>
      </section>
    </div>
  );
}
