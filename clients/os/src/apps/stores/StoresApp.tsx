import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";

import { AppLogsSection } from "../../logs/AppLogsSection";
import { Check, Head, Panel } from "../../kit";
import type { OsAppProps } from "../../system/registry";
import { StoresSection } from "./StoresSection";
import {
  DEFAULT_STORES_SETTINGS,
  LocalStoresSettingsStore,
  STORES_SECTIONS,
  type StoresSettings,
  type StoresSettingsStore,
} from "./settings";

// Stores: the Shopify connector's operator surface (epic memql#5009).
//
// Every store this cluster mirrors, what each one is allowed to see, what it
// has mirrored, and the two acts that are a STORE's rather than a mirrored
// concept's. The per-domain acts -- backfill, per-domain pause, retry,
// discard -- belong to every connector and live in the Cluster app's Data
// origins section; `StorePage.tsx`'s header says why, and says it where
// somebody about to "fix the omission" will read it.
//
// Sections are the app's own navigation. It never opens a window.

/** The concepts this app owns, for its Logs section: a line about a store is
 *  this app's line. The 65 mirrored concepts are NOT this app's -- they
 *  belong to the connector runtime, whose surface is Data origins. */
const STORES_LOG_CONCEPTS = [Concepts.SHOPIFY_STORE] as const;

export function StoresApp({
  sectionId,
  navigate,
  askContext,
  intent,
  consumeIntent,
  store,
}: OsAppProps & { store?: StoresSettingsStore }) {
  // The store is injectable for tests, which is the whole reason the
  // parameter exists -- nothing in the shell passes one.
  const settingsStore = useMemo(() => store ?? new LocalStoresSettingsStore(), [store]);
  const [settings, setSettings] = useState<StoresSettings>(() => settingsStore.load());

  const update = useCallback(
    (patch: Partial<StoresSettings>) => {
      setSettings((held) => {
        const next = { ...held, ...patch, version: 1 as const };
        settingsStore.save(next);
        return next;
      });
    },
    [settingsStore],
  );

  // THE DEFAULT-SECTION PREFERENCE, APPLIED ONCE PER WINDOW.
  //
  // The shell opens an app on its manifest's FIRST section: a window carries
  // no section until something navigates it, and WindowFrame resolves that
  // to sections[0]. So an app-level "open me here" preference can only be
  // the app navigating itself, immediately, on the first render of this
  // component instance. Once per WINDOW is exactly right, and the ref is
  // what makes it so: this component stays mounted while the section changes
  // (only its props do), so the guard fires on open and never again.
  const applied = useRef(false);
  useEffect(() => {
    if (applied.current) return;
    applied.current = true;
    // ONLY when the window opened on the SHELL's default. A window opened on
    // a named section -- the Settings apps index deep-linking to this app's
    // own settings, say -- was opened by somebody who said where they wanted
    // to be, and a preference that overrode that would make the deep link
    // silently not work (memql#4743).
    const shellDefault = STORES_SECTIONS[0]?.id ?? "";
    if (sectionId !== shellDefault) return;
    if (settings.defaultSection && settings.defaultSection !== sectionId) {
      navigate(settings.defaultSection);
    }
    // AN EMPTY DEP LIST IS THE POINT: this runs once per mount, which is once
    // per window. Re-running it on a section change would drag an operator
    // back to their default the moment they navigated away.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  if (sectionId === "settings") {
    return <StoresSettingsSection settings={settings} update={update} />;
  }
  if (sectionId === "logs") {
    return (
      <AppLogsSection
        app="stores"
        subjectConcepts={STORES_LOG_CONCEPTS}
        intent={intent}
        consumeIntent={consumeIntent}
      />
    );
  }
  return (
    <StoresSection
      hideQuietDomains={settings.hideQuietDomains}
      onOpenSettings={() => navigate("settings")}
      onAsk={askContext}
    />
  );
}

function StoresSettingsSection({
  settings,
  update,
}: {
  settings: StoresSettings;
  update: (patch: Partial<StoresSettings>) => void;
}) {
  return (
    <div className="os-settings">
      <Head title="Stores settings" />
      <Panel label="Stores settings">
        <fieldset className="os-field-group">
          <legend>Open Stores on</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Default section">
            {STORES_SECTIONS.map((section) => (
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
            Applies the next time a Stores window opens. It does not move the window you are looking
            at.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Quiet domains</legend>
          <Check
            checked={settings.hideQuietDomains}
            onChange={(next) => update({ hideQuietDomains: next })}
          >
            Hide domains with nothing to say
          </Check>
          <p className="os-caption">
            Off by default, so a store&rsquo;s table is the complete diagnostic it claims to be. A
            quiet domain is idle, with no drift, no stale writes and nothing tombstoned -- a domain
            whose counters have never been REPORTED is not quiet and is always shown, because that
            is the gap somebody looking for one needs to see.
          </p>
        </fieldset>

        <p className="os-caption">
          These are kept in this browser. A different browser starts from the defaults ({" "}
          {DEFAULT_STORES_SETTINGS.defaultSection}, every domain listed).
        </p>
      </Panel>
    </div>
  );
}
