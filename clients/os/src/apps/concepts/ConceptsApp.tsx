import { useEffect, useMemo, useRef, useState } from "react";
import { AppLogsSection } from "../../logs/AppLogsSection";
import type { OsAppProps } from "../../system/registry";
import { Head, Panel, Select } from "../../kit";
import { RegistrySection } from "./RegistrySection";
import {
  CONCEPTS_PAGE_SIZES,
  CONCEPTS_SECTIONS,
  DEFAULT_CONCEPTS_SETTINGS,
  LocalConceptsSettingsStore,
  type ConceptsSettings,
  type ConceptsSettingsStore,
} from "./settings";

// Concepts: every kind of thing this cluster knows, what each one declares,
// and the rows it holds (epic memql#5009, issue memql#5010).
//
// It is the concept-AGNOSTIC surface -- the one place a person reaches a
// concept nobody built a screen for. Everything it renders comes from the
// registry the engine publishes and from `@displayCard`, so a concept added
// by a package this build has never heard of reads exactly as well as one
// the OS ships an app for.
//
// Sections are the app's own navigation. It never opens a window.

// THIS APP OWNS NO CONCEPT, and its Logs section says so by omission.
// `subjectConcepts` defaults to empty, so the slice is the lines tagged
// `app:concepts` and nothing else -- which is right: this app is a VIEWER
// over every concept, so naming any of them as "its own" would pull one
// arbitrary domain's lines into a section about this window.

export function ConceptsApp({
  sectionId,
  navigate,
  intent,
  consumeIntent,
  store,
}: OsAppProps & { store?: ConceptsSettingsStore }) {
  // Injectable for tests, which is the whole reason the parameter exists --
  // nothing in the shell passes one.
  const settingsStore = useMemo(() => store ?? new LocalConceptsSettingsStore(), [store]);
  const [settings, setSettings] = useState<ConceptsSettings>(() => settingsStore.load());

  function update(patch: Partial<ConceptsSettings>) {
    const next = { ...settings, ...patch, version: 1 as const };
    setSettings(next);
    settingsStore.save(next);
  }

  // A concept named in the address bar, delivered as the window's open
  // intent. Consumed by id so acting on a stale render can never eat a
  // newer instruction.
  const openConceptId =
    typeof intent?.payload["conceptId"] === "string" ? intent.payload["conceptId"] : "";
  useEffect(() => {
    if (intent && openConceptId !== "") consumeIntent?.(intent.id);
  }, [intent, openConceptId, consumeIntent]);

  // The default-section preference, applied once per WINDOW. The ref is what
  // makes it once: this component stays mounted while the section changes,
  // so an operator who navigates away is not dragged back.
  const applied = useRef(false);
  useEffect(() => {
    if (applied.current) return;
    applied.current = true;
    // Only when the window opened on the SHELL's default. A window opened
    // on a named section was opened by somebody who said where they wanted
    // to be.
    const shellDefault = CONCEPTS_SECTIONS[0]?.id ?? "";
    if (sectionId !== shellDefault) return;
    if (settings.defaultSection && settings.defaultSection !== sectionId) {
      navigate(settings.defaultSection);
    }
    // Once per mount is once per window.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  if (sectionId === "settings") {
    return <ConceptsSettingsSection settings={settings} update={update} />;
  }
  if (sectionId === "logs") {
    return (
      <AppLogsSection app="concepts" intent={intent} consumeIntent={consumeIntent} />
    );
  }
  return <RegistrySection settings={settings} openConceptId={openConceptId} />;
}

function ConceptsSettingsSection({
  settings,
  update,
}: {
  settings: ConceptsSettings;
  update: (patch: Partial<ConceptsSettings>) => void;
}) {
  return (
    <div className="os-settings">
      <Head title="Concepts settings" />
      <Panel label="Concepts settings">
        <fieldset className="os-field-group">
          <legend>Open Concepts on</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Default section">
            {CONCEPTS_SECTIONS.map((section) => (
              <button
                key={section.id}
                type="button"
                role="radio"
                aria-checked={settings.defaultSection === section.id}
                className="os-choice"
                onClick={() => update({ defaultSection: section.id })}
              >
                {section.name}
              </button>
            ))}
          </div>
          <p className="os-caption">
            Applies the next time a Concepts window opens. It does not move the window you are
            looking at.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Rows per page</legend>
          <Select
            id="concepts-page-size"
            label="Rows per page"
            value={String(settings.pageSize)}
            onChange={(next) => update({ pageSize: Number(next) })}
          >
            {CONCEPTS_PAGE_SIZES.map((size) => (
              <option key={size} value={String(size)}>
                {size}
              </option>
            ))}
          </Select>
          <p className="os-caption">
            How many rows one page of a concept's row window asks the cluster for. Changing it
            starts the walk again from the first page.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Undeclared fields</legend>
          <label className="os-check">
            <input
              type="checkbox"
              checked={settings.showUndeclaredFields}
              onChange={(e) => update({ showUndeclaredFields: e.target.checked })}
            />
            <span>List keys rows carry that the concept does not declare</span>
          </label>
          <p className="os-caption">
            On by default. A key nothing declares is a finding this app can produce and nothing
            else can -- it is invisible in the DSL file and invisible to every shaped read,
            because a shape can only project declared fields.
          </p>
        </fieldset>

        <p className="os-caption">
          These are kept in this browser. A different browser starts from the defaults (
          {DEFAULT_CONCEPTS_SETTINGS.defaultSection}, {DEFAULT_CONCEPTS_SETTINGS.pageSize} rows a
          page).
        </p>
      </Panel>
    </div>
  );
}
