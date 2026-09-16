import { AttentionMarker, AttentionDestination } from "../attention/Attention";
import { useState } from "react";
import { ArrowLeft, LayoutGrid } from "lucide-react";

import { useAsk } from "../ask/AskProvider";
import { ProvenanceDot } from "../kit";
import {
  canConfigure,
  gateFor,
  markToneFor,
  SurfaceUnconfigured,
} from "../kit/ReadinessStates";
import { MODULE_DESCRIPTIONS } from "../system/modules";
import {
  allRequirementsFor,
  appsFor,
  canOpen,
  requirementsFor,
  sectionsFor,
} from "../system/registry";
import { useSession } from "./access";
import { Mark } from "./Mark";
import { useOs } from "./state";
import { WindowErrorBoundary } from "./WindowErrorBoundary";

// Phone chrome (spec D13): no desks, no windows, no pins. The Launcher
// grid is home; an app opens full screen, one at a time, with its section
// nav as a top strip; Ask is a sheet; sign out lives in the top bar.

export function PhoneShell({ onSignOut }: { onSignOut: () => void }) {
  const { registry, actorRole } = useOs();
  const { openAsk } = useAsk();
  const [currentAppId, setCurrentAppId] = useState<string | null>(null);
  const [sectionId, setSectionId] = useState("");

  const apps = appsFor(registry);
  const current = currentAppId ? apps.find((a) => a.id === currentAppId) ?? null : null;
  const sections = current ? sectionsFor(current) : [];
  const activeSection = sections.find((s) => s.id === sectionId) ?? sections[0];

  // The same two gates the window frame computes, for the same reasons -- see
  // WindowFrame.tsx. The phone has no gear, so the mark rides on the Settings
  // entry of the section strip and nowhere else.
  const { readiness } = useSession();
  const sectionReqs = current
    ? requirementsFor(current, activeSection?.id ?? "")
    : { requires: [], wants: [] };
  const gate = gateFor(readiness, sectionReqs.requires, sectionReqs.wants);
  const appReqs = current ? allRequirementsFor(current) : { requires: [], wants: [] };
  const appGate = gateFor(readiness, appReqs.requires, appReqs.wants);
  const settingsTone = current ? markToneFor(appGate) : null;
  // The dot inside a button is DECORATIVE and the button says the state
  // itself: a labelled role="img" nested in a button appends to the button's
  // accessible name, so "Settings" would announce as "Settings Campaigns is
  // not set up" -- the right information in a shape nothing can match on.
  const settingsStatePhrase = settingsTone === "needsSetup" ? "not set up" : "partly set up";
  const unmetIsSectionOnly = gate.unmet.some((id) => !(current?.needs ?? []).includes(id));

  function open(appId: string) {
    if (!canOpen(registry, appId)) return;
    setCurrentAppId(appId);
    setSectionId("");
  }

  return (
    <div className="os-phone" data-os-phone>
      <header className="os-phone-bar">
        {current ? (
          <button
            type="button"
            className="os-icon-button"
            aria-label="Back to home"
            onClick={() => setCurrentAppId(null)}
          >
            <ArrowLeft size={16} aria-hidden />
          </button>
        ) : (
          <Mark className="os-phone-mark" />
        )}
        <span className="os-phone-title">{current ? current.name : "MemQL OS"}{current ? <AttentionMarker appId={current.id} /> : null}</span>
        <button type="button" className="os-link" onClick={onSignOut}>
          Sign out
        </button>
      </header>
      {current ? (
        <main className="os-phone-app">
          {sections.length > 1 ? (
            <nav className="os-phone-sections" aria-label={`${current.name} sections`}>
              {sections.filter((section) => !section.parent).map((section) => (
                <button
                  key={section.id}
                  type="button"
                  className="os-phone-section"
                  data-os-setup={
                    settingsTone && section.id === current.settingsSection ? "" : undefined
                  }
                  aria-label={
                    settingsTone && section.id === current.settingsSection
                      ? `${section.name}, ${current.name} is ${settingsStatePhrase}`
                      : undefined
                  }
                  aria-current={section.id === (activeSection?.parent ?? activeSection?.id) ? "page" : undefined}
                  onClick={() => setSectionId(section.id)}
                >
                  {section.name}<AttentionMarker appId={current.id} sectionId={section.id} />
                  {settingsTone && section.id === current.settingsSection ? (
                    <ProvenanceDot tone={settingsTone} />
                  ) : null}
                </button>
              ))}
            </nav>
          ) : null}
          {gate.state === "unconfigured" ? (
            <SurfaceUnconfigured
              surface={unmetIsSectionOnly ? (activeSection?.name ?? current.name) : current.name}
              unmet={gate.unmet}
              descriptions={MODULE_DESCRIPTIONS}
              canSetUp={canConfigure(actorRole)}
              onSetUp={() => setSectionId(current.settingsSection)}
            />
          ) : (
            <WindowErrorBoundary key={current.id} app={current.id} section={activeSection?.id ?? ""}>
              <AttentionDestination appId={current.id} sectionId={activeSection?.id ?? ""}><current.component
                sectionId={activeSection?.id ?? ""}
                navigate={setSectionId}
                askContext={(tag) => openAsk(tag)}
              /></AttentionDestination>
            </WindowErrorBoundary>
          )}
        </main>
      ) : (
        <main className="os-phone-home">
          {apps.map((app) => {
            const Icon = app.icon;
            return (
              <button key={app.id} type="button" className="os-tile" onClick={() => open(app.id)}>
                <span className="os-attention-anchor"><Icon size={26} aria-hidden /><AttentionMarker appId={app.id} /></span>
                <span>{app.name}</span>
              </button>
            );
          })}
        </main>
      )}
      <footer className="os-phone-tabbar">
        <button
          type="button"
          className="os-icon-button"
          aria-label="Home"
          aria-current={!current || undefined}
          onClick={() => setCurrentAppId(null)}
        >
          <LayoutGrid size={20} aria-hidden />
        </button>
        <button type="button" className="os-ask-orb" aria-label="Ask" onClick={() => openAsk(null)}>
          <Mark className="os-ask-mark" />
        </button>
      </footer>
    </div>
  );
}
