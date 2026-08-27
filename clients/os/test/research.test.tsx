import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { Shell } from "../src/chrome/Shell";
import { Research } from "../src/research/Research";

describe("Research", () => {
  it("is one component with two hosts — strip vs sheet", () => {
    const { rerender } = render(<Research host="strip" />);
    const strip = document.querySelector("[data-os-research]");
    expect(strip?.getAttribute("data-os-host")).toBe("strip");
    rerender(<Research host="sheet" />);
    expect(document.querySelector("[data-os-research]")?.getAttribute("data-os-host")).toBe(
      "sheet",
    );
  });

  it("is chrome, not a module, and cannot be closed as one", () => {
    render(<Research host="strip" />);
    const root = document.querySelector("[data-os-research]");
    expect(root).toBeTruthy();
    expect(root?.closest("[data-os-slot]")).toBeNull();
    expect(root?.getAttribute("data-os-module")).toBeNull();
    expect(screen.queryByRole("button", { name: /close/i })).toBeNull();
  });

  it("keeps a text well that works with voice off", () => {
    render(<Research host="strip" />);
    const well = screen.getByRole("textbox", { name: "Research" });
    fireEvent.change(well, { target: { value: "what is on this cluster" } });
    expect((well as HTMLTextAreaElement).value).toBe("what is on this cluster");
    expect(document.querySelector("[data-os-voice]")).toBeNull();
  });

  it("is always on for desktop (strip) and never occupies a slot", () => {
    render(<Shell layout="desktop" onSignOut={vi.fn()} />);
    const research = document.querySelector("[data-os-research]");
    expect(research).toBeTruthy();
    expect(research?.getAttribute("data-os-host")).toBe("strip");
    expect(research?.closest("[data-os-slot]")).toBeNull();
    expect(document.querySelectorAll("[data-os-slot]")).toHaveLength(2);
  });

  it("is a sheet on phone, still not a module", () => {
    render(<Shell layout="phone" onSignOut={vi.fn()} />);
    const research = document.querySelector("[data-os-research]");
    expect(research?.getAttribute("data-os-host")).toBe("sheet");
    expect(document.querySelector("[data-os-slot]")).toBeNull();
  });
});
