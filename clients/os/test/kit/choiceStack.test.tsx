import { readFileSync } from "node:fs";
import { join } from "node:path";

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";

import { ChoiceStack } from "../../src/kit";

afterEach(cleanup);

// An option that cannot be chosen RIGHT NOW (the Ask route picker's unready
// sources) stays visible and reachable: its line says why, and a keyboard or
// screen reader can get to that line. So it is aria-disabled, never disabled,
// and choosing it does nothing.
it("keeps an unavailable option visible and focusable, says why, and refuses the choice", () => {
  const onChange = vi.fn();
  render(
    <ChoiceStack
      name="where"
      label="Where"
      voice="prose"
      value="auto"
      onChange={onChange}
      options={[
        { value: "auto", label: "Auto", description: "Follows your rules" },
        { value: "codex", label: "Codex", description: "Not installed on any machine", unavailable: true },
      ]}
    />,
  );
  const codex = screen.getByRole("radio", { name: /Codex/ }) as HTMLButtonElement;
  expect(codex.getAttribute("aria-disabled")).toBe("true");
  expect(codex.disabled).toBe(false);
  expect(codex.textContent).toContain("Not installed on any machine");
  fireEvent.click(codex);
  expect(onChange).not.toHaveBeenCalled();

  const auto = screen.getByRole("radio", { name: /Auto/ });
  expect(auto.getAttribute("aria-disabled")).toBeNull();
  fireEvent.click(auto);
  expect(onChange).toHaveBeenCalledWith("auto");
});

// The current choice keeps its mark when it becomes unavailable (a pinned
// source whose machine went offline): the unavailable rule strips the card's
// edge, so the checked-and-unavailable rule must come AFTER it and put the
// accent edge back. Otherwise only the name's colour says which row is chosen.
it("keeps the checked edge on an unavailable option", () => {
  const css = readFileSync(join(__dirname, "..", "..", "src", "styles", "index.css"), "utf8");
  const quiet = css.indexOf('.os-choice-card[aria-disabled="true"],\n.os-choice-card[aria-disabled="true"]:hover {');
  const marked = css.indexOf('.os-choice-card[aria-checked="true"][aria-disabled="true"] {\n  border-color: var(--os-accent);');
  expect(quiet).toBeGreaterThan(-1);
  expect(marked).toBeGreaterThan(quiet);
});
