import { Caption, Field, Input, Select } from "../../../../../kit";
import { DEPLOYABLE_KINDS, NOT_OFFERED_SENTENCE } from "../../../targets";
import type { ComposeDraft } from "../../compose";

// The two fields more than one branch of the Source stop asks for.
//
// They live beside the stop rather than inside it because the repository
// answer is its own component (TokenSourceForm.tsx) and both need the name;
// a shared field imported from the stop that mounts the form would be a
// module cycle.

// ---------------------------------------------------------------------------
// The two fields the hand-made paths share
// ---------------------------------------------------------------------------

export function NameField({
  draft,
  onDraft,
  label,
  placeholderFrom,
}: {
  draft: ComposeDraft;
  onDraft: (patch: Partial<ComposeDraft>) => void;
  label: string;
  placeholderFrom: string;
}) {
  return (
    <Field label={label}>
      <Input
        id="os-compose-name"
        label="What this deployable is called"
        value={draft.name}
        onChange={(name) => onDraft({ name })}
        placeholder={placeholderFrom || "storefront"}
      />
    </Field>
  );
}

/**
 * The kind, for a deployable this cluster is not analyzing.
 *
 * A package declares each app's kind in its manifest and the report reads it
 * back; a built-site zip and a CI push declare nothing, so the choice is the
 * person's. The one sentence about the three kinds that are NOT offered sits
 * beneath it, said once, in place of three disabled controls.
 */
export function KindField({ draft, onDraft }: { draft: ComposeDraft; onDraft: (patch: Partial<ComposeDraft>) => void }) {
  const chosen = DEPLOYABLE_KINDS.find((k) => k.value === draft.kind);
  return (
    <>
      <Field label="What kind">
        <Select
          id="os-compose-kind"
          label="What kind of deployable this is"
          value={draft.kind}
          onChange={(kind) => onDraft({ kind })}
        >
          <option value="">Choose a kind</option>
          {DEPLOYABLE_KINDS.map((kind) => (
            <option key={kind.value} value={kind.value}>
              {kind.label}
            </option>
          ))}
        </Select>
      </Field>
      {chosen ? <Caption>{chosen.blurb}</Caption> : null}
      <Caption>{NOT_OFFERED_SENTENCE}</Caption>
    </>
  );
}
