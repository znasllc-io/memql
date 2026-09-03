import { useEffect, useMemo, useRef, useState } from "react";

import { useSession } from "../../chrome/access";
import { subjectIntentOf } from "../../logs/filters";
import type { OsAppProps } from "../../system/registry";
import { SearchSection } from "./SearchSection";
import {
  LOGS_SECTIONS,
  LocalLogsSettingsStore,
  type LogsSettings,
  type LogsSettingsStore,
} from "./settings";
import { LogsSettingsSection } from "./SettingsSection";
import { StreamSection } from "./StreamSection";

// Logs: one app over everything every node wrote (epic memql#4895, spec H
// "The Logs app"). Stream is the store following; Search is the store asked
// about a window; Settings is the app's own, plus what this cluster keeps and
// the archived days.
//
// The manifest carries `roles: { min: "admin" }` and no section carries a
// floor of its own: every read on the log store is admin-and-above in the
// ENGINE (spec L3, enforced in the Go handler), so the floor here is
// presentation over that, and `logsSection` names the Stream.
//
// Sections are the app's own navigation. It never opens a window.

export function LogsApp({
  sectionId,
  navigate,
  intent,
  consumeIntent,
  store,
}: OsAppProps & { store?: LogsSettingsStore }) {
  // Injectable for tests, which is the whole reason the parameter exists --
  // nothing in the shell passes one.
  const settingsStore = useMemo(() => store ?? new LocalLogsSettingsStore(), [store]);
  const [settings, setSettings] = useState<LogsSettings>(() => settingsStore.load());
  const { access } = useSession();
  const actorRole = access?.clusterRole ?? "";

  function update(patch: Partial<LogsSettings>): void {
    const next = { ...settings, ...patch, version: 1 as const };
    setSettings(next);
    settingsStore.save(next);
  }

  // THE DEFAULT-SECTION PREFERENCE, APPLIED ONCE PER WINDOW -- Fleet's
  // pattern, and its reasoning holds unchanged: the shell opens an app on its
  // manifest's FIRST section, so "open me here" can only be the app
  // navigating itself on the first render of this component instance.
  const applied = useRef(false);
  useEffect(() => {
    if (applied.current) return;
    applied.current = true;
    // ONLY when the window opened on the SHELL's default. A window opened on
    // a named section -- a Deployables "Logs" action landing on Search with a
    // subject, the Settings apps index deep-linking here -- was opened by
    // somebody who said where they wanted to be, and a preference that
    // overrode that would make the deep link silently not work.
    const shellDefault = LOGS_SECTIONS[0]?.id ?? "";
    if (sectionId !== shellDefault) return;
    if (settings.defaultSection && settings.defaultSection !== sectionId) {
      navigate(settings.defaultSection);
    }
    // ONCE PER MOUNT, WHICH IS ONCE PER WINDOW.
  }, []);

  // THE INTENT -> SEARCH HANDOFF. A `{ subject, subjectConcept }` intent is a
  // question about a window, so it lands on Search whichever section the
  // window is on; Search consumes it. `openApp` already adopts the section
  // it was handed, so this covers a sender that named none.
  useEffect(() => {
    if (!intent || sectionId === "search") return;
    if (subjectIntentOf(intent.payload) !== null) navigate("search");
  }, [intent, sectionId, navigate]);

  if (sectionId === "settings") {
    return <LogsSettingsSection settings={settings} update={update} actorRole={actorRole} />;
  }
  if (sectionId === "search") {
    return <SearchSection settings={settings} intent={intent} consumeIntent={consumeIntent} />;
  }
  return <StreamSection settings={settings} />;
}
