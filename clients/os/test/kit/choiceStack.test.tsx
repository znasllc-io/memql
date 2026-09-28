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
