import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { DateTimeInput, Field } from "../../src/kit";

afterEach(cleanup);

// A local date and time is a field two surfaces ask for: Logs' custom window
// (From and To) and the Email app's Schedule and Repeat. The value is the
// browser's own `datetime-local` string, in the viewer's zone and with no
// offset, and the control neither parses nor converts it: what a zone means is
// the caller's decision (Logs turns it into an instant, Email names the zone in
// the label).

describe("DateTimeInput", () => {
  it("is a named local date-time field", () => {
    const onChange = vi.fn();
    render(<DateTimeInput id="send-at" label="Send at" value="" onChange={onChange} />);
    const field = screen.getByLabelText("Send at") as HTMLInputElement;
    expect(field.tagName).toBe("INPUT");
    expect(field.type).toBe("datetime-local");
    fireEvent.change(field, { target: { value: "2026-10-14T09:00" } });
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith("2026-10-14T09:00");
  });

  it("shows the value it is given, and hands the earliest time on to the browser", () => {
    const { rerender } = render(
      <DateTimeInput id="send-at" label="Send at" value="2026-10-14T09:00" min="2026-10-14T08:00" onChange={() => {}} />,
    );
    const field = screen.getByLabelText("Send at") as HTMLInputElement;
    expect(field.value).toBe("2026-10-14T09:00");
    expect(field.min).toBe("2026-10-14T08:00");

    // It is a controlled field: the caller's next value is what it shows, and
    // no earliest time is an attribute that is not there, not an empty one.
    rerender(<DateTimeInput id="send-at" label="Send at" value="2026-10-15T10:30" onChange={() => {}} />);
    expect(field.value).toBe("2026-10-15T10:30");
    expect(field.hasAttribute("min")).toBe(false);
  });

  it("names the field to assistive tech and stands on the control line", () => {
    render(<DateTimeInput id="send-at" label="Send at" value="" onChange={() => {}} />);
    const field = screen.getByLabelText("Send at");
    // The name is a real label bound by `for`, visually hidden like the kit
    // Input's; the field wears the shared field rule (rule 5: one control line).
    const label = document.querySelector('label[for="send-at"]');
    expect(label?.textContent).toBe("Send at");
    expect(label?.classList.contains("os-sr-only")).toBe(true);
    expect(field.id).toBe("send-at");
    expect(field.classList.contains("os-input")).toBe(true);
  });

  it("sits in a Field without naming the control twice", () => {
    // Logs draws each end of a custom window as a Field (its name above) that
    // holds this control. The Field's visible text is aria-hidden, so the
    // control is announced once, by its own label.
    render(
      <Field label="From">
        <DateTimeInput id="logs-search-from" label="From" value="" onChange={() => {}} />
      </Field>,
    );
    expect(screen.getAllByLabelText("From")).toHaveLength(1);
    expect((screen.getByLabelText("From") as HTMLInputElement).type).toBe("datetime-local");
  });
});
