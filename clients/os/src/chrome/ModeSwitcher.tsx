import { useState } from "react";

import { readStoredTheme, setTheme, type ThemeChoice } from "../app/theme";

const CHOICES: { value: ThemeChoice; label: string }[] = [
  { value: "light", label: "Light" },
  { value: "dark", label: "Dark" },
  { value: "system", label: "System" },
];

export function ModeSwitcher() {
  const [choice, setChoice] = useState<ThemeChoice>(() => readStoredTheme());

  return (
    <div className="os-mode" data-mode-switcher role="group" aria-label="Appearance">
      {CHOICES.map((item) => (
        <button
          key={item.value}
          type="button"
          data-mode={item.value}
          aria-pressed={choice === item.value}
          className={choice === item.value ? "is-active" : undefined}
          onClick={() => {
            setTheme(item.value);
            setChoice(item.value);
          }}
        >
          {item.label}
        </button>
      ))}
    </div>
  );
}
