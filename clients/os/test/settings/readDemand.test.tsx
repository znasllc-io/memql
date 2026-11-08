import { act, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ReactNode } from "react";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { SharedPackagesProvider, usePackages } from "../../src/apps/deployables/packages/usePackages";
import { SharedCredentialsProvider, useSourceCredentials } from "../../src/modules/connections/useSourceCredentials";
import { SharedExternalConnectionsProvider, useExternalConnections } from "../../src/modules/connections/useExternalConnections";
import { SharedSourceConnectionsProvider, useSourceConnections } from "../../src/modules/connections/connections";
import { fakeConnection, withSession } from "../deployables/harness";

const CATALOGS = ["packagesAll", "sourceCredentialsMine", "sourceConnectionsMine", "externalConnectionsMine"];

function Providers({ children }: { children?: ReactNode }) {
  return <SharedPackagesProvider><SharedCredentialsProvider><SharedSourceConnectionsProvider><SharedExternalConnectionsProvider>
    {children}
  </SharedExternalConnectionsProvider></SharedSourceConnectionsProvider></SharedCredentialsProvider></SharedPackagesProvider>;
}

function Consumer({ id }: { id: string }) {
  const packages = usePackages();
  const credentials = useSourceCredentials();
  const connections = useSourceConnections();
  const external = useExternalConnections();
  return <output data-testid={id}>{[packages.snapshot.state, credentials.snapshot.state, connections.state, external.snapshot.state].join(",")}</output>;
}

afterEach(() => vi.useRealTimers());

describe("shell catalog demand", () => {
  it("does not seed unrelated catalogs for a desktop without consumers", async () => {
    const connection = fakeConnection();
    h.connection = connection;
    const subscribe = vi.spyOn(connection.subscriptions, "subscribeGraph");
    render(withSession(<Providers><span>Desktop</span></Providers>));
    await act(async () => {});
    for (const name of CATALOGS) expect(connection.callsNamed(name)).toHaveLength(0);
    expect(subscribe).not.toHaveBeenCalled();
  });

  it("shares reads between consumers and releases them after the final consumer leaves", async () => {
    const connection = fakeConnection();
    h.connection = connection;
    const release = vi.fn();
    const subscribeGraph = connection.subscriptions.subscribeGraph;
    vi.spyOn(connection.subscriptions, "subscribeGraph").mockImplementation((handler, options) => {
      const stop = subscribeGraph(handler, options);
      return () => { release(); stop(); };
    });
    const view = render(withSession(<Providers><Consumer id="one" /><Consumer id="two" /></Providers>));
    await waitFor(() => expect(screen.getByTestId("one").textContent).toBe("live,live,live,live"));
    for (const name of CATALOGS) expect(connection.callsNamed(name)).toHaveLength(1);
    view.rerender(withSession(<Providers><Consumer id="two" /></Providers>));
    expect(release).not.toHaveBeenCalled();
    vi.useFakeTimers();
    view.rerender(withSession(<Providers />));
    await act(async () => { await vi.advanceTimersByTimeAsync(5_001); });
    expect(release).toHaveBeenCalledTimes(4);
  });

  it("keeps capability checks in force even when a consumer requests a catalog", async () => {
    const connection = fakeConnection();
    h.connection = connection;
    render(withSession(<Providers><Consumer id="denied" /></Providers>, { capabilities: [] }));
    await act(async () => {});
    for (const name of CATALOGS) expect(connection.callsNamed(name)).toHaveLength(0);
  });
});
