import { useEffect, useMemo, useState } from "react";
import { Plus, Trash2 } from "lucide-react";

import { Button, Caption, Fact, Facts, Input, Switch } from "../../../../kit";
import { useOsConnection } from "../../../../live/connection";
import { ProblemNotice } from "../../packages/ReportView";
import { saveShopperForms, saveSiteSettings } from "../../packages/calls";
import type { SiteRow } from "../../rows";
import {
  SETTINGS_KEY_FORM,
  settingsFingerprint,
  settingsKeyProblem,
  settingsRows,
  toSettingsMap,
  type SettingRow,
} from "../../settings-editor";

// The key-values a bundle reads at load (epic memql#4906, decision P7).
//
// ===========================================================================
// THE ONE SENTENCE THAT MATTERS IS ON THE PANEL, NOT IN A TOOLTIP
// ===========================================================================
// Every value here is served to every visitor of this deployable,
// unauthenticated, in a document their browser fetches. A person adding an
// API base URL will reach for an API KEY next, and the moment to say so is
// while they are looking at the empty field -- not in a refusal after they
// typed one, and not in documentation. So the sentence stands above the
// editor in both states, including the empty one.
//
// The server refuses a key ending in `Ref` for the same reason, and that
// refusal renders here verbatim: it names the storefront binding as the one
// place a reference belongs, which is the fact a paraphrase would drop.
//
// ===========================================================================
// ONE SAVE, AND IT SENDS THE WHOLE MAP
// ===========================================================================
// `updateSiteSettings` REPLACES rather than merges, so what is on screen is
// what the row will hold -- which is what makes removing a setting possible
// at all. Per-row saves would need a delete call beside the write and would
// leave the panel able to show a state the row was never in.
//
// ===========================================================================
// THE FORM RULES ARE MIRRORED FOR A KEYSTROKE-RATE ANSWER, AND SAY SO
// ===========================================================================
// The key's shape is checked here so somebody typing `api-base` learns it
// before saving. The caps and the `Ref` rule are the SERVER's and are not
// mirrored: a browser cannot know this cluster's configured limits, and a
// refusal that arrives from the engine names the knob, which is what an
// operator needs. Both halves are the site hostname field's own precedent.

const NOT_A_SECRET =
  "Read by the app when it loads, and served to everyone who visits it. Not a place for a secret -- put credentials in the cluster's secrets. A storefront names its Storefront token on its store, which is the one reference the edge resolves.";

export function RuntimeSettingsPanel({ site, canEdit }: { site: SiteRow; canEdit: boolean }) {
  const connection = useOsConnection();
  // THE SETTINGS' OWN VALUES, not the object holding them, and the difference
  // is a bug somebody types into. `site` is re-projected whenever the live
  // collection changes -- which is whenever ANY deployable in the cluster is
  // published, renamed or paused -- so `site.settings` is a fresh object many
  // times a minute on a busy cluster. An effect keyed on it would reset the
  // draft under the hands of somebody halfway through typing a value,
  // because a colleague deployed something unrelated.
  //
  // Keyed on the serialized VALUES, the re-seed happens when this
  // deployable's settings actually changed, which is the case it is for.
  const storedKey = settingsFingerprint(site.settings);
  const stored = useMemo(() => settingsRows(site.settings), [storedKey]);
  const [draft, setDraft] = useState<SettingRow[]>(stored);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState("");

  // Re-seed when this deployable's stored settings change -- this save
  // landing, another tab's, or a different deployable selected. An edit in
  // progress is then deliberately discarded rather than merged: a half-typed
  // key silently surviving a change somebody else made is how two people
  // overwrite each other and neither is told.
  useEffect(() => {
    setDraft(stored);
    setRefusal("");
  }, [stored, site.id]);

  // SYSTEM-OWNED ROWS RENDER NO CONTROLS AT ALL, the rule this app already
  // states for the lifecycle: the server refuses the write whoever asks, and a row of
  // disabled fields is a form a person has to read to learn it is not for
  // them.
  if (site.systemOwned) return null;

  if (!canEdit) {
    return (
      <section className="os-report-part">
        <h4 className="os-report-heading">Settings</h4>
        <Caption>{NOT_A_SECRET}</Caption>
        {stored.length === 0 ? (
          <Caption>No app values have been added.</Caption>
        ) : (
          <Facts>
            {stored.map((row) => (
              <Fact key={row.key} label={row.key} value={row.value} mono />
            ))}
          </Facts>
        )}
      </section>
    );
  }

  const problems = draft.map((row) => settingsKeyProblem(row.key, draft));
  const firstProblem = problems.find((p) => p !== "") ?? "";
  const dirty = settingsFingerprint(toSettingsMap(draft)) !== storedKey;

  function update(index: number, patch: Partial<SettingRow>) {
    setDraft((rows) => rows.map((row, i) => (i === index ? { ...row, ...patch } : row)));
  }

  async function save() {
    if (connection === null) return;
    setBusy(true);
    setRefusal("");
    try {
      await saveSiteSettings(connection.query, site.id, toSettingsMap(draft));
      // NOTHING IS WRITTEN LOCALLY. The row arrives on its own broadcast --
      // v1:platform:site broadcasts updates -- with the arrival cue, exactly
      // like a save somebody else made. A local copy would put a state on
      // screen the cluster had not confirmed.
    } catch (err: unknown) {
      setRefusal(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="os-report-part">
      <ShopperFormsControl site={site} canEdit={canEdit} />

      <h4 className="os-report-heading">Settings</h4>
      <Caption>{NOT_A_SECRET}</Caption>

      {draft.length === 0 ? (
        <Caption>
          Add a public app value, such as an API URL or region, without rebuilding.
        </Caption>
      ) : (
        <>
          {/* The two columns, named once at the top. Every field keeps its own
              visually-hidden label for a screen reader; printing "Name" and
              "Value" beside each of six controls would say the same two words
              three times. */}
          <div className="os-settings-header" aria-hidden>
            <span>Name</span>
            <span>Value</span>
            <span />
          </div>
          <ul className="os-settings-rows">
          {draft.map((row, i) => (
            <li key={row.id} className="os-settings-row">
              <Input
                id={`os-setting-key-${site.id}-${row.id}`}
                label="Setting name"
                placeholder="apiBase"
                value={row.key}
                onChange={(next) => update(i, { key: next })}
              />
              <Input
                id={`os-setting-value-${site.id}-${row.id}`}
                label={`Value for ${row.key === "" ? "this setting" : row.key}`}
                placeholder="https://api.example.com"
                value={row.value}
                onChange={(next) => update(i, { value: next })}
              />
              <Button
                tone="quiet"
                onClick={() => setDraft((rows) => rows.filter((_, j) => j !== i))}
                ariaLabel={row.key === "" ? "Remove this setting" : `Remove ${row.key}`}
              >
                <Trash2 size={12} aria-hidden /> Remove
              </Button>
              {problems[i] === "" ? null : <p className="os-settings-problem">{problems[i]}</p>}
            </li>
          ))}
          </ul>
        </>
      )}

      <div className="os-settings-actions">
        <Button tone="quiet" onClick={() => setDraft((rows) => [...rows, newRow()])}>
          <Plus size={12} aria-hidden /> Add a setting
        </Button>
        <Button
          tone="primary"
          disabled={!dirty || firstProblem !== ""}
          busy={busy}
          busyLabel="Saving"
          onClick={() => void save()}
        >
          Save settings
        </Button>
      </div>

      {/* The server is the law and it says so here, in its own words: the
          caps and the `Ref` rule are checked beside the engine's write path,
          so a refusal that arrives despite this panel having allowed the
          click is the interesting case. */}
      {refusal === "" ? null : (
        <ProblemNotice problem={{ code: "settings_refused", message: refusal, fatal: true }} tone="error" />
      )}
    </section>
  );
}

let rowSeq = 0;

/** A fresh editor row. The id is the LIST KEY and never the setting's name:
 *  keying on the name would remount the field a person is typing a name into,
 *  which loses focus on every keystroke. */
function newRow(): SettingRow {
  rowSeq += 1;
  return { id: `new-${rowSeq}`, key: "", value: "" };
}

export { SETTINGS_KEY_FORM };

// ===========================================================================
// THE PUBLIC FORM ENDPOINT
// ===========================================================================
// A true on/off setting, so it is a switch (SUPERVISED-VISUAL-COMPOSITION:
// "suitable true on/off settings use switches").
//
// THE CONSEQUENCE IS STATED BEFORE THE CLICK, not after it and not in a
// tooltip. Turning this on puts an endpoint on this deployable's own address
// that anybody on the internet may post to -- which is the single most
// material thing on this panel, and the person deciding is owed it while
// they are looking at the control rather than in a refusal or in
// documentation. That is the same rule the settings editor above keeps about
// secrets, applied to the other write on this stop.
//
// WHAT IT DOES NOT DECIDE IS SAID TOO, because the obvious reading of a
// switch called "public form endpoint" is that it opens one. It does not: a
// pack DECLARES which forms and reads exist, in Go, and a cluster that has
// enabled no pack publishes nothing at any setting of this switch. Leaving
// that out would make an operator believe they had opened something they had
// not, which is the worse direction of the two.
//
// IT IS LIVE, unlike the pack switch in Cluster > Modules: the edge reads
// this off the site row it resolves per request, so there is no restart
// sentence to write and none is written. Saying "takes effect at next boot"
// here because a sibling control says it there would be a false promise in
// the reassuring direction.

const SHOPPER_FORMS_CONSEQUENCE =
  "Puts a form endpoint on this deployable's own address that anyone can post to. What can be posted is fixed by the packs this cluster runs -- with none enabled, nothing is published. Submissions are rate limited per visitor and size capped, and each one is recorded against the store this deployable is bound to.";

function ShopperFormsControl({ site, canEdit }: { site: SiteRow; canEdit: boolean }) {
  const connection = useOsConnection();
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState("");

  // NO LOCAL DRAFT. v1:platform:site broadcasts `updated`, so the row comes
  // back on the feed this page already reads -- and a local optimistic flag
  // would show the switch on while the cluster had refused the write.
  const on = site.shopperForms;

  async function flip(next: boolean) {
    const query = connection?.query ?? null;
    if (query === null) {
      setRefusal("Not connected to the cluster, so nothing was written.");
      return;
    }
    setBusy(true);
    setRefusal("");
    try {
      await saveShopperForms(query, site.id, next);
    } catch (err: unknown) {
      setRefusal(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="os-report-part">
      <h4 className="os-report-heading">Public form endpoint</h4>
      <Caption>{SHOPPER_FORMS_CONSEQUENCE}</Caption>
      {canEdit ? (
        <Switch checked={on} disabled={busy} onChange={(next) => void flip(next)}>
          Accept form submissions from visitors
        </Switch>
      ) : (
        // A CONTROL THAT WOULD ONLY REFUSE IS ABSENT, not drawn disabled
        // (DESIGN.md rule 12). The state is still reported, because reading
        // what a deployable does is not the same permission as changing it.
        <Fact
          label="Form submissions"
          value={on ? "Accepted from visitors" : "Not accepted"}
        />
      )}
      {refusal === "" ? null : (
        <ProblemNotice
          problem={{ code: "shopper_forms_refused", message: refusal }}
          tone="error"
        />
      )}
    </div>
  );
}
