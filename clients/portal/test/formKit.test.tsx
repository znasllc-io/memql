// The form kit's control contract (memql#4504).
//
// WHAT THIS FILE CAN AND CANNOT PROVE, STATED UP FRONT.
//
// The defect this epic fixes is geometric: controls in one row rendered at
// four different heights and bottom-aligned to each other. jsdom performs no
// layout -- every getBoundingClientRect() here would return zeros -- so a test
// in this file CANNOT measure a pixel and must not pretend to.
//
// What it can prove is the thing that actually went wrong, which is upstream
// of the pixels: the controls did not SHARE A SOURCE for their height. So the
// assertions below are about the contract -- every line control resolves its
// height from `h-control` and nothing states a vertical metric of its own, the
// row aligns at the top, and the actions column's spacer is literally the same
// constant as the label line it compensates for. Given that contract, one
// token move changes all of them together, which is the property the operator
// needed and did not have.
//
// The pixel half is measured in a real browser, against a real build, in the
// visual QA pass (memql#4511) -- and only there, because only there is it true.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";

import { Button, ButtonLink } from "../src/ui/Button";
import { Checkbox, RadioGroup } from "../src/ui/Choice";
import { FIELD_LABEL_OFFSET, Field, Select, TextInput, Textarea } from "../src/ui/Field";
import { FormActions, FormRow } from "../src/ui/FormRow";

function classOf(el: Element | null): string {
  return el === null ? "" : el.getAttribute("class") ?? "";
}

/**
 * tokensOf splits a className into the set of classes it actually applies.
 *
 * Substring matching on a class string is quietly wrong here and it bit this
 * file while it was being written: "h-control-sm" CONTAINS "h-control", and a
 * \b word boundary does not separate them either, because `-` is not a word
 * character. A test asserting "the xs button does not carry h-control" passes
 * or fails for reasons unrelated to the code unless the comparison is against
 * whole tokens.
 */
function tokensOf(el: Element | null): Set<string> {
  return new Set(classOf(el).split(/\s+/).filter((t) => t !== ""));
}

describe("every control on a form line resolves its height from one token", () => {
  it("puts h-control on the text input", () => {
    render(<TextInput value="" onChange={() => {}} ariaLabel="Name" />);
    expect(tokensOf(screen.getByLabelText("Name"))).toContain("h-control");
  });

  it("puts h-control on the select", () => {
    render(
      <Select value="a" onChange={() => {}} ariaLabel="Vendor">
        <option value="a">A</option>
      </Select>,
    );
    expect(tokensOf(screen.getByLabelText("Vendor"))).toContain("h-control");
  });

  it("puts the same h-control on a size-sm button -- this is the whole fix", () => {
    // The reported bug in one assertion: the submit button beside a field is
    // the same height as the field, because both read the same token.
    render(<Button size="sm">Add</Button>);
    expect(tokensOf(screen.getByRole("button", { name: "Add" }))).toContain("h-control");
  });

  it("puts the compact token on a size-xs button, and not the full one", () => {
    render(<Button size="xs">Edit</Button>);
    const classes = tokensOf(screen.getByRole("button", { name: "Edit" }));
    expect(classes).toContain("h-control-sm");
    // And NOT the full one. See tokensOf: this is the assertion that cannot
    // be written as a substring check.
    expect(classes.has("h-control")).toBe(false);
  });

  it("builds ButtonLink from the same recipe as Button", () => {
    // A <button> and an <a> meant to look identical must not be two class
    // strings someone edits out of sync.
    render(
      <>
        <Button size="sm">Go</Button>
        <ButtonLink size="sm" href="/x">
          Go there
        </ButtonLink>
      </>,
    );
    const button = classOf(screen.getByRole("button", { name: "Go" }));
    const link = classOf(screen.getByRole("link", { name: "Go there" }));
    expect(link).toBe(button);
  });

  it("leaves the textarea flowing -- it has no single line to sit on", () => {
    render(<Textarea value="" onChange={() => {}} />);
    const textarea = document.querySelector("textarea");
    const cls = classOf(textarea);
    expect(tokensOf(textarea).has("h-control")).toBe(false);
    // But it keeps the inset LOOK, so a form does not reveal which control
    // happens to be fixed-height.
    expect(cls).toContain("border-line");
    expect(cls).toContain("bg-surface");
  });

  it("states no vertical padding on a line control", () => {
    // The regression this guards: `py-2` coming back. A line control that
    // carries both a fixed height and vertical padding is fine until the
    // content grows, and then it is a different height again -- which is the
    // accident the token replaced.
    render(<TextInput value="" onChange={() => {}} ariaLabel="Name" />);
    const padding = [...tokensOf(screen.getByLabelText("Name"))].filter((t) => t.startsWith("py-"));
    expect(padding).toEqual([]);
  });
});

describe("the select is drawn by us, not by the platform", () => {
  it("removes the native appearance", () => {
    // Without this a <select> adds per-platform chrome ON TOP of the box the
    // CSS asked for, so the identical class string measures differently on
    // Linux and macOS and no padding could have reconciled them.
    render(
      <Select value="a" onChange={() => {}} ariaLabel="Vendor">
        <option value="a">A</option>
      </Select>,
    );
    expect(tokensOf(screen.getByLabelText("Vendor"))).toContain("appearance-none");
  });

  it("draws the chevron as an element, hidden from assistive tech", () => {
    const { container } = render(
      <Select value="a" onChange={() => {}} ariaLabel="Vendor">
        <option value="a">A</option>
      </Select>,
    );
    const chevron = container.querySelector("svg[aria-hidden='true']");
    expect(chevron).not.toBeNull();
    // It must not eat the click that opens the select.
    expect(classOf(chevron)).toContain("pointer-events-none");
  });

  it("still reports its value through onChange as a string", () => {
    const onChange = vi.fn();
    render(
      <Select value="a" onChange={onChange} ariaLabel="Vendor">
        <option value="a">A</option>
        <option value="b">B</option>
      </Select>,
    );
    fireEvent.change(screen.getByLabelText("Vendor"), { target: { value: "b" } });
    expect(onChange).toHaveBeenCalledWith("b");
  });
});

describe("a form row aligns at the top", () => {
  it("is items-start, never items-end", () => {
    const { container } = render(
      <FormRow>
        <span>child</span>
      </FormRow>,
    );
    const cls = classOf(container.firstElementChild);
    expect(cls).toContain("items-start");
    // The banned class, asserted here as well as in the repo-root gate: this
    // is the component the gate sends people to, so it is the one place the
    // rule must be true by construction rather than by scanning.
    expect(cls).not.toContain("items-end");
  });

  it("becomes a real form when given onSubmit, so Enter submits", () => {
    const onSubmit = vi.fn((e: React.FormEvent) => e.preventDefault());
    const { container } = render(
      <FormRow onSubmit={onSubmit}>
        <Button type="submit">Go</Button>
      </FormRow>,
    );
    const form = container.querySelector("form");
    expect(form).not.toBeNull();
    fireEvent.submit(form as HTMLFormElement);
    expect(onSubmit).toHaveBeenCalledTimes(1);
  });

  it("is a plain div when it is not the form", () => {
    const { container } = render(
      <FormRow>
        <span>child</span>
      </FormRow>,
    );
    expect(container.querySelector("form")).toBeNull();
    expect(container.firstElementChild?.tagName).toBe("DIV");
  });
});

describe("the actions column compensates for the label line it cannot see", () => {
  it("spaces its buttons by the SAME constant the Field label uses", () => {
    // The drift this guards is specific and would be invisible: someone tunes
    // Field's label line and the buttons in every form row silently stop
    // aligning with the controls beside them. One exported constant, asserted
    // present on both nodes, is what makes that impossible to do by halves.
    const { container } = render(
      <FormRow>
        <Field label="Vendor">
          <TextInput value="" onChange={() => {}} />
        </Field>
        <FormActions>
          <Button>Add</Button>
        </FormActions>
      </FormRow>,
    );
    const labelLine = container.querySelector("label > span");
    const spacer = container.querySelector("span[aria-hidden='true']");
    expect(classOf(labelLine)).toContain(FIELD_LABEL_OFFSET);
    expect(classOf(spacer)).toContain(FIELD_LABEL_OFFSET);
  });

  it("hides the spacer from assistive tech and leaves it empty", () => {
    const { container } = render(
      <FormActions>
        <Button>Add</Button>
      </FormActions>,
    );
    const spacer = container.querySelector("span[aria-hidden='true']");
    expect(spacer).not.toBeNull();
    expect(spacer?.textContent).toBe("");
  });
});

describe("the Field label line is a fixed box", () => {
  it("truncates rather than wrapping, so a long label cannot move a neighbour", () => {
    const { container } = render(
      <Field label="A label long enough that it would certainly wrap in a narrow column">
        <TextInput value="" onChange={() => {}} />
      </Field>,
    );
    expect(classOf(container.querySelector("label > span"))).toContain("truncate");
  });

  it("states its own line-height", () => {
    // --memql-text-xs declares a font size and no line-height, so `text-xs`
    // alone leaves the line box to the cascade -- which is not a number a
    // layout can be built on. The label states leading explicitly.
    const { container } = render(
      <Field label="Vendor">
        <TextInput value="" onChange={() => {}} />
      </Field>,
    );
    expect(classOf(container.querySelector("label > span"))).toMatch(/\bleading-\d/);
  });

  it("keeps the hint below the control, where it affects no sibling", () => {
    const { container } = render(
      <Field label="API key" hint="Stored write-only.">
        <TextInput value="" onChange={() => {}} />
      </Field>,
    );
    const children = Array.from(container.querySelectorAll("label > *"));
    const input = children.findIndex((c) => c.tagName === "INPUT");
    const hint = children.findIndex((c) => c.textContent === "Stored write-only.");
    expect(input).toBeGreaterThanOrEqual(0);
    expect(hint).toBeGreaterThan(input);
  });
});

describe("Checkbox", () => {
  it("is labelled by its own text, so clicking the words toggles it", () => {
    const onChange = vi.fn();
    render(<Checkbox checked={false} onChange={onChange} label="Allow computer use" />);
    fireEvent.click(screen.getByLabelText("Allow computer use"));
    expect(onChange).toHaveBeenCalledWith(true);
  });

  it("reports the boolean, not the event", () => {
    const onChange = vi.fn();
    render(<Checkbox checked={true} onChange={onChange} label="On" />);
    fireEvent.click(screen.getByLabelText("On"));
    expect(onChange).toHaveBeenCalledWith(false);
  });

  it("stays a native input, so the space key and form participation are free", () => {
    render(<Checkbox checked={false} onChange={() => {}} label="Allow" />);
    const box = screen.getByLabelText("Allow");
    expect(box.tagName).toBe("INPUT");
    expect(box.getAttribute("type")).toBe("checkbox");
  });

  it("aligns the box to the FIRST line of a label that wraps", () => {
    // A centred box floats into the middle of a paragraph the moment a label
    // is two lines long -- and these labels carry sentences, because a
    // checkbox that changes what a machine may do has to say so.
    const { container } = render(
      <Checkbox checked={false} onChange={() => {}} label="A long consent sentence" />,
    );
    expect(classOf(container.querySelector("label"))).toContain("items-start");
  });

  it("disables the INPUT rather than only dimming the row", () => {
    // Asserted on the native property, not by clicking and expecting silence:
    // jsdom dispatches a synthetic click straight at the element and React
    // runs the handler, so a click-based assertion here would be measuring
    // jsdom rather than this component. `disabled` on the native input is the
    // real contract -- it is what stops the pointer, the space key, and form
    // submission in every engine, and dimming without it is a control that
    // looks off and still works.
    render(<Checkbox checked={false} onChange={() => {}} label="Allow" disabled />);
    expect((screen.getByLabelText("Allow") as HTMLInputElement).disabled).toBe(true);
  });
});

describe("RadioGroup", () => {
  const OPTIONS = [
    { value: "patch", label: "Patch" },
    { value: "minor", label: "Minor" },
  ];

  it("announces the group question with a legend", () => {
    render(
      <RadioGroup name="bump" legend="Version bump" value="patch" onChange={() => {}} options={OPTIONS} />,
    );
    expect(screen.getByRole("group", { name: "Version bump" })).toBeTruthy();
  });

  it("gives every option the same name, which is what makes them exclusive", () => {
    // Not styling: the shared name is what the BROWSER uses to make the
    // choice exclusive and to move between exactly these options with the
    // arrow keys. Two groups sharing a name is one group.
    render(<RadioGroup name="bump" value="patch" onChange={() => {}} options={OPTIONS} />);
    const names = screen
      .getAllByRole("radio")
      .map((r) => (r as HTMLInputElement).name);
    expect(names).toEqual(["bump", "bump"]);
  });

  it("marks exactly the selected option checked", () => {
    render(<RadioGroup name="bump" value="minor" onChange={() => {}} options={OPTIONS} />);
    expect((screen.getByLabelText("Patch") as HTMLInputElement).checked).toBe(false);
    expect((screen.getByLabelText("Minor") as HTMLInputElement).checked).toBe(true);
  });

  it("reports the chosen value", () => {
    const onChange = vi.fn();
    render(<RadioGroup name="bump" value="patch" onChange={onChange} options={OPTIONS} />);
    fireEvent.click(screen.getByLabelText("Minor"));
    expect(onChange).toHaveBeenCalledWith("minor");
  });

  it("disables every option when the group is disabled", () => {
    render(<RadioGroup name="bump" value="patch" onChange={() => {}} options={OPTIONS} disabled />);
    for (const radio of screen.getAllByRole("radio")) {
      expect((radio as HTMLInputElement).disabled).toBe(true);
    }
  });

  it("disables one option without disabling the group", () => {
    render(
      <RadioGroup
        name="bump"
        value="patch"
        onChange={() => {}}
        options={[OPTIONS[0], { ...OPTIONS[1], disabled: true }]}
      />,
    );
    expect((screen.getByLabelText("Patch") as HTMLInputElement).disabled).toBe(false);
    expect((screen.getByLabelText("Minor") as HTMLInputElement).disabled).toBe(true);
  });
});
