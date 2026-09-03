import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { WindowErrorBoundary } from "../../src/chrome/WindowErrorBoundary";
import {
  flushCapture,
  resetCaptureForTest,
  setCaptureTransport,
  type CaptureLine,
} from "../../src/logs/capture";

// The per-window error boundary (epic memql#4895, spec H): a render error
// stays in its window, and the line it reports names the app and section
// EXACTLY -- from props, not from a guess about which window had focus.

function Bomb({ message }: { message: string }): never {
  throw new Error(message);
}

// Throws while ARMED, and the test disarms it from outside the render. A
// component that flips its own flag mid-render throws on React's first
// concurrent attempt and succeeds on the synchronous retry, which React 19
// reports as a RECOVERABLE error through reportError -- an unhandled error
// in vitest, and not what a boundary test is about.
let armed = true;
function Armed({ message, after }: { message: string; after: string }) {
  if (armed) throw new Error(message);
  return <p>{after}</p>;
}

let sent: CaptureLine[][] = [];

beforeEach(() => {
  resetCaptureForTest();
  armed = true;
  sent = [];
  setCaptureTransport(async (_session, lines) => {
    sent.push(lines);
    return {};
  });
  // React reports a caught render error through console.error; that is the
  // noise, not the subject.
  vi.spyOn(console, "error").mockImplementation(() => {});
});

afterEach(() => {
  vi.restoreAllMocks();
  resetCaptureForTest();
});

describe("the window error boundary", () => {
  it("contains the fault and says so with the error's own sentence and a way back", () => {
    render(
      <WindowErrorBoundary app="fleet" section="routing">
        <Bomb message="kaboom in the routing table" />
      </WindowErrorBoundary>,
    );
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText("This app hit an error.")).toBeTruthy();
    expect(screen.getByText("kaboom in the routing table")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reload app" })).toBeTruthy();
  });

  it("reports the line with the exact app id, section, stack and component stack", () => {
    render(
      <WindowErrorBoundary app="fleet" section="routing">
        <Bomb message="kaboom in the routing table" />
      </WindowErrorBoundary>,
    );
    flushCapture();
    expect(sent).toHaveLength(1);
    const line = sent[0]?.[0];
    expect(line).toMatchObject({
      level: "error",
      app: "fleet",
      component: "os.fleet",
      message: "kaboom in the routing table",
    });
    expect(line?.attributes).toMatchObject({ section: "routing", source: "boundary" });
    expect(typeof line?.attributes?.componentStack).toBe("string");
    expect(typeof line?.attributes?.stack).toBe("string");
  });

  it("Reload app remounts the body", () => {
    render(
      <WindowErrorBoundary app="files" section="browse">
        <Armed message="first render only" after="recovered" />
      </WindowErrorBoundary>,
    );
    expect(screen.getByText("first render only")).toBeTruthy();
    armed = false;
    fireEvent.click(screen.getByRole("button", { name: "Reload app" }));
    expect(screen.getByText("recovered")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("navigating to another section is a way out too", () => {
    const view = render(
      <WindowErrorBoundary app="files" section="browse">
        <Armed message="browse blew up" after="backups" />
      </WindowErrorBoundary>,
    );
    expect(screen.getByText("browse blew up")).toBeTruthy();
    armed = false;
    view.rerender(
      <WindowErrorBoundary app="files" section="backups">
        <Armed message="browse blew up" after="backups" />
      </WindowErrorBoundary>,
    );
    expect(screen.getByText("backups")).toBeTruthy();
  });
});
