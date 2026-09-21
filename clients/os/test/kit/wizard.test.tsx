import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Stop } from "../../src/kit";
import { Wizard } from "../../src/kit/Wizard";

afterEach(cleanup);

// THE WIZARD'S CONTRACT, which is the same for every thing a person can add:
// one column of steps with one of them open, and a floor that says whose turn
// it is. What each flow puts IN the steps is that flow's own test.

const STEPS: Stop[] = [
  { id: "name", name: "Name", state: "done", answer: "studio-mac-mini", openable: false },
  { id: "install", name: "Install", state: "open", sentence: "One line, on the machine itself.", body: <p>the command</p> },
  { id: "connect", name: "Connect", state: "current", answer: "Listening for the machine", body: <p>what usually goes wrong</p> },
  { id: "checks", name: "Checks", state: "ahead", sentence: "Online, steady, and what you asked for." },
];

function mount(over: Partial<Parameters<typeof Wizard>[0]> = {}) {
  const onOpen = vi.fn();
  render(
    <Wizard
      icon={<svg />}
      title="Add a machine"
      lead="Connect a computer to this cluster."
      label="Adding a machine"
      steps={STEPS}
      open="install"
      onOpen={onOpen}
      status={{ word: "Waiting for studio-mac-mini", detail: "run the command on the machine", tone: "busy", meta: "0:42" }}
      acts={[{ label: "Cancel", tone: "quiet", onAct: () => {} }]}
      {...over}
    />,
  );
  return { onOpen };
}

const bar = () => document.querySelector(".os-actbar") as HTMLElement;

describe("the wizard", () => {
  it("is a region named by its title, with the title as its heading", () => {
    mount();
    const region = screen.getByRole("region", { name: "Add a machine" });
    expect(within(region).getByRole("heading", { name: "Add a machine" })).toBeTruthy();
    expect(within(region).getByText("Connect a computer to this cluster.")).toBeTruthy();
  });

  it("draws every step as a line in order, carrying the rail's own states", () => {
    mount();
    const items = [...screen.getByRole("list", { name: "Adding a machine" }).querySelectorAll(":scope > li")];
    expect(items.map((li) => li.getAttribute("data-state"))).toEqual(["done", "open", "current", "ahead"]);
    // A settled step reads back what was answered.
    expect(within(items[0] as HTMLElement).getByText("studio-mac-mini")).toBeTruthy();
  });

  it("draws the body of the open step and of no other", () => {
    mount();
    expect(screen.getByText("the command")).toBeTruthy();
    expect(screen.queryByText("what usually goes wrong")).toBeNull();
  });

  it("opens a reachable step on a click, and never closes the open one", () => {
    const { onOpen } = mount();
    fireEvent.click(screen.getByRole("button", { name: /^Connect/ }));
    expect(onOpen).toHaveBeenCalledWith("connect");
    onOpen.mockClear();
    // ONE STEP IS ALWAYS OPEN: a wizard with nothing open is a list of names
    // over a button that would act on a form nobody can see.
    fireEvent.click(screen.getByRole("button", { name: /^Install/ }));
    expect(onOpen).not.toHaveBeenCalled();
  });

  it("makes a step that cannot be opened a line rather than a control", () => {
    mount();
    // Answered and closed by the flow; not reached yet.
    expect(screen.queryByRole("button", { name: /^Name/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Checks/ })).toBeNull();
  });

  it("names a step nobody has reached and says nothing more about it", () => {
    mount();
    expect(screen.getByText("Checks")).toBeTruthy();
    expect(screen.queryByText("Online, steady, and what you asked for.")).toBeNull();
    // The open step does say what it is for.
    expect(screen.getByText("One line, on the machine itself.")).toBeTruthy();
  });

  it("states whose turn it is on the floor: the words, the tone and the measure of the wait", () => {
    mount();
    expect(bar().getAttribute("data-tone")).toBe("busy");
    const status = within(bar()).getByRole("status");
    expect(within(status).getByText("Waiting for studio-mac-mini")).toBeTruthy();
    expect(within(status).getByText("run the command on the machine")).toBeTruthy();
    expect(within(status).getByText("0:42")).toBeTruthy();
  });

  it("keeps the acts on the floor, the way out first and the forward act last", () => {
    mount({
      status: { word: "Ready to mint" },
      acts: [
        { label: "Cancel", text: true, onAct: () => {} },
        { label: "Mint a token", tone: "primary", onAct: () => {} },
      ],
    });
    expect(within(bar()).getAllByRole("button").map((b) => b.textContent)).toEqual(["Cancel", "Mint a token"]);
    // The forward act lives nowhere else.
    expect(screen.getAllByRole("button", { name: "Mint a token" })).toHaveLength(1);
  });

  // ONE BUTTON ON THE FLOOR. Two buttons side by side ask to be weighed
  // against each other; a way out is not the same kind of thing as what
  // happens next, so it is dressed as a label -- and is still a real button
  // to a keyboard and to a screen reader.
  it("draws the way out as a text action beside the one button", () => {
    const onCancel = vi.fn();
    mount({
      status: { word: "Waiting for studio-mac-mini", tone: "busy" },
      acts: [
        { label: "Cancel", text: true, ariaLabel: "Cancel adding studio-mac-mini", onAct: onCancel },
        { label: "Leave", onAct: () => {} },
      ],
    });
    expect(bar().querySelectorAll(".os-button")).toHaveLength(1);
    expect(bar().querySelector(".os-button")?.textContent).toBe("Leave");
    const cancel = within(bar()).getByRole("button", { name: "Cancel adding studio-mac-mini" });
    expect(cancel.classList.contains("os-actbar-text")).toBe(true);
    expect(cancel.tagName).toBe("BUTTON");
    fireEvent.click(cancel);
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("draws no measure of a wait when nothing is being waited for", () => {
    mount({ status: { word: "Describe the machine", detail: "a name, and which operating system it runs" } });
    expect(bar().getAttribute("data-tone")).toBe("none");
    expect(bar().querySelector(".os-actbar-meta")).toBeNull();
    expect(bar().querySelector(".os-actbar-dot")).toBeNull();
  });

  it("replaces the acts' prose with a question when the flow has one to ask", () => {
    mount({ confirm: <p>Leave? The token you copied still works.</p> });
    expect(within(bar()).getByText("Leave? The token you copied still works.")).toBeTruthy();
  });
});

describe("the way in", () => {
  it("puts the cursor in the open step's first text field", () => {
    render(
      <Wizard
        icon={<svg />}
        title="Add a domain"
        label="Adding a domain"
        steps={[{ id: "domain", name: "Domain", state: "open", body: <input aria-label="Domain to bind" /> }, { id: "dns", name: "DNS", state: "ahead" }]}
        open="domain"
        onOpen={() => {}}
        status={{ word: "Name the domain" }}
      />,
    );
    expect(document.activeElement).toBe(screen.getByLabelText("Domain to bind"));
  });
});

// IT TAKES THE WINDOW. Given room the wizard is two panes -- the steps standing
// still on the left, the open step with the rest of the width -- because a
// centred column painted a third of a wide window and wrapped the one thing on
// the page that wants width. The choice is measured, so a test says which.
describe("side by side, when there is room", () => {
  it("is one column where nothing can be measured, which is the arrangement that needs no room", () => {
    mount();
    expect(document.querySelector(".os-wizard")?.getAttribute("data-layout")).toBe("stack");
    // The open step's body sits beneath its own line.
    expect(document.querySelector('.os-rail-stage[data-open="true"] .os-wizard-body')).not.toBeNull();
  });

  it("moves the open step's body out of the rail and into the stage", () => {
    mount({ layout: "split" });
    expect(document.querySelector(".os-wizard")?.getAttribute("data-layout")).toBe("split");
    const aside = document.querySelector(".os-wizard-aside") as HTMLElement;
    const stage = document.querySelector(".os-wizard-stage") as HTMLElement;
    // The rail is still the steps, in order, with their states and answers...
    expect([...aside.querySelectorAll(".os-rail > li")].map((li) => li.getAttribute("data-state"))).toEqual(["done", "open", "current", "ahead"]);
    expect(within(aside).getByText("studio-mac-mini")).toBeTruthy();
    // ...and holds no body: ONE copy of the step's content, in the stage.
    expect(aside.querySelector(".os-wizard-body")).toBeNull();
    expect(within(stage).getByText("the command")).toBeTruthy();
    expect(screen.getAllByText("the command")).toHaveLength(1);
  });

  it("heads the stage with the open step's name and what it asks, said once", () => {
    mount({ layout: "split" });
    const stage = document.querySelector(".os-wizard-stage") as HTMLElement;
    expect(within(stage).getByRole("heading", { name: "Install" })).toBeTruthy();
    expect(within(stage).getByText("One line, on the machine itself.")).toBeTruthy();
    // The rail's line is the name and the answer; the sentence is the stage's.
    expect(screen.getAllByText("One line, on the machine itself.")).toHaveLength(1);
  });

  it("changes what the stage shows from the rail", () => {
    const { onOpen } = mount({ layout: "split" });
    fireEvent.click(within(document.querySelector(".os-wizard-aside") as HTMLElement).getByRole("button", { name: /^Connect/ }));
    expect(onOpen).toHaveBeenCalledWith("connect");
  });

  it("keeps the title, the floor and the region where they were", () => {
    mount({ layout: "split" });
    const region = screen.getByRole("region", { name: "Add a machine" });
    expect(within(region).getByRole("heading", { name: "Add a machine" })).toBeTruthy();
    expect(within(bar()).getByRole("status")).toBeTruthy();
  });

  it("puts the cursor in the stage's first text field on the way in", () => {
    render(
      <Wizard
        layout="split"
        icon={<svg />}
        title="Add a domain"
        label="Adding a domain"
        steps={[{ id: "domain", name: "Domain", state: "open", body: <input aria-label="Domain to bind" /> }, { id: "dns", name: "DNS", state: "ahead" }]}
        open="domain"
        onOpen={() => {}}
        status={{ word: "Name the domain" }}
      />,
    );
    expect(document.activeElement).toBe(screen.getByLabelText("Domain to bind"));
  });
});

// FOUND BY TYPING INTO A FRESHLY OPENED WIZARD IN A WIDE WINDOW and watching
// nothing land. The arrangement is measured after the first commit, and going
// side by side moves the open step's body to another place in the tree -- so
// the field focused on mount was unmounted a moment later and took the cursor
// with it.
describe("the cursor, when the arrangement settles", () => {
  const field = (layout: "stack" | "split") => (
    <Wizard
      layout={layout}
      icon={<svg />}
      title="Add a domain"
      label="Adding a domain"
      steps={[{ id: "domain", name: "Domain", state: "open", body: <input aria-label="Domain to bind" /> }, { id: "dns", name: "DNS", state: "ahead" }]}
      open="domain"
      onOpen={() => {}}
      status={{ word: "Name the domain" }}
    />
  );

  it("is put back in the first field after the wizard goes side by side", () => {
    const view = render(field("stack"));
    const before = screen.getByLabelText("Domain to bind");
    expect(document.activeElement).toBe(before);
    view.rerender(field("split"));
    const after = screen.getByLabelText("Domain to bind");
    // A different element: the body moved to the stage...
    expect(after).not.toBe(before);
    // ...and the cursor went with it.
    expect(document.activeElement).toBe(after);
  });

  it("is never taken back once the person has touched the wizard", () => {
    const view = render(
      <>
        {field("stack")}
        <button type="button">elsewhere</button>
      </>,
    );
    fireEvent.keyDown(screen.getByLabelText("Domain to bind"), { key: "a" });
    const elsewhere = screen.getByRole("button", { name: "elsewhere" });
    elsewhere.focus();
    view.rerender(
      <>
        {field("split")}
        <button type="button">elsewhere</button>
      </>,
    );
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "elsewhere" }));
  });
});
