import { afterEach, describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";

import { OpenInVsCode } from "../src/components/OpenInVsCode";
import { EDITOR_SCHEME_STORAGE_KEY } from "../src/cluster/editorLink";

afterEach(() => {
  globalThis.localStorage?.removeItem(EDITOR_SCHEME_STORAGE_KEY);
});

describe("OpenInVsCode", () => {
  it("is a link carrying the v=1 contract, with the install pointer beside it", () => {
    render(<OpenInVsCode domain="acme.example.com" kind="concept" name="v1:cognition:space" />);
    const link = screen.getByRole("link", { name: "Open definition in VS Code" });
    expect(link.getAttribute("href")).toBe(
      "vscode://znasllc.memql/open?v=1&cluster=acme.example.com&kind=concept&name=v1%3Acognition%3Aspace",
    );
    const help = screen.getByRole("link", { name: "how to install" });
    expect(help.getAttribute("href")).toContain("editors/vscode/README.md#install--update-the-extension-locally");
    expect(help.getAttribute("target")).toBe("_blank");
    expect(help.getAttribute("rel")).toContain("noopener");
  });

  it("switches to Insiders and remembers it", () => {
    const { unmount } = render(<OpenInVsCode domain="d.test" kind="concept" name="x" />);
    fireEvent.click(screen.getByRole("button", { name: "Use VS Code Insiders" }));
    expect(screen.getByRole("link", { name: "Open definition in VS Code Insiders" }).getAttribute("href")).toMatch(
      /^vscode-insiders:\/\//,
    );
    unmount();
    render(<OpenInVsCode domain="d.test" kind="concept" name="x" />);
    expect(screen.getByRole("link", { name: "Open definition in VS Code Insiders" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Use VS Code" })).toBeTruthy();
  });

  it("renders nothing when the cluster's domain is unknown", () => {
    const { container } = render(<OpenInVsCode domain="" kind="concept" name="x" />);
    expect(container.textContent).toBe("");
  });
});
