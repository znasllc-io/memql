import { useEffect, useRef, useState } from "react";
import { Check, Trash2, X } from "lucide-react";

import { useOs } from "../chrome/state";
import { Button, Notice } from "../kit";
import { isBuiltInId, themePacks, validateThemePack, type OsThemePack, type OsThemeTokens } from "./registry";

// The theme marketplace (epic memql#4745).
//
// ===========================================================================
// IT IS A DRAWER, NOT A WINDOW AND NOT A LAUNCHER TAB
// ===========================================================================
// The whole gesture is APPLY-ON-HOVER TO THE REAL DESKTOP: point at a theme
// and the desk, the dock, the wallpaper and every open window restyle behind
// this panel; leave and they snap back. That is the most convincing way to
// sell a theme, and it decides the surface's shape rather than following from
// it. A window would occupy a desk slot and cover half of what it is
// previewing; the Launcher is a full-screen glass overlay and would hide all
// of it. So the Launcher's Themes tile CLOSES the Launcher and opens this.
//
// Ask is the precedent, not an exception being stretched: a surface whose
// subject is the desktop itself cannot be a window on the desktop. Themes is
// the second inhabitant of that rule.
//
// The preview never persists -- it is `previewPack` in session state, absent
// from `documentFromState`, so nothing a pointer does can roam.

export function ThemeStore({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { state, actions } = useOs();
  const [refusal, setRefusal] = useState<string | null>(null);
  const fileRef = useRef<HTMLInputElement | null>(null);
  const panelRef = useRef<HTMLDivElement | null>(null);
  const [dropping, setDropping] = useState(false);

  useEffect(() => {
    if (!open) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  // Closing must end any preview. Otherwise the desktop keeps wearing the
  // last card the pointer crossed on its way out, which reads as a theme that
  // applied itself.
  useEffect(() => {
    if (!open) actions.previewThemePack(null);
  }, [open, actions]);

  useEffect(() => {
    return () => actions.previewThemePack(null);
  }, [actions]);

  if (!open) return null;

  const packs = themePacks(state.installedPacks);

  async function install(file: File | undefined) {
    if (!file) return;
    setRefusal(null);
    let parsed: unknown;
    try {
      parsed = JSON.parse(await file.text());
    } catch {
      setRefusal(`${file.name} is not readable as JSON.`);
      return;
    }
    const load = validateThemePack(parsed);
    if (!load.ok) {
      // The refusal NAMES what is wrong -- which colours, which mode. "Invalid
      // theme" would leave the author of the pack with nowhere to start, and
      // the person who was handed it with nothing to pass back.
      setRefusal(load.detail);
      return;
    }
    if (isBuiltInId(load.pack.id)) {
      setRefusal(`"${load.pack.id}" is a built-in theme. A pack cannot replace one.`);
      return;
    }
    actions.installThemePack(load.pack);
    actions.setThemePack(load.pack.id);
  }

  return (
    <div
      className="os-themes-backdrop"
      onPointerDown={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <div
        className="os-themes"
        ref={panelRef}
        role="dialog"
        aria-label="Themes"
        data-os-sheet
        // The desk plate underneath turns a dropped file into a Library
        // artifact and a desk icon. Without stopPropagation, dropping a theme
        // here installs it AND uploads it -- two outcomes, one of which
        // nobody asked for. Stop it on dragover too, or the plate's own
        // handler allows the drop before this one ever sees it.
        onDragOver={(event) => {
          event.preventDefault();
          event.stopPropagation();
          setDropping(true);
        }}
        onDragLeave={() => setDropping(false)}
        onDrop={(event) => {
          event.preventDefault();
          event.stopPropagation();
          setDropping(false);
          void install(event.dataTransfer?.files?.[0]);
        }}
        data-dropping={dropping || undefined}
      >
        <header className="os-themes-head">
          <h2>Themes</h2>
          <button type="button" className="os-icon-button" aria-label="Close themes" onClick={onClose}>
            <X size={15} aria-hidden />
          </button>
        </header>
        <p className="os-caption os-themes-hint">
          Point at a theme to wear it. Nothing is saved until you pick one.
        </p>

        <div className="os-themes-list">
          {packs.map((pack) => (
            <ThemeCard
              key={pack.id}
              pack={pack}
              current={state.themePack === pack.id}
              onPreview={() => actions.previewThemePack(pack.id)}
              onEndPreview={() => actions.previewThemePack(null)}
              onApply={() => actions.setThemePack(pack.id)}
              onRemove={
                pack.builtIn ? null : () => actions.removeThemePack(pack.id)
              }
            />
          ))}
        </div>

        <footer className="os-themes-foot">
          <input
            ref={fileRef}
            type="file"
            accept="application/json,.json"
            hidden
            onChange={(event) => {
              void install(event.target.files?.[0]);
              event.target.value = "";
            }}
          />
          <Button tone="quiet" onClick={() => fileRef.current?.click()}>
            Add a theme
          </Button>
          <p className="os-caption">
            A theme is one JSON file. Drop it here, or choose it.
          </p>
          {refusal ? (
            <Notice
              tone="warn"
              sentence="That theme was not installed."
              detail={refusal}
            />
          ) : null}
        </footer>
      </div>
    </div>
  );
}

function ThemeCard({
  pack,
  current,
  onPreview,
  onEndPreview,
  onApply,
  onRemove,
}: {
  pack: OsThemePack;
  current: boolean;
  onPreview: () => void;
  onEndPreview: () => void;
  onApply: () => void;
  onRemove: (() => void) | null;
}) {
  return (
    <div className="os-theme-card" data-current={current || undefined}>
      <button
        type="button"
        className="os-theme-choose"
        aria-label={`Use ${pack.name}`}
        aria-pressed={current}
        // FOCUS PREVIEWS TOO, and that is not a nicety. The preview is the
        // whole product here, and a keyboard reader who could only preview by
        // choosing would be shopping by trying things on permanently.
        onPointerEnter={onPreview}
        onPointerLeave={onEndPreview}
        onFocus={onPreview}
        onBlur={onEndPreview}
        onClick={onApply}
      >
        <span className="os-theme-minis">
          <ThemeMiniature tokens={pack.tokens.dark} label="Dark" />
          <ThemeMiniature tokens={pack.tokens.light} label="Light" />
        </span>
        <span className="os-theme-name">
          {pack.name}
          {current ? (
            <span className="os-theme-current">
              <Check size={12} aria-hidden /> Wearing
            </span>
          ) : null}
        </span>
        <span className="os-caption os-theme-by">
          {pack.author} &middot; {pack.version}
          {pack.builtIn ? " · built in" : ""}
        </span>
        {pack.description ? <span className="os-caption">{pack.description}</span> : null}
      </button>
      {onRemove ? (
        <button
          type="button"
          className="os-icon-button os-theme-remove"
          aria-label={`Remove ${pack.name}`}
          onClick={onRemove}
        >
          <Trash2 size={14} aria-hidden />
        </button>
      ) : null}
    </div>
  );
}

/**
 * A pack's own desktop, at 76 by 48.
 *
 * DRAWN FROM THE PACK'S VALUES, never from a screenshot -- so it cannot go
 * stale, and it is the only honest way to show a theme's OTHER mode: a swatch
 * strip would show six colours, and this shows what they DO. The pair of them
 * side by side is the epic's "per-pack light and dark verification" made
 * visible, and the numeric half of that verification is
 * test/themes/contrast.test.ts.
 *
 * Inline styles rather than CSS variables on purpose: a card renders BOTH
 * modes at once, and a variable can only carry the one the document is in.
 */
function ThemeMiniature({ tokens, label }: { tokens: OsThemeTokens; label: string }) {
  return (
    <span
      className="os-theme-mini"
      role="img"
      aria-label={`${label} mode`}
      style={{ background: tokens.ground, borderColor: tokens.line }}
    >
      <span className="os-theme-mini-field" style={{ background: tokens["field-dot"] }} />
      <span
        className="os-theme-mini-window"
        style={{ background: tokens.plate, borderColor: tokens.line }}
      >
        <span className="os-theme-mini-bar" style={{ background: tokens.accent }} />
        <span className="os-theme-mini-line" style={{ background: tokens.muted }} />
        <span className="os-theme-mini-line" style={{ background: tokens.line }} />
      </span>
      <span
        className="os-theme-mini-dock"
        style={{ background: tokens["glass-solid"], borderColor: tokens.line }}
      >
        <span className="os-theme-mini-dot" style={{ background: tokens.accent }} />
        <span className="os-theme-mini-dot" style={{ background: tokens.muted }} />
      </span>
    </span>
  );
}
