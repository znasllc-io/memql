import { useMemo, useState } from "react";

import { Button, Caption, Field, FormRow, Input, Notice, Select } from "../../../kit";
import { useCredentialActions } from "../packages/actions";
import { credentialIsRevoked, type CredentialRow } from "./rows";
import { SOURCE_HOST } from "./probe";

// The credential control: pick one of yours, or add one (epic memql#4885,
// design section D).
//
// ===========================================================================
// THE TOKEN IS TYPED ONCE, IN THE CLEAR, AND LEAVES IN ONE CALL
// ===========================================================================
// The token field is NOT masked, and that is the posture every write-only
// secret field in this shell takes -- the reasoning is written out at
// `apps/settings/IntegrationsSection.tsx`'s SlotRow and holds here unchanged:
// the value is pasted once and never read back, so masking protects nothing
// the card does not already refuse to show, while hiding a trailing character
// in a pasted credential, which then fails later as an opaque vendor error.
//
// It reaches `sourceCredentialCreate` and nothing else. It is a parameter of
// one function all the way down (calls.ts), is never held on a hook, and no
// other call on this surface takes a token at all: a package names a
// credential ID. `test/deployables/compose.test.tsx` reads every captured
// call string and holds that.
//
// ===========================================================================
// SAYING WHAT KIND OF TOKEN TO MAKE IS PART OF THE CONTROL
// ===========================================================================
// "Paste a token" with no shape is how somebody ends up pasting a classic
// token with repo-admin scope into a box, or a fine-grained one with no
// contents permission that then fails at fetch. The sentence the design fixes
// is rendered beside the field, not in a doc.

/** The one non-id value the picker can hold: "make me a new one". */
const ADD = "__add__";

/** No credential at all -- what a public repository needs. */
export const NO_CREDENTIAL = "";

export const TOKEN_SHAPE =
  "A fine-grained personal access token with contents: read on the repositories it should reach, and nothing else. " +
  "It is sealed in the cluster and read only at fetch time; nothing shows it again.";

export function CredentialField({
  id,
  credentials,
  value,
  onChange,
  label = "Credential",
  disabled = false,
  allowNone = true,
  host = SOURCE_HOST,
}: {
  id: string;
  /** The caller's cards, from the app root's one feed. */
  credentials: readonly CredentialRow[];
  value: string;
  /** Called with the chosen credential's id -- including the one just added. */
  onChange: (credentialId: string) => void;
  label?: string;
  disabled?: boolean;
  /** Whether "no credential" is offered. A public repository needs it; the Sources group does not. */
  allowNone?: boolean;
  host?: string;
}) {
  const [adding, setAdding] = useState(false);

  const offered = useMemo(() => {
    // ACTIVE ONES ARE THE CHOICES, and a revoked one is kept only when it is
    // the value already held -- somebody switching a source AWAY from a
    // revoked credential is exactly the person who has to see it named. The
    // rule is AccountPicker's, for its reason: dropping the held value would
    // silently re-point the source at whatever option came first.
    const list = credentials
      .filter((c) => c.host === host && (!credentialIsRevoked(c) || c.id === value))
      .map((c) => ({ id: c.id, label: cardLabel(c) }));
    if (value !== "" && !list.some((o) => o.id === value)) {
      list.unshift({ id: value, label: `${value} (not visible to you)` });
    }
    return list;
  }, [credentials, value, host]);

  if (adding) {
    return (
      <AddCredential
        id={id}
        host={host}
        onCancel={() => setAdding(false)}
        onAdded={(credentialId) => {
          setAdding(false);
          onChange(credentialId);
        }}
      />
    );
  }

  return (
    <>
      <Field label={label}>
        <Select
          id={id}
          label={`The credential this source is fetched under, on ${host}`}
          value={value}
          onChange={(next) => (next === ADD ? setAdding(true) : onChange(next))}
        >
          {allowNone ? <option value={NO_CREDENTIAL}>No credential -- a public repository</option> : null}
          {offered.map((o) => (
            <option key={o.id} value={o.id} disabled={disabled}>
              {o.label}
            </option>
          ))}
          <option value={ADD}>Add a credential...</option>
        </Select>
      </Field>
    </>
  );
}

/**
 * The add form, which is the only place a token exists on this surface.
 *
 * It REPLACES the picker rather than opening beneath it: a select and the
 * form that adds an option to it, both live, is two controls answering one
 * question. Cancel puts the picker back with whatever was chosen before.
 */
export function AddCredential({
  id,
  host,
  onAdded,
  onCancel,
}: {
  id: string;
  host: string;
  onAdded: (credentialId: string) => void;
  onCancel?: () => void;
}) {
  const actions = useCredentialActions();
  const [label, setLabel] = useState("");
  const [token, setToken] = useState("");
  const ready = label.trim() !== "" && token.trim() !== "";

  async function save() {
    if (!ready) return;
    const added = await actions.add({ host, label: label.trim(), token: token.trim() });
    if (added.credentialId === "") return;
    // THE TOKEN GOES FIRST, before anything renders again. It has done the
    // one thing it exists for.
    setToken("");
    setLabel("");
    onAdded(added.credentialId);
  }

  return (
    <div className="os-stop-form" role="group" aria-label={`Add a credential for ${host}`}>
      <Field label="What to call it">
        <Input
          id={`${id}-label`}
          label={`A name for this ${host} credential`}
          value={label}
          onChange={setLabel}
          placeholder="work laptop"
        />
      </Field>
      <Field label={`Token for ${host}`}>
        <Input
          id={`${id}-token`}
          label={`The ${host} access token`}
          value={token}
          onChange={setToken}
          placeholder="github_pat_..."
          onEnter={() => void save()}
        />
      </Field>
      <Caption>{TOKEN_SHAPE}</Caption>
      <FormRow>
        <Button tone="primary" disabled={!ready} busy={actions.busy} busyLabel="Sealing" onClick={() => void save()}>
          Add credential
        </Button>
        {onCancel ? (
          <Button
            tone="quiet"
            onClick={() => {
              setToken("");
              onCancel();
            }}
          >
            Cancel
          </Button>
        ) : null}
      </FormRow>
      {actions.refusal ? (
        <Notice
          tone="error"
          sentence="That credential was not stored."
          next="Nothing was written, and the token was not kept anywhere."
          detail={actions.refusal.message}
        />
      ) : null}
    </div>
  );
}

/** A card as one line: what somebody called it, and the fingerprint that tells two apart. */
export function cardLabel(card: CredentialRow): string {
  const name = card.label.trim() === "" ? card.id : card.label.trim();
  const mark = card.fingerprint.trim() === "" ? "" : ` ${card.fingerprint.trim()}`;
  return credentialIsRevoked(card) ? `${name}${mark} (revoked)` : `${name}${mark}`;
}
