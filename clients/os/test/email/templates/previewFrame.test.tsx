import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { Notice } from "../../../src/kit";
import { PREVIEW_SANDBOX, Preview } from "../../../src/apps/email/templates/Preview";

// THE SHARED PREVIEW (memql#6033, Ruling 21; Review Focus 7): an email shown as
// it is sent, which can neither run nor fetch anything -- the sanitizer
// (test/email/preview.test.ts), the policy it writes first, and the frame's
// sandbox, asserted here on the component a template's page, a layout's page
// and the capture inbox all draw.

const HTML = '<html><body style="background-color:#eef0f3"><p style="color:#2b2b2b" onclick="alert(1)">Hello</p><script>alert(2)</script></body></html>';

describe("the email preview", () => {
  it("isolates the email: no scripts, no origin of its own, no referrer, and a policy that loads nothing", () => {
    render(<Preview html={HTML} text="Hello" />);
    const frame = screen.getByTitle("Email preview");
    expect(frame.getAttribute("sandbox")).toBe(PREVIEW_SANDBOX);
    expect(PREVIEW_SANDBOX.split(" ")).toEqual(["allow-popups", "allow-popups-to-escape-sandbox"]);
    expect(frame.getAttribute("referrerpolicy")).toBe("no-referrer");
    const doc = new DOMParser().parseFromString(frame.getAttribute("srcdoc") ?? "", "text/html");
    expect(doc.head.firstElementChild?.getAttribute("content")).toContain("default-src 'none'");
    expect(doc.querySelectorAll("script, [onclick]")).toHaveLength(0);
    // Its own look stays.
    expect(doc.querySelector("p")?.getAttribute("style")).toBe("color:#2b2b2b");
    expect(doc.body.getAttribute("style")).toBe("background-color:#eef0f3");
  });

  it("switches between the formatted email and its text", () => {
    render(<Preview html={HTML} text="Hello, in text" />);
    const tabs = screen.getByRole("navigation", { name: "Preview" });
    expect(within(tabs).getByRole("button", { name: "Formatted" }).getAttribute("aria-current")).toBe("page");
    fireEvent.click(within(tabs).getByRole("button", { name: "Plain text" }));
    expect(screen.queryByTitle("Email preview")).toBeNull();
    expect(document.querySelector("pre")?.textContent).toBe("Hello, in text");
  });

  it("shows a message with only text as text, with nothing to switch between", () => {
    render(<Preview html="" text="Just words" />);
    expect(screen.queryByRole("navigation", { name: "Preview" })).toBeNull();
    expect(screen.queryByTitle("Email preview")).toBeNull();
    expect(document.querySelector("pre")?.textContent).toBe("Just words");
  });

  it("draws the frame's shape while there is nothing to show yet, and a failure in its place", () => {
    const { container, rerender } = render(<Preview html="" text="" loading />);
    expect(container.querySelector(".os-content-skeleton")).not.toBeNull();
    expect(screen.queryByTitle("Email preview")).toBeNull();
    rerender(<Preview html="" text="" failed={<Notice tone="error" sentence="This cluster did not return the preview." />} />);
    expect(screen.getByText("This cluster did not return the preview.")).toBeTruthy();
    expect(screen.queryByRole("navigation", { name: "Preview" })).toBeNull();
    expect(container.querySelector(".os-content-skeleton")).toBeNull();
  });
});
