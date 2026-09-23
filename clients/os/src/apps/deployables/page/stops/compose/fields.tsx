import { Caption, Field, Input, Select } from "../../../../../kit";
import { storeLongLabel } from "../../../store/rows";
import { useStoreList } from "../../../store/useStore";
import { DEPLOYABLE_KINDS } from "../../../targets";
import type { ComposeDraft } from "../../compose";

// The two fields more than one branch of the Source stop asks for.
//
// They live beside the stop rather than inside it because the repository
// answer is its own component (RepositorySource.tsx) and both need the name;
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
      {draft.kind === "shopify_storefront" ? <StoreField draft={draft} onDraft={onDraft} /> : null}
    </>
  );
}

// ---------------------------------------------------------------------------
// The store a storefront fronts
// ---------------------------------------------------------------------------

/**
 * Which registered store this storefront will front.
 *
 * ONE FIELD WHERE THERE WERE TWO FREE-TEXT ONES (epic memql#5530). It asked
 * for the store's domain and the name of the secret holding its Storefront
 * token, and wrote both onto the site row -- a second record of a store the
 * cluster usually already had, typed by hand, with no check that either value
 * named anything real. The site names the store now, and this offers the
 * stores this caller may read, which is exactly the set the engine will
 * accept a binding to.
 *
 * NOT ATTACHING IS AN ANSWER. Registering a store takes a cluster owner and
 * three cluster secrets; requiring one here would stop somebody who can
 * create deployables from creating a storefront at all. The Store pane on the
 * finished deployable is where a store is registered and attached, and it is
 * one place rather than two.
 */
function StoreField({
  draft,
  onDraft,
}: {
  draft: ComposeDraft;
  onDraft: (patch: Partial<ComposeDraft>) => void;
}) {
  const list = useStoreList();
  return (
    <>
      <Field label="Shopify store">
        <Select
          id="os-compose-store"
          label="The Shopify store this storefront fronts"
          value={draft.storeId ?? ""}
          onChange={(storeId) => onDraft({ storeId })}
        >
          <option value="">Attach one later</option>
          {list.stores.map((store) => (
            <option key={store.id} value={store.id}>
              {storeLongLabel(store)}
            </option>
          ))}
        </Select>
      </Field>
      <Caption>
        {list.stores.length === 0
          ? "No Shopify store is registered on this cluster yet. Create the deployable, then register and attach its store from its Store pane."
          : "The storefront names the store; it does not copy it. The domain its pages call and the Storefront token the edge serves are both read from the store row."}
      </Caption>
    </>
  );
}
