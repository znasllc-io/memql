import { useId, useState } from "react";
import { readStoredTheme, setTheme, type ThemeChoice } from "../app/theme";

export function SetupTheme() {
  const name = useId();
  const [mode, setMode] = useState<ThemeChoice>(readStoredTheme);

  return <fieldset className="os-field-group os-setup-theme">
    <legend>Theme</legend>
    <div className="os-choice-row">
      {(["dark", "light", "system"] as const).map(choice => <label className="os-choice" key={choice}>
        <input className="os-sr-only" type="radio" name={name} value={choice} checked={mode === choice}
          onChange={() => { setMode(choice); setTheme(choice); }} />
        {choice === "dark" ? "Dark" : choice === "light" ? "Light" : "System"}
      </label>)}
    </div>
  </fieldset>;
}
