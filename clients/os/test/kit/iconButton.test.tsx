import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { IconButton } from "../../src/kit/IconButton";

// The icon button's name on hover. By default it is the browser's own `title`
// tooltip, which the browser holds back; `tip="above"` draws it at once,
// above the button (Files' picked-file icons, Jesús Nov 20 2026).
describe("IconButton's tooltip", () => {
  it("leaves the browser's own tooltip in place by default", () => {
    render(<IconButton label="Download">x</IconButton>);
    expect(screen.getByRole("button", { name: "Download" }).title).toBe("Download");
    fireEvent.pointerEnter(screen.getByRole("button", { name: "Download" }));
    expect(document.querySelector(".os-tip")).toBeNull();
  });

  it("draws the name at once above the button, and hides it on leave and on a press", () => {
    const enter = vi.fn();
    const press = vi.fn();
    render(<IconButton label="Details" tip="above" onPointerEnter={enter} onClick={press}>x</IconButton>);
    const button = screen.getByRole("button", { name: "Details" });
    // No title, so the browser's late tooltip never doubles the drawn one.
    expect(button.hasAttribute("title")).toBe(false);
    vi.spyOn(button, "getBoundingClientRect").mockReturnValue({ left: 100, top: 40, width: 36, height: 36 } as DOMRect);

    fireEvent.pointerEnter(button);
    const tip = document.body.querySelector<HTMLElement>(".os-tip");
    expect(tip?.textContent).toBe("Details");
    // Centred on the button, anchored at its top edge; the stylesheet lifts it above.
    expect(tip?.style.left).toBe("118px");
    expect(tip?.style.top).toBe("40px");
    expect(enter).toHaveBeenCalledOnce();

    fireEvent.pointerDown(button);
    expect(document.querySelector(".os-tip")).toBeNull();
    fireEvent.click(button);
    expect(press).toHaveBeenCalledOnce();

    fireEvent.pointerEnter(button);
    fireEvent.pointerLeave(button);
    expect(document.querySelector(".os-tip")).toBeNull();
  });
});
