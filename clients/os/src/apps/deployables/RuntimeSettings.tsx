import { useEffect, useMemo, useState } from "react";
import { Plus, Trash2 } from "lucide-react";

import { Button, Caption, Fact, Facts, Input } from "../../kit";
import { useOsConnection } from "../../live/connection";
import { ProblemNotice } from "./packages/ReportView";
import { saveSiteSettings } from "./packages/calls";
import type { SiteRow } from "./rows";
import {
  SETTINGS_KEY_FORM,
  settingsFingerprint,
  settingsKeyProblem,
  settingsRows,
  toSettingsMap,
  type SettingRow,
} from "./settings-editor";

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
  "Read by the app when it loads, and served to everyone who visits it. Not a place for a secret -- put credentials in the cluster's secrets and name them from the deployable's binding.";

export function RuntimeSettingsPanel({ site, canWrite }: { site: SiteRow; canWrite: boolean }) {
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

  if (!canWrite) {
    return (
      <section className="os-report-part">
        <h4 className="os-report-heading">Settings</h4>
        <Caption>{NOT_A_SECRET}</Caption>
        {stored.length === 0 ? (
          <Caption>No settings. This app reads nothing from the cluster at load.</Caption>
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
      <h4 className="os-report-heading">Settings</h4>
      <Caption>{NOT_A_SECRET}</Caption>

      {draft.length === 0 ? (
        <Caption>
          No settings yet. Add one to give this app a value it reads at load -- an API base, a region, a feature switch --
          without rebuilding it.
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
