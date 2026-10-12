import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { TextArea } from "../../src/kit";

afterEach(cleanup);

// Prose a person writes: a description, a sentence for a compiler. Fleet's
// "Describe a rule" page was the first; the Email app's "Describe the
// automation" is the second, which is the day it joins the kit.

describe("TextArea", () => {
  it("is a named multi-line field", () => {
    render(<TextArea id="describe" label="Describe the automation" value="" onChange={() => {}} />);
    const box = screen.getByRole("textbox", { name: "Describe the automation" });
    expect(box.tagName).toBe("TEXTAREA");
    expect((box as HTMLTextAreaElement).rows).toBe(3);
  });

  it("takes its row count, placeholder and value from the caller and hands typing back as text", () => {
    const onChange = vi.fn();
    render(
      <TextArea
        id="describe"
        label="Describe the automation"
        value="When someone joins"
        placeholder="Say what should happen"
        rows={6}
        onChange={onChange}
      />,
    );
    const box = screen.getByLabelText("Describe the automation") as HTMLTextAreaElement;
    expect(box.rows).toBe(6);
    expect(box.placeholder).toBe("Say what should happen");
    expect(box.value).toBe("When someone joins");
    fireEvent.change(box, { target: { value: "When someone joins, send Welcome 2" } });
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith("When someone joins, send Welcome 2");
  });

  it("names the field to assistive tech and wears the shared field rule", () => {
    render(<TextArea id="describe" label="Describe the automation" value="" onChange={() => {}} />);
    const box = screen.getByLabelText("Describe the automation");
    const label = document.querySelector('label[for="describe"]');
    expect(label?.textContent).toBe("Describe the automation");
    expect(label?.classList.contains("os-sr-only")).toBe(true);
    expect(box.id).toBe("describe");
    // `os-input` is the field's colour, border, radius and text size (rule 5);
    // `os-textarea` is the one thing that differs: it grows past the line.
    expect(box.classList.contains("os-input")).toBe(true);
    expect(box.classList.contains("os-textarea")).toBe(true);
  });
});

// jsdom performs no layout, so the height cannot be measured here; what a test
// can pin is the declaration and its place in the file. `.os-input` is a
// single-line field (`height: var(--os-control-h)`), and a text area wearing it
// is only as tall as one line unless the later rule wins -- a rule of equal
// specificity that comes first loses, silently.
describe("the text area's stylesheet rule", () => {
  const CSS = readFileSync(join(__dirname, "..", "..", "src", "styles", "index.css"), "utf8");

  /** The text of the one rule that starts a line with this selector. */
  function ruleOf(selector: string): string {
    const start = CSS.indexOf(`\n${selector} {`);
    expect(start).toBeGreaterThan(-1);
    expect(CSS.indexOf(`\n${selector} {`, start + 1)).toBe(-1);
    return CSS.slice(start, CSS.indexOf("\n}", start));
  }

  it("lets the field grow past the control line, from two lines, vertically", () => {
    const rule = ruleOf(".os-textarea");
    expect(rule).toContain("height: auto;");
    expect(rule).toContain("min-height: calc(var(--os-control-h) * 2);");
    expect(rule).toContain("resize: vertical;");
  });

  it("comes after the field rule it overrides", () => {
    const field = CSS.indexOf("\n.os-input,\n.os-select {");
    expect(field).toBeGreaterThan(-1);
    expect(CSS.indexOf("\n.os-textarea {")).toBeGreaterThan(field);
  });

  it("is the only name anything in src has for it", () => {
    // The rename touched two stylesheets and a component. A selector left
    // behind under the old name is a dead rule, and it fails silently: nothing
    // errors, the text area just loses a rule.
    const SRC = join(__dirname, "..", "..", "src");
    const stale = readdirSync(SRC, { recursive: true, encoding: "utf8" })
      .filter((file) => /\.(css|tsx?)$/.test(file))
      .filter((file) => readFileSync(join(SRC, file), "utf8").includes("os-rule-textarea"));
    expect(stale).toEqual([]);
  });

  it("keeps the Describe a rule page's measure on the new name", () => {
    const routing = readFileSync(join(__dirname, "..", "..", "src", "apps", "fleet", "routing", "routing.css"), "utf8");
    expect(routing).toContain(".fleet-rule-page .os-textarea { max-width: 80ch; }");
  });
});
