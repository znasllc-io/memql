import { useState } from "react";
import { ArrowLeft, LayoutGrid, Sparkles } from "lucide-react";

import { useAsk } from "../ask/AskProvider";
import { appsForRole, canOpen, sectionsForRole } from "../system/registry";
import { Mark } from "./Mark";
import { useOs } from "./state";

// Phone chrome (spec D13): no desks, no windows, no pins. The Launcher
// grid is home; an app opens full screen, one at a time, with its section
// nav as a top strip; Ask is a sheet; sign out lives in the top bar.

export function PhoneShell({ onSignOut }: { onSignOut: () => void }) {
  const { registry, actorRole } = useOs();
  const { openAsk } = useAsk();
  const [currentAppId, setCurrentAppId] = useState<string | null>(null);
  const [sectionId, setSectionId] = useState("");

  const apps = appsForRole(registry, actorRole);
  const current = currentAppId ? apps.find((a) => a.id === currentAppId) ?? null : null;
  const sections = current ? sectionsForRole(current, actorRole) : [];
  const activeSection = sections.find((s) => s.id === sectionId) ?? sections[0];

  function open(appId: string) {
    if (!canOpen(registry, actorRole, appId)) return;
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
        <span className="os-phone-title">{current ? current.name : "MemQL OS"}</span>
        <button type="button" className="os-link" onClick={onSignOut}>
          Sign out
        </button>
      </header>
      {current ? (
        <main className="os-phone-app">
          {sections.length > 1 ? (
            <nav className="os-phone-sections" aria-label={`${current.name} sections`}>
              {sections.map((section) => (
                <button
                  key={section.id}
                  type="button"
                  className="os-phone-section"
                  aria-current={section.id === activeSection?.id ? "page" : undefined}
                  onClick={() => setSectionId(section.id)}
                >
                  {section.name}
                </button>
              ))}
            </nav>
          ) : null}
          <current.component
            sectionId={activeSection?.id ?? ""}
            navigate={setSectionId}
            askContext={(tag) => openAsk(tag)}
          />
        </main>
      ) : (
        <main className="os-phone-home">
          {apps.map((app) => {
            const Icon = app.icon;
            return (
              <button key={app.id} type="button" className="os-tile" onClick={() => open(app.id)}>
                <Icon size={26} aria-hidden />
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
          <Sparkles size={18} aria-hidden />
        </button>
      </footer>
    </div>
  );
}
