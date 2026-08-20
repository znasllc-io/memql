import { useState, type ReactNode } from "react";

import { readStoredTheme, setTheme, type ThemeChoice } from "../app/theme";
import { Monitor, Moon, Sun } from "../ui/icons";

// The light / dark / system control.
//
// Three states, not two. A binary toggle cannot express "follow the OS",
// which is the correct default and the one most operators never change --
// collapsing it loses the ability to go back. Icons carry the three states
// in the topbar's quiet control language; the accessible names stay words.

const CHOICES: readonly ThemeChoice[] = ["light", "dark", "system"];

const LABELS: Record<ThemeChoice, string> = {
  light: "Light",
  dark: "Dark",
  system: "System",
};

const ICONS: Record<ThemeChoice, typeof Sun> = {
  light: Sun,
  dark: Moon,
  system: Monitor,
};

export function ThemeToggle(): ReactNode {
  // Seeded from storage rather than defaulted: main.tsx already applied the
  // stored choice to the document before React mounted, so defaulting here
  // would render a control that disagrees with the palette on screen.
  const [choice, setChoice] = useState<ThemeChoice>(() => readStoredTheme());

  return (
    <div
      role="group"
      aria-label="Colour theme"
      className="flex h-7 items-center overflow-hidden rounded border border-line"
    >
      {CHOICES.map((option) => {
        const Icon = ICONS[option];
        return (
          <button
            key={option}
            type="button"
            aria-pressed={choice === option}
            aria-label={LABELS[option]}
            title={LABELS[option]}
            onClick={() => {
              setTheme(option);
              setChoice(option);
            }}
            className={
              "flex h-full items-center px-2 " +
              (choice === option
                ? "bg-accent text-accent-fg"
                : "bg-surface text-muted hover:bg-raised hover:text-fg")
            }
          >
            <Icon size={13} aria-hidden="true" />
          </button>
        );
      })}
    </div>
  );
}
