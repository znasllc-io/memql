import { cleanup, render } from "@testing-library/react";
import { useRef } from "react";
import { afterEach, expect, it, vi } from "vitest";

import { AnchoredPicker } from "../../src/kit/AnchoredPicker";

// The share picker in a half-desk window: its field was ~60px wide and the
// list took exactly that width, wrapping every name to a letter per line.

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

function Picker() {
  const anchor = useRef<HTMLInputElement>(null), owner = useRef<HTMLDivElement>(null), popup = useRef<HTMLDivElement>(null);
  return (
    <div ref={owner}>
      <input ref={anchor} aria-label="Users and groups" />
      <AnchoredPicker anchor={anchor} owner={owner} popupRef={popup} onClose={() => {}}>names</AnchoredPicker>
    </div>
  );
}

function renderWithField(width: number) {
  vi.spyOn(HTMLInputElement.prototype, "getBoundingClientRect").mockReturnValue({ left: 100, top: 100, bottom: 130, right: 100 + width, width, height: 30 } as DOMRect);
  const view = render(<Picker />);
  // First render has no anchor yet; the second portals and positions.
  view.rerender(<Picker />);
  return document.querySelector<HTMLElement>(".os-anchored-picker")!;
}

it("keeps a readable minimum width when its field is narrow", () => {
  expect(renderWithField(60).style.width).toBe("240px");
});

it("matches a field wider than the minimum", () => {
  expect(renderWithField(400).style.width).toBe("400px");
});

it("never runs past the viewport", () => {
  vi.spyOn(window, "innerWidth", "get").mockReturnValue(200);
  const popup = renderWithField(60);
  expect(popup.style.width).toBe("184px");
  expect(popup.style.left).toBe("8px");
});
