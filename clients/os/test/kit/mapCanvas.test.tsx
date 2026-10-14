import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { MapCanvas } from "../../src/kit/MapCanvas";

const transform = () => document.querySelector("[data-os-map-view]")!.getAttribute("transform");
const canvas = () => screen.getByRole("application", { name: "Connections" });

afterEach(() => vi.restoreAllMocks());

describe("the shared map viewport", () => {
  it("leaves wheel and trackpad scrolling to the page, including over a node or a control", () => {
    const wheel = vi.fn();
    render(<div onWheel={wheel}><MapCanvas label="Connections" width={600} height={300} ready>
      <g role="button" aria-label="A node" tabIndex={0}><rect width={100} height={50} /></g>
    </MapCanvas></div>);
    const initial = transform();
    for (const target of [canvas(), screen.getByRole("button", { name: "A node" }), screen.getByRole("button", { name: "Zoom in" })]) {
      for (const deltaY of [-120, 120, -1, 1]) {
        const event = new WheelEvent("wheel", { deltaY, deltaX: 12, bubbles: true, cancelable: true });
        fireEvent(target, event);
        expect(event.defaultPrevented).toBe(false);
        expect(transform()).toBe(initial);
      }
    }
    expect(wheel).toHaveBeenCalledTimes(12);
  });

  it("zooms with the controls and keyboard, and resets pan and scale to the fitted view", () => {
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue(new DOMRect(0, 0, 800, 400));
    render(<MapCanvas label="Connections" width={1000} height={200} ready><rect width={1000} height={200} /></MapCanvas>);
    const initial = transform();
    fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
    expect(transform()).not.toBe(initial);
    fireEvent.click(screen.getByRole("button", { name: "Zoom out" }));
    expect(transform()).toBe(initial);
    fireEvent.keyDown(canvas(), { key: "+" });
    fireEvent.keyDown(canvas(), { key: "ArrowRight" });
    expect(transform()).not.toBe(initial);
    fireEvent.click(screen.getByRole("button", { name: "Reset zoom and position" }));
    expect(transform()).toBe(initial);
    fireEvent.keyDown(canvas(), { key: "-" });
    fireEvent.keyDown(canvas(), { key: "0" });
    expect(transform()).toBe(initial);
  });

  it("suppresses a dragged node's pointer click while preserving keyboard, assistive and subsequent pointer activation", () => {
    const select = vi.fn();
    render(<MapCanvas label="Connections" width={600} height={300} ready>
      <g role="button" aria-label="A node" tabIndex={0} onClick={select} onKeyDown={event => { if (event.key === "Enter") select(); }} />
    </MapCanvas>);
    const node = screen.getByRole("button", { name: "A node" });
    fireEvent.pointerDown(node, { pointerId: 1, clientX: 20, clientY: 20 });
    fireEvent.pointerMove(canvas(), { pointerId: 1, clientX: 70, clientY: 50 });
    fireEvent.pointerUp(canvas(), { pointerId: 1, clientX: 70, clientY: 50 });
    expect(transform()).toBe("translate(50 30) scale(1)");
    fireEvent.click(node, { detail: 1 });
    expect(select).not.toHaveBeenCalled();
    fireEvent.keyDown(node, { key: "Enter" });
    fireEvent.click(node, { detail: 0 });
    expect(select).toHaveBeenCalledTimes(2);
    fireEvent.pointerDown(node, { pointerId: 2, clientX: 70, clientY: 50 });
    fireEvent.pointerUp(node, { pointerId: 2, clientX: 70, clientY: 50 });
    fireEvent.click(node, { detail: 1 });
    expect(select).toHaveBeenCalledTimes(3);
  });

  it("does not pan when a node handles arrows itself", () => {
    render(<MapCanvas label="Connections" width={600} height={300} ready>
      <g role="button" aria-label="A node" tabIndex={0} onKeyDown={event => event.preventDefault()} />
    </MapCanvas>);
    const initial = transform();
    fireEvent.keyDown(screen.getByRole("button", { name: "A node" }), { key: "ArrowRight" });
    expect(transform()).toBe(initial);
    fireEvent.keyDown(canvas(), { key: "ArrowRight" });
    expect(transform()).not.toBe(initial);
  });

  it("keeps fitting new content after scrolling or tabbing, but respects a deliberate zoom", () => {
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue(new DOMRect(0, 0, 800, 400));
    const picture = (width: number) => <MapCanvas label="Connections" width={width} height={200} ready><rect width={width} height={200} /></MapCanvas>;
    const view = render(picture(1000));
    const initial = transform();
    fireEvent.wheel(canvas(), { deltaY: 100 });
    fireEvent.keyDown(canvas(), { key: "Tab" });
    view.rerender(picture(1200));
    expect(transform()).not.toBe(initial);
    fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
    const steered = transform();
    view.rerender(picture(1400));
    expect(transform()).toBe(steered);
    fireEvent.click(screen.getByRole("button", { name: "Reset zoom and position" }));
    const fitted = transform();
    view.rerender(picture(1600));
    expect(transform()).not.toBe(fitted);
  });

  it("keeps separate maps independent", () => {
    render(<><MapCanvas label="First" width={600} height={300} ready children={null} /><MapCanvas label="Second" width={600} height={300} ready children={null} /></>);
    const views = document.querySelectorAll("[data-os-map-view]");
    const second = views[1]!.getAttribute("transform");
    act(() => fireEvent.keyDown(screen.getByRole("application", { name: "First" }), { key: "+" }));
    expect(views[0]!.getAttribute("transform")).not.toBe(second);
    expect(views[1]!.getAttribute("transform")).toBe(second);
  });

  it("brings an offscreen focused node into view without changing zoom", () => {
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue(new DOMRect(0, 0, 600, 300));
    render(<MapCanvas label="Connections" width={600} height={300} ready>
      <g role="button" aria-label="A node" tabIndex={0} />
    </MapCanvas>);
    const node = screen.getByRole("button", { name: "A node" });
    vi.spyOn(node, "getBoundingClientRect").mockReturnValue(new DOMRect(700, 100, 100, 50));
    fireEvent.focus(node);
    expect(transform()).toBe("translate(-212 0) scale(1)");
  });
});
