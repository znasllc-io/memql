import { useState } from "react";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { Result } from "@znasllc-io/memql-sdk-core/client";
import { ClusterApp } from "../../src/apps/cluster/ClusterApp";
import { withSession } from "../deployables/harness";

const h = vi.hoisted(() => ({ query: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => ({ query: h.query }) }));
vi.mock("../../src/chrome/connection", () => ({ useConnectionStatus: () => "connected" }));

it("retains the exact released candidate through cross-app navigation and waits for proposal confirmation", async () => {
  const release = { candidateId: `sha256:${"a".repeat(64)}`, catalogDigest: `sha256:${"b".repeat(64)}`, label: "engine 0.31.19" };
  const executeNamed = vi.fn(async (name: string) => {
    if (name === "releaseInstallationStatus") return new Result({ data: [{ installationId: "instance", currentRevision: "c".repeat(40), choices: [], active: null, proposals: [], proposalBaseRevision: "d".repeat(40), proposalLandingMode: "owner_push" }] } as never);
    throw new Error("fixture stops after the proposal request");
  });
  h.query = { executeNamed };
  const consumed = vi.fn();
  function Destination() {
    const [section, setSection] = useState("runtime");
    const [intent, setIntent] = useState<{ id: string; payload: { release: typeof release } } | undefined>({ id: "release-selection", payload: { release } });
    return <ClusterApp sectionId={section} navigate={setSection} askContext={() => {}} intent={intent} consumeIntent={id => { consumed(id); setIntent(undefined); }} />;
  }
  render(withSession(<Destination />));
  await screen.findByRole("heading", { name: release.label });
  expect(consumed).toHaveBeenCalledWith("release-selection");
  expect(executeNamed.mock.calls.map(c => c[0])).toEqual(["releaseInstallationStatus"]);
  fireEvent.click(await screen.findByRole("button", { name: "Prepare update proposal" }));
  await waitFor(() => expect(executeNamed.mock.calls.some(c => c[0] === "releaseProposeInstallation")).toBe(true));
  const call = (executeNamed.mock.calls as unknown as string[][]).find(c => c[0] === "releaseProposeInstallation")!;
  expect(call[1]).toContain(release.candidateId);
  expect(call[1]).toContain(release.catalogDigest);
  expect(executeNamed.mock.calls.some(c => /Apply|Land/.test(c[0]))).toBe(false);
});
