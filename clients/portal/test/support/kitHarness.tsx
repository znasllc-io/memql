import { vi } from "vitest";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ReactNode } from "react";
import type { AccessSummary, Connection } from "@znasllc-io/memql-sdk-core/client";

import { AuthProvider } from "../../src/auth/AuthProvider";
import { ClusterProvider } from "../../src/cluster/ClusterProvider";
import { asQueryClient } from "./queryFake";

// Rendering a KIT COMPONENT the way the app renders it.
//
// Several ui/ components are role-aware now (memql#4653): ErrorNotice's
// "Technical details" disclosure and PageGuide's technical section both ask
// the cluster who you are, through the same read the rail's admin rows use.
// That makes them untestable as bare components -- and rightly so, because a
// component that took the role as a PROP would let a page pass the wrong one.
//
// So this harness gives them the real providers over a fake dial, and takes
// the one thing a test actually varies: the role the cluster resolves.

export function renderInKit(
  ui: ReactNode,
  { role = "reader" }: { role?: string } = {},
) {
  const access = {
    requestId: "r1",
    userId: "user-1",
    primaryEmail: "ada@example.test",
    clusterRole: role,
    sessionId: "",
    displayName: "Ada Lovelace",
  } as unknown as AccessSummary;

  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "0.0.0-test",
        query: asQueryClient({
          listConcepts: vi.fn(async () => []),
          getMyAccess: vi.fn(async () => access),
        }),
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;

  return render(
    <MemoryRouter>
      <AuthProvider
        config={{
          identityUrl: "",
          identityApiBaseUrl: "",
          oauthClientId: "",
          authEnabled: false,
          domain: "",
        }}
        fetchImpl={async () => {
          throw new Error("a kit test must make no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://api.example.test/auth/callback"
      >
        <ClusterProvider dial={dial}>{ui}</ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}
