import { describe, expect, it } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { reportFromRow } from "../../src/live/readiness";

// A ROW WITH NO STATE IS UNKNOWN, NEVER UNCONFIGURED (the 2026-09-14
// readiness-convergence record, D1). The old default turned a malformed or
// partial row into a "not set up" vote, which is the one direction a default
// must never take: it sends somebody to a setup form over a row that said
// nothing.
describe("reportFromRow", () => {
  const base = {
    id: "v1:platform:moduleReadiness:ai--bff-a",
    module: "ai",
    nodeId: "bff-a",
    nodeType: "bff",
    core: true,
    lanes: [],
    reportedAt: "2026-09-13T11:58:00Z",
  };

  it("defaults a missing state to unknown", () => {
    const r = reportFromRow(base as unknown as Row);
    expect(r?.state).toBe("unknown");
    expect(r?.reason).toBeUndefined();
  });

  it("carries the reason of an unknown row", () => {
    const r = reportFromRow({ ...base, state: "unknown", reason: "fleetReadFailed" } as unknown as Row);
    expect(r?.state).toBe("unknown");
    expect(r?.reason).toBe("fleetReadFailed");
  });

  it("keeps a stated verdict as it is", () => {
    expect(reportFromRow({ ...base, state: "configured" } as unknown as Row)?.state).toBe("configured");
  });

  it("carries a lane's scope through untouched", () => {
    const r = reportFromRow({
      ...base,
      state: "configured",
      lanes: [{ name: "local", configurableFrom: "fleet", scope: "cluster", complete: true, slots: [] }],
    } as unknown as Row);
    expect(r?.lanes?.[0]?.scope).toBe("cluster");
  });

  it("drops a row with no module", () => {
    expect(reportFromRow({ ...base, module: "" } as unknown as Row)).toBeNull();
  });

  // THE DECLARATION RIDES THROUGH. A feed that dropped the two flags would
  // read the pipelines item as one somebody NEEDS, and nothing would say so --
  // false is a value, not an error.
  it("carries the module's optional and dismissable flags", () => {
    const r = reportFromRow({
      ...base,
      module: "pipelines",
      state: "notApplicable",
      core: false,
      optional: true,
      dismissable: true,
    } as unknown as Row);
    expect(r?.optional).toBe(true);
    expect(r?.dismissable).toBe(true);
    expect(r?.core).toBe(false);
  });

  // And a row written before the flags existed carries neither key, which
  // reads as the module having declared neither -- never as undefined.
  it("reads a row without the flags as declaring neither", () => {
    const r = reportFromRow({ ...base, state: "configured" } as unknown as Row);
    expect(r?.optional).toBe(false);
    expect(r?.dismissable).toBe(false);
  });
});
