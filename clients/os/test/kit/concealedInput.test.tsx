import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it } from "vitest";

import { Input } from "../../src/kit";

// A concealed input (DESIGN.md, "Applying them"): a secret typed once, such as
// an API key. It is hidden as it is typed, the browser is told not to keep it,
// and the value is never written into the page's markup. The value used here
// is a fixture.

afterEach(cleanup);

const SECRET = "sk-test-concealed-0001";

function Held({ onValue }: { onValue?: (value: string) => void }) {
  const [value, setValue] = useState("");
  return (
    <>
      <Input id="secret" label="The API key" value={value} conceal onChange={(next) => { setValue(next); onValue?.(next); }} />
      <button type="button" onClick={() => setValue("")}>Clear</button>
      <output>{value.length}</output>
    </>
  );
}

describe("a concealed input", () => {
  it("is named, hides what is typed, and is not a field a browser offers to keep", () => {
    render(<Held />);
    const field = screen.getByLabelText("The API key") as HTMLInputElement;
    expect(field.classList.contains("os-input")).toBe(true);
    expect(field.classList.contains("os-input-concealed")).toBe(true);
    // A password field is what a browser's password manager looks for.
    expect(field.getAttribute("type")).toBe("text");
    expect(field.getAttribute("autocomplete")).toBe("off");
    expect(field.getAttribute("spellcheck")).toBe("false");
    expect(field.getAttribute("autocapitalize")).toBe("off");
    expect(field.getAttribute("autocorrect")).toBe("off");
    expect(field.getAttribute("name")).toBeNull();
  });

  it("hands every change to its owner and keeps the value out of the markup", () => {
    const seen: string[] = [];
    const view = render(<Held onValue={(value) => seen.push(value)} />);
    const field = screen.getByLabelText("The API key") as HTMLInputElement;
    fireEvent.change(field, { target: { value: SECRET } });
    expect(seen).toEqual([SECRET]);
    expect(field.value).toBe(SECRET);
    expect(screen.getByRole("status").textContent).toBe(String(SECRET.length));
    // The value is a property of the element and never an attribute of it.
    expect(field.getAttribute("value")).toBeNull();
    expect(view.container.innerHTML).not.toContain(SECRET);
  });

  it("empties when its owner clears it", () => {
    render(<Held />);
    const field = screen.getByLabelText("The API key") as HTMLInputElement;
    fireEvent.change(field, { target: { value: SECRET } });
    fireEvent.click(screen.getByRole("button", { name: "Clear" }));
    expect(field.value).toBe("");
  });

  it("leaves an ordinary input as it was", () => {
    render(<Input id="plain" label="Name" value="Personal" onChange={() => {}} />);
    const field = screen.getByLabelText("Name") as HTMLInputElement;
    expect(field.classList.contains("os-input-concealed")).toBe(false);
    expect(field.getAttribute("autocomplete")).toBeNull();
    expect(field.value).toBe("Personal");
  });
});
