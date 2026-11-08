import { StrictMode } from "react";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { loadApp } from "../../src/system/loadApp";
import { WindowErrorBoundary } from "../../src/chrome/WindowErrorBoundary";
import type { OsAppProps } from "../../src/system/registry";

const props: OsAppProps = { sectionId: "recent", navigate: () => {}, askContext: () => {} };

describe("installed app loading", () => {
  it("loads only on open, shares an in-flight load and retains it after close", async () => {
    let finish!: (component: (props: OsAppProps) => React.ReactNode) => void;
    const load = vi.fn(() => new Promise<(props: OsAppProps) => React.ReactNode>(resolve => { finish = resolve; }));
    const App = loadApp("Library", load);
    expect(load).not.toHaveBeenCalled();
    const view = render(<StrictMode><App {...props} /><App {...props} /></StrictMode>);
    expect(screen.getAllByText("Opening Library")).toHaveLength(2);
    await waitFor(() => expect(load).toHaveBeenCalledTimes(1));
    await act(async () => { finish(({ sectionId }) => <p>Library {sectionId}</p>); });
    expect(screen.getAllByText("Library recent")).toHaveLength(2);
    view.rerender(<span>Closed</span>);
    view.rerender(<App {...props} sectionId="folders" />);
    expect(screen.getByText("Library folders")).toBeTruthy();
    expect(screen.queryByText("Opening Library")).toBeNull();
    expect(load).toHaveBeenCalledTimes(1);
  });

  it("contains a download failure in its window and retries when the app is reloaded", async () => {
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    const load = vi.fn()
      .mockRejectedValueOnce(new Error("The app download was interrupted"))
      .mockResolvedValueOnce(() => <p>App content</p>);
    const App = loadApp("Library", load);
    try {
      render(<><span>Desktop stays available</span><WindowErrorBoundary app="library" section="recent"><App {...props} /></WindowErrorBoundary></>);
      expect(await screen.findByText("This app hit an error.")).toBeTruthy();
      expect(screen.getByText("Desktop stays available")).toBeTruthy();
      fireEvent.click(screen.getByRole("button", { name: "Reload app" }));
      expect(await screen.findByText("App content")).toBeTruthy();
      expect(load).toHaveBeenCalledTimes(2);
    } finally { consoleError.mockRestore(); }
  });

  it("does not mount a downloaded app after its window closes", async () => {
    let finish!: (component: () => React.ReactNode) => void;
    const mount = vi.fn(() => <p>Late app</p>);
    const load = vi.fn(() => new Promise<() => React.ReactNode>(resolve => { finish = resolve; }));
    const App = loadApp("Library", load);
    const view = render(<App {...props} />);
    await waitFor(() => expect(load).toHaveBeenCalledTimes(1));
    view.unmount();
    await act(async () => { finish(mount); });
    expect(mount).not.toHaveBeenCalled();
    render(<App {...props} />);
    expect(screen.getByText("Late app")).toBeTruthy();
    expect(load).toHaveBeenCalledTimes(1);
  });
});
