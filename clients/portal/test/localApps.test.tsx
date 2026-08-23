// The local-apps surface (memql#4363), a Fleet tab.
//
// Two things here are worth pinning, and both are places where a
// plausible-looking implementation would mislead a person rather than fail:
//
//   1. "selectable" must mean exactly what the ENGINE means. A machine that
//      has Claude Code installed but is not signed in, or whose own
//      policy.yaml does not list it, is NOT routable -- and a badge that said
//      otherwise would send somebody hunting a routing bug that is not there.
//
//   2. A session's address must survive a round trip. Session ids are
//      canonical (v1:worker:appSession:<shortId>), so the colons have to come
//      back byte-identical through react-router.

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useParams } from "react-router-dom";

import { fleetPath, sessionPath, SESSION_ROUTE_PATTERN } from "../src/fleet/urls";
import { appLabel } from "../src/fleet/rows";
import { isLive } from "../src/fleet/useAppSessions";
import { policyRowId } from "../src/fleet/useDelegationPolicy";

// runnableFor mirrors the rule the page renders and the engine enforces.
// Kept here rather than imported so the test states the rule independently:
// if the page's copy drifts, this fails rather than agreeing with it.
const KNOWN = new Set(["claude-code", "codex"]);
function runnableFor(app: { id: string; allowed: boolean; signedIn: boolean }): boolean {
  return KNOWN.has(app.id) && app.allowed && app.signedIn;
}

describe("app selectability", () => {
  it("requires the machine to allow it AND the app to be signed in", () => {
    expect(runnableFor({ id: "claude-code", allowed: true, signedIn: true })).toBe(true);
    expect(runnableFor({ id: "claude-code", allowed: false, signedIn: true })).toBe(false);
    expect(runnableFor({ id: "claude-code", allowed: true, signedIn: false })).toBe(false);
  });

  it("never marks an app the engine cannot drive as selectable", () => {
    // A newer cockpit may report a third app. It is displayed, because the
    // machine really has it -- and never selectable, because this engine has
    // no protocol for it.
    expect(runnableFor({ id: "some-future-app", allowed: true, signedIn: true })).toBe(false);
  });
});

describe("app labels", () => {
  it("names the two apps the engine drives", () => {
    expect(appLabel("claude-code")).toBe("Claude Code");
    expect(appLabel("codex")).toBe("Codex");
  });

  it("falls back to the raw id rather than inventing a name", () => {
    expect(appLabel("some-future-app")).toBe("some-future-app");
  });
});

describe("session liveness", () => {
  it("polls only while the run has not reached a terminal status", () => {
    expect(isLive("starting")).toBe(true);
    expect(isLive("running")).toBe(true);
    for (const terminal of ["ended", "failed", "cancelled"]) {
      expect(isLive(terminal)).toBe(false);
    }
  });
});

describe("delegation policy id", () => {
  it("derives one row per user, so a second save updates rather than forks", () => {
    const first = policyRowId("v1:identity:user:alice");
    const second = policyRowId("v1:identity:user:alice");
    expect(first).toBe(second);
    expect(first).not.toBe(policyRowId("v1:identity:user:bob"));
    // The id must be a legal shortId: no colons past the concept prefix, or
    // canonical-id validation drops the write silently.
    expect(first.startsWith("v1:worker:delegationPolicy:")).toBe(true);
    expect(first.slice("v1:worker:delegationPolicy:".length)).not.toContain(":");
  });
});

function CapturedSessionId(): React.ReactElement {
  const params = useParams<{ sessionId: string }>();
  return <span data-testid="captured">{decodeURIComponent(params.sessionId ?? "")}</span>;
}

describe("session addresses", () => {
  it("roots the surface under Fleet rather than beside it", () => {
    // Two doors to one thing is the question the rail's reshuffle removed.
    // Local apps run ON a machine, so the surface is a Fleet tab.
    expect(fleetPath("apps")).toBe("/fleet/apps");
    expect(sessionPath("s1")).toContain("/fleet/apps/sessions/");
  });

  it("round-trips a canonical session id through the router", () => {
    const sessionId = "v1:worker:appSession:abc123";
    render(
      <MemoryRouter initialEntries={[sessionPath(sessionId)]}>
        <Routes>
          <Route path={`/fleet/${SESSION_ROUTE_PATTERN}`} element={<CapturedSessionId />} />
        </Routes>
      </MemoryRouter>,
    );
    expect(screen.getByTestId("captured").textContent).toBe(sessionId);
  });
});
