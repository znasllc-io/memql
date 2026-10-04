import { useId, useState } from "react";
import { Monitor, Moon, Sun } from "lucide-react";
import { readStoredTheme, setTheme, type ThemeChoice } from "../app/theme";

const choices = [
  { value: "dark", label: "Dark", icon: Moon },
  { value: "light", label: "Light", icon: Sun },
  { value: "system", label: "System", icon: Monitor },
] as const;

export function SetupTheme() {
  const name = useId();
  const [mode, setMode] = useState<ThemeChoice>(readStoredTheme);

  return <fieldset className="os-setup-theme" data-mode={mode}>
    <legend className="os-sr-only">Color theme</legend>
    {choices.map(({ value, label, icon: Icon }) => <label className="os-setup-theme-option" key={value} title={label}>
      <input className="os-sr-only" type="radio" name={name} value={value} aria-label={label} checked={mode === value}
        onChange={() => { setMode(value); setTheme(value); }} />
      <Icon size={14} strokeWidth={1.75} aria-hidden="true" />
    </label>)}
  </fieldset>;
}
