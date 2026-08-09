import { useState, type ReactNode } from "react";

import { readStoredTheme, setTheme, type ThemeChoice } from "../app/theme";

// The light / dark / system control.
//
// Three states, not two. A binary toggle cannot express "follow the OS",
// which is the correct default and the one most operators never change --
// collapsing it loses the ability to go back.

const CHOICES: readonly ThemeChoice[] = ["light", "dark", "system"];

const LABELS: Record<ThemeChoice, string> = {
  light: "Light",
  dark: "Dark",
  system: "System",
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
      className="flex overflow-hidden rounded border border-line"
    >
      {CHOICES.map((option) => (
        <button
          key={option}
          type="button"
          aria-pressed={choice === option}
          onClick={() => {
            setTheme(option);
            setChoice(option);
          }}
          className={
            "px-2 py-0.5 text-xs " +
            (choice === option
              ? "bg-accent text-accent-fg"
              : "bg-surface text-muted hover:bg-raised")
          }
        >
          {LABELS[option]}
        </button>
      ))}
    </div>
  );
}
