import { StrictMode, useState } from "react";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { PageFrame } from "./pageHarness";
import { PageHelp } from "../src/kit/pageHelp";
import { Head } from "../src/kit";
import { Dialog } from "../src/kit/Dialog";

function Pages() {
  const [tab, setTab] = useState("machine");
  const [count, setCount] = useState(1);
  return <PageFrame>
    <button onClick={() => setTab(tab === "machine" ? "history" : "machine")}>Switch page</button>
    <button onClick={() => setCount(count + 1)}>Update reading</button>
    <div hidden={tab !== "machine"}><Head title="Machine" />
      <PageHelp title="Hardware"><p>Read the reported hardware.</p></PageHelp>
      <PageHelp title="Models"><p>{count} models available.</p></PageHelp>
      <div aria-hidden="true"><PageHelp title="Hidden"><p>Not this page.</p></PageHelp></div>
    </div>
    <div hidden={tab !== "history"}><Head title="History" /><PageHelp title="Sessions"><p>Open a recorded session.</p></PageHelp></div>
  </PageFrame>;
}

describe("one help entry per page", () => {
  it("collects current sections in page order, omits retained pages, and restores focus", async () => {
    render(<StrictMode><Pages /></StrictMode>);
    const help = await screen.findByRole("button", { name: "Help with Machine" });
    expect(screen.getAllByRole("button", { name: /^Help with/ })).toHaveLength(1);
    expect(screen.queryByText("Read the reported hardware.")).toBeNull();
    help.focus(); fireEvent.click(help);
    const modal = screen.getByRole("dialog");
    expect(within(modal).getAllByRole("heading", { level: 4 }).map(node => node.textContent)).toEqual(["Hardware", "Models"]);
    expect(screen.queryByText("Not this page.")).toBeNull();
    expect(screen.queryByText("Open a recorded session.")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Update reading" }));
    expect(within(modal).getByText("2 models available.")).toBeTruthy();
    fireEvent.keyDown(modal, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(help);
    fireEvent.click(screen.getByRole("button", { name: "Switch page" }));
    fireEvent.click(await screen.findByRole("button", { name: "Help with History" }));
    expect(screen.getByText("Open a recorded session.")).toBeTruthy();
    expect(screen.queryByText("Read the reported hardware.")).toBeNull();
  });

  it("does not mix documentation across windows and removes unmounted sections", async () => {
    const view = render(<><PageFrame><Head title="First" /><PageHelp title="First section">First documentation</PageHelp></PageFrame><PageFrame><Head title="Second" /><PageHelp title="Second section">Second documentation</PageHelp></PageFrame></>);
    fireEvent.click(await screen.findByRole("button", { name: "Help with Second" }));
    expect(screen.getByText("Second documentation")).toBeTruthy();
    expect(screen.queryByText("First documentation")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Close help" }));
    view.rerender(<PageFrame><Head title="First" /></PageFrame>);
    fireEvent.click(await screen.findByRole("button", { name: "Help with First" }));
    expect(screen.queryByText("First documentation")).toBeNull();
    expect(screen.getByText("Documentation is coming soon.")).toBeTruthy();
  });

  it("keeps a modal's help separate and Escape preserves its draft and focus", async () => {
    const dismiss = vi.fn();
    render(<PageFrame><Head title="Machine" /><PageHelp title="Machine docs">Outer documentation</PageHelp>
      <Dialog title="Share machine" onDismiss={dismiss}>
        <input aria-label="People" defaultValue="Ana" />
        <PageHelp title="Sharing scope">Only the people you select.</PageHelp>
      </Dialog>
    </PageFrame>);
    const button = await screen.findByRole("button", { name: "Help with Share machine" });
    button.focus(); fireEvent.click(button);
    const help = screen.getAllByRole("dialog").at(-1);
    expect(help).toBeTruthy();
    expect(screen.queryByText("Outer documentation")).toBeNull();
    fireEvent.keyDown(help!, { key: "Escape" });
    await waitFor(() => expect(screen.getAllByRole("dialog")).toHaveLength(1));
    expect(dismiss).not.toHaveBeenCalled();
    expect((screen.getByLabelText("People") as HTMLInputElement).value).toBe("Ana");
    expect(document.activeElement).toBe(button);
  });
});
