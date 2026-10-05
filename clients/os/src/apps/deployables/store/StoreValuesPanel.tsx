import { useEffect, useId, useState } from "react";
import { Plus, Trash2 } from "lucide-react";

import { Button, Caption, Fact, Facts, Input, Notice, Panel, RecordList, RecordRow, Subhead } from "../../../kit";
import { useOsConnection } from "../../../live/connection";
import { saveSiteStoreSettings } from "../packages/calls";
import type { SiteRow } from "../rows";
import type { SettingRow } from "../settings-editor";
import {
  EMPTY_STORE_DRAFT,
  conflictingStoreIds,
  editStore,
  keepDraftOverStored,
  keptStoreIds,
  rowsForStore,
  setKeptRemoved,
  settleDraft,
  storeCountProblem,
  storeRowProblems,
  storeSettingsFingerprint,
  storeSettingsFromDraft,
  storeTargets,
  storeValuesDirty,
  takeStoredWhereMoved,
  type StoreDestination,
  type StoreTarget,
  type StoreValuesDraft,
} from "./storeValues";

// STORE VALUES -- the values that belong to ONE STORE (memql#5602), on the
// page where the stores are.
//
// ===========================================================================
// WHY HERE AND NOT IN APP VALUES
// ===========================================================================
// A Customer Account API client id is one store's Headless channel's; a
// wholesale adapter is configured for one store. They are app values in every
// other respect -- read by the bundle at load, public, held to the same rules
// -- but what they BELONG to is a store, and this page is where a storefront's
// stores are: Production's, and Testing's. So each store carries its values
// here, under its own domain, the way a connected machine carries its models
// in Fleet. App values keeps the values that belong to the site, and says
// where these went.
//
// ===========================================================================
// WHICH STORE, AND WHAT HAPPENS WITHOUT ONE, SAID WHERE IT IS DECIDED
// ===========================================================================
// Each group leads with the store's DOMAIN -- the identifier somebody checks
// against the Shopify admin -- and names the website it supplies after it. A
// store both websites use is ONE group: the values are keyed by store, so two
// editors would be two views of one entry. A store with no values says the
// app values apply to it, and a value that replaces an app value says which,
// on its own row. The rule itself is stated once, above them.
//
// ===========================================================================
// ONE SAVE, AND IT SENDS EVERY STORE'S ENTRY
// ===========================================================================
// updateSiteStoreSettings REPLACES the map, so a save sends the stored map
// with this person's edits laid over it (`storeValues.ts`) -- the entries of
// stores this storefront no longer uses included. Those are listed beneath,
// because they are kept on purpose (pointing a binding back restores them)
// and because the sixteen-store limit is answered by removing them.
//
// ===========================================================================
// A CHANGE MADE ELSEWHERE NEVER SILENTLY REPLACES AN EDIT
// ===========================================================================
// `site` is re-projected whenever any deployable changes, so nothing here keys
// on its identity. An untouched store follows the row; a store somebody is
// editing keeps their edit, and if the row moves under it the panel says so
// and Save waits for them to choose (Fleet's sharing dialog, memql#5659).

const ABOUT =
  "Read by the storefront when it loads, for one store only. A value here replaces the app value with the same name for that store; other names keep the app value. Public, like every app value.";

export function StoreValues({
  site,
  canEdit,
  labelFor,
}: {
  site: SiteRow;
  /** Whether this person may write the deployable's runtime values -- App values' own gate. */
  canEdit: boolean;
  /** How a store is named on screen: its domain when the store reads back, else its id. */
  labelFor: (storeId: string) => string;
}) {
  const connection = useOsConnection();
  const stored = site.storeSettings;
  const storedKey = storeSettingsFingerprint(stored);
  const targets = storeTargets(site);
  const kept = keptStoreIds(stored, targets);
  const [draft, setDraft] = useState<StoreValuesDraft>(EMPTY_STORE_DRAFT);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState("");

  // ANOTHER DEPLOYABLE IS ANOTHER DRAFT.
  useEffect(() => {
    setDraft(EMPTY_STORE_DRAFT);
    setRefusal("");
  }, [site.id]);

  // THE ROW MOVED: let go of every store whose edit now says exactly what is
  // stored -- this save landing, or somebody making the same change. KEYED ON
  // WHAT IS STORED, never on the draft: an edit typed back to the stored value
  // must not be swept away mid-keystroke, remounting the field it is in.
  useEffect(() => {
    setDraft((held) => settleDraft(held, stored));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [storedKey]);

  // SYSTEM-OWNED ROWS RENDER NOTHING HERE, the rule App values keeps: the
  // engine refuses the write whoever asks.
  if (site.systemOwned) return null;

  const map = storeSettingsFromDraft(draft, stored);
  const dirty = storeValuesDirty(draft, stored);
  const conflicts = conflictingStoreIds(draft, stored);
  const conflict = conflicts.length > 0;
  const problems = Object.fromEntries(targets.map((t) => [t.storeId, storeRowProblems(rowsForStore(draft, stored, t.storeId))]));
  const rowsInvalid = Object.values(problems).some((list) => list.some((p) => p !== ""));
  const countProblem = storeCountProblem(map);
  const savable = dirty && !conflict && !rowsInvalid && countProblem === "";

  function rowsOf(storeId: string): readonly SettingRow[] {
    return rowsForStore(draft, stored, storeId);
  }
  function edit(storeId: string, next: (rows: readonly SettingRow[]) => readonly SettingRow[]): void {
    setRefusal("");
    setDraft((held) => editStore(held, stored, storeId, next(rowsForStore(held, stored, storeId))));
  }

  async function save(): Promise<void> {
    if (connection === null) {
      setRefusal("Not connected to the cluster, so nothing was written.");
      return;
    }
    setBusy(true);
    setRefusal("");
    try {
      await saveSiteStoreSettings(connection.query, site.id, map);
      // NOTHING IS WRITTEN LOCALLY. The row comes back on its own broadcast --
      // v1:platform:site broadcasts updates -- and the edit lets go when it
      // does (`settleDraft`), so what this shows is always what the cluster
      // holds or what this person has not saved yet.
    } catch (err: unknown) {
      setRefusal(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Panel label="Store values">
      <Subhead>Store values</Subhead>
      <Caption>{ABOUT}</Caption>

      {targets.length === 0 ? (
        <Caption>Connect a store to give it values of its own.</Caption>
      ) : (
        <div className="os-store-values">
          {targets.map((target) => (
            <StoreGroup
              key={target.storeId}
              siteId={site.id}
              target={target}
              label={labelFor(target.storeId)}
              rows={rowsOf(target.storeId)}
              problems={problems[target.storeId] ?? []}
              appValues={site.settings}
              canEdit={canEdit}
              onEdit={(next) => edit(target.storeId, next)}
            />
          ))}
        </div>
      )}

      {kept.length > 0 ? (
        <KeptStores
          kept={kept}
          stored={stored}
          removed={draft.removed}
          labelFor={labelFor}
          canEdit={canEdit}
          onToggle={(storeId, remove) => {
            setRefusal("");
            setDraft((held) => setKeptRemoved(held, stored, storeId, remove));
          }}
        />
      ) : null}

      {conflict ? (
        // WHAT EACH STORE IS NOW, then the two answers, side by side and equal:
        // neither writes anything -- the write stays on Save -- so neither is
        // primary. Keep my changes is the one that, saved, replaces the change.
        <Notice tone="warn" sentence="Store values changed somewhere else while you were editing.">
          {conflicts.map((storeId) => (
            <p key={storeId} className="os-caption">
              Now, for {labelFor(storeId)}: <span className="os-store-values-now">{nowSentence(stored[storeId] ?? {})}</span>
            </p>
          ))}
          <div className="os-store-values-acts">
            <Button onClick={() => setDraft((held) => takeStoredWhereMoved(held, stored))}>Use the new values</Button>
            <Button onClick={() => setDraft((held) => keepDraftOverStored(held, stored))}>Keep my changes</Button>
          </div>
        </Notice>
      ) : null}

      {countProblem === "" ? null : <p className="os-settings-problem">{countProblem}</p>}

      {canEdit && (targets.length > 0 || kept.length > 0) ? (
        <div className="os-panel-actions">
          <Button tone="primary" disabled={!savable} busy={busy} busyLabel="Saving" onClick={() => void save()}>
            Save store values
          </Button>
        </div>
      ) : null}

      {/* THE ENGINE IS THE LAW, in its own words: the value caps, the `Ref`
          rule and the store-id form are checked beside its write path, and
          its sentence names the knob or the remedy. */}
      {refusal === "" ? null : <Notice tone="error" sentence="Store values were not saved." next="Your changes are still here." detail={refusal} />}
    </Panel>
  );
}

/** One store's stored values as the conflict notice says them. */
function nowSentence(values: Readonly<Record<string, string>>): string {
  const keys = Object.keys(values).sort();
  if (keys.length === 0) return "no values of its own.";
  return `${keys.map((key) => `${key} is ${quoted(values[key] ?? "")}`).join(", ")}.`;
}

/** "Production", "Testing", or "Production and Testing". */
function destinationWords(destinations: readonly StoreDestination[]): string {
  const words = destinations.map((d) => (d === "production" ? "Production" : "Testing"));
  return words.join(" and ");
}

/** An app value, short enough to sit in a sentence. Public anyway, as every value is. */
function quoted(value: string): string {
  const flat = value.replace(/\s+/g, " ").trim();
  return flat.length > 48 ? `${flat.slice(0, 47)}…` : flat;
}

function StoreGroup({
  siteId,
  target,
  label,
  rows,
  problems,
  appValues,
  canEdit,
  onEdit,
}: {
  siteId: string;
  target: StoreTarget;
  label: string;
  rows: readonly SettingRow[];
  problems: readonly string[];
  /** The site's own values, for saying which one a store's value replaces. */
  appValues: Readonly<Record<string, string>>;
  canEdit: boolean;
  /** A change to this store's rows, as a function of the rows it applies to. */
  onEdit: (next: (rows: readonly SettingRow[]) => readonly SettingRow[]) => void;
}) {
  const headingId = useId();
  const where = destinationWords(target.destinations);
  const fieldId = (rowId: string, part: string) => `os-store-value-${part}-${siteId}-${target.storeId}-${rowId}`;
  // BY ROW ID, inside the updater: the rows a change applies to are the ones
  // held when it lands, not the ones this render closed over.
  const patch = (rowId: string, change: Partial<SettingRow>) =>
    onEdit((held) => held.map((r) => (r.id === rowId ? { ...r, ...change } : r)));
  return (
    <section className="os-store-values-store" aria-labelledby={headingId}>
      <h4 className="os-store-values-head" id={headingId}>
        <span className="os-store-values-name">{label}</span>
        <span className="os-store-values-where">{where}</span>
      </h4>
      {rows.length === 0 ? (
        <Caption>
          No values of its own, so {where} {target.destinations.length > 1 ? "use" : "uses"} the app values.
        </Caption>
      ) : canEdit ? (
        <>
          {/* The two columns, named once per store. Every field keeps its own
              hidden label, which says which store it is for. */}
          <div className="os-settings-header" aria-hidden>
            <span>Name</span>
            <span>Value</span>
            <span />
          </div>
          <ul className="os-settings-rows">
            {rows.map((row, i) => {
              const replaces = appValues[row.key.trim()];
              return (
                <li key={row.id} className="os-settings-row">
                  <Input
                    id={fieldId(row.id, "key")}
                    label={`Value name for ${label}`}
                    placeholder="customerAccountClientId"
                    value={row.key}
                    onChange={(next) => patch(row.id, { key: next })}
                    code
                  />
                  <Input
                    id={fieldId(row.id, "value")}
                    label={`${row.key.trim() === "" ? "This value" : row.key.trim()} for ${label}`}
                    value={row.value}
                    onChange={(next) => patch(row.id, { value: next })}
                    code
                  />
                  <Button
                    tone="quiet"
                    onClick={() => onEdit((held) => held.filter((r) => r.id !== row.id))}
                    ariaLabel={row.key.trim() === "" ? `Remove this value for ${label}` : `Remove ${row.key.trim()} for ${label}`}
                  >
                    <Trash2 size={12} aria-hidden /> Remove
                  </Button>
                  {problems[i] ? (
                    <p className="os-settings-problem">{problems[i]}</p>
                  ) : replaces !== undefined ? (
                    <p className="os-store-values-note">Replaces the app value &ldquo;{quoted(replaces)}&rdquo; for this store.</p>
                  ) : null}
                </li>
              );
            })}
          </ul>
        </>
      ) : (
        <Facts>
          {rows.map((row) => (
            <Fact key={row.id} label={row.key} value={row.value} mono />
          ))}
        </Facts>
      )}
      {canEdit ? (
        <div>
          <Button tone="quiet" onClick={() => onEdit((held) => [...held, newRow()])} ariaLabel={`Add a value for ${label}`}>
            <Plus size={12} aria-hidden /> Add a value
          </Button>
        </div>
      ) : null}
    </section>
  );
}

/**
 * The stores whose values are kept although no binding names them.
 *
 * LISTED, NOT HIDDEN, for two reasons: they are kept on purpose -- pointing a
 * binding back restores them -- and the sixteen-store limit is answered by
 * removing them, which needs somewhere to do it. Removing one is a mark until
 * Save, with its inverse beside it.
 */
function KeptStores({
  kept,
  stored,
  removed,
  labelFor,
  canEdit,
  onToggle,
}: {
  kept: readonly string[];
  stored: SiteRow["storeSettings"];
  removed: readonly string[];
  labelFor: (storeId: string) => string;
  canEdit: boolean;
  onToggle: (storeId: string, remove: boolean) => void;
}) {
  return (
    <div className="os-store-values-kept">
      <Caption>Kept for stores this storefront does not use now. They are not served, and return if the store is connected again.</Caption>
      <RecordList as="ul" label="Values kept for other stores">
        {kept.map((storeId) => {
          const count = Object.keys(stored[storeId] ?? {}).length;
          const marked = removed.includes(storeId);
          const label = labelFor(storeId);
          return (
            <RecordRow
              key={storeId}
              name={label}
              secondary={`${count} ${count === 1 ? "value" : "values"}`}
              state={marked ? "Removed when you save" : undefined}
              tone="muted"
              dim={marked}
              actionLayout="compact"
              actions={
                canEdit ? (
                  <Button tone="quiet" onClick={() => onToggle(storeId, !marked)} ariaLabel={`${marked ? "Keep" : "Remove"} the values kept for ${label}`}>
                    {marked ? "Keep" : "Remove"}
                  </Button>
                ) : null
              }
            />
          );
        })}
      </RecordList>
    </div>
  );
}

let rowSeq = 0;

/** A fresh row. Its id is the LIST KEY and never the value's name -- keying on
 *  the name would remount the field somebody is typing the name into. */
function newRow(): SettingRow {
  rowSeq += 1;
  return { id: `new-store-value-${rowSeq}`, key: "", value: "" };
}
