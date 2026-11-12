import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { Absent, Measure } from "../../src/kit";
import { IconButton } from "../../src/kit/IconButton";
import { AddButton, AddLink } from "../../src/kit/AddButton";

afterEach(cleanup);

// ONE SPELLING OF "NO ANSWER": the kit's em dash with its reason a hover away,
// drawn by `Absent` and by `Measure` for an absent figure alike.
describe("Absent", () => {
  it("draws the kit's dash with its reason as the title", () => {
    const { container } = render(<Absent reason="Not recorded" />);
    const dash = container.querySelector(".os-figure-absent") as HTMLElement;
    expect(dash.textContent).toBe("—");
    expect(dash.getAttribute("title")).toBe("Not recorded");
  });

  it("is the dash Measure draws for an absent figure", () => {
    const measured = render(<Measure figure={{ kind: "absent", reason: "unmeasured", detail: "" }} />).container.innerHTML;
    cleanup();
    const absent = render(<Absent reason="Nothing has reported this yet." />).container.innerHTML;
    expect(absent).toBe(measured);
  });
});

// THE ICON BUTTON'S SIZE IS ONE TOKEN (`--os-icon-button-size`), read by the
// button and by any line that must keep its height.
describe("the icon button's size", () => {
  it("is read from the kit's token by IconButton, AddButton and AddLink", () => {
    const { container } = render(<>
      <IconButton label="Close" onClick={() => {}}>x</IconButton>
      <AddButton label="Add" onClick={() => {}} />
      <AddLink label="Add a link" href="#" />
    </>);
    for (const el of container.querySelectorAll<HTMLElement>(".os-icon-button")) {
      expect(el.style.minWidth).toBe("var(--os-icon-button-size)");
      expect(el.style.minHeight).toBe("var(--os-icon-button-size)");
    }
    expect(container.querySelectorAll(".os-icon-button")).toHaveLength(3);
  });
});
