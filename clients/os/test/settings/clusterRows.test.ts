import { describe, expect, it } from "vitest";

import {
  clusterFromRow,
  deploymentFromRow,
  latestDeployment,
  nodeSpecFromRow,
  providerFromRow,
  resolvedVersion,
  ridesTheSpine,
} from "../../src/apps/settings/clusterRows";
import { mailHeadline, mailTone, readMailStatus } from "../../src/apps/settings/mailStatus";

describe("cluster row projections (memql#4742)", () => {
  it("reads the cluster singleton, flattening a payload-nested row", () => {
    const c = clusterFromRow({
      id: "v1:cluster:cluster:abc",
      // `status` is DELIBERATELY still in the fixture payload: memql#4772
      // removed the concept field, and a row written before that removal still
      // carries the key. The projection must ignore it rather than surface it.
      payload: { name: "dev", region: "local", status: "healthy", version: "v1.2.3", provider: "docker-local" },
    });
    expect(c).toEqual({
      id: "v1:cluster:cluster:abc",
      name: "dev",
      region: "local",
      version: "v1.2.3",
      provider: "docker-local",
    });
  });

  it("picks the newest deployment by createdAt, not by reply order", () => {
    // deploymentsForCluster declares no sort and its own comment says
    // ordering is the consumer's job, so a panel that trusted reply order
    // would call whichever row came back first "current".
    const rows = [
      { deploymentId: "old", createdAt: "2026-01-01T00:00:00Z" },
      { deploymentId: "new", createdAt: "2026-06-01T00:00:00Z" },
      { deploymentId: "mid", createdAt: "2026-03-01T00:00:00Z" },
    ].map(deploymentFromRow);
    expect(latestDeployment(rows)?.deploymentId).toBe("new");
  });

  it("ignores a row with no deploymentId and answers null for an empty set", () => {
    expect(latestDeployment([])).toBeNull();
    expect(latestDeployment([deploymentFromRow({ createdAt: "2026-06-01T00:00:00Z" })])).toBeNull();
  });

  it("resolves an empty node-spec version against the deployment engine version", () => {
    // Engine-as-spine. The engine never fills this in -- the query's comment
    // names the consumer as responsible -- and rendering the raw empty
    // string shows "no version" for the NORMAL case, which reads as a
    // broken deployment.
    const unpinned = nodeSpecFromRow({ nodeType: "bff", version: "", replicas: 2 });
    expect(resolvedVersion(unpinned, "2026.6.21")).toBe("2026.6.21");
    expect(ridesTheSpine(unpinned)).toBe(true);

    const pinned = nodeSpecFromRow({ nodeType: "voice", version: "2026.5.1", replicas: 1 });
    expect(resolvedVersion(pinned, "2026.6.21")).toBe("2026.5.1");
    expect(ridesTheSpine(pinned)).toBe(false);
  });

  it("reads a provider's availability whether the wire sent a bool or a string", () => {
    expect(providerFromRow({ name: "a", available: true }).available).toBe(true);
    expect(providerFromRow({ name: "b", available: "true" }).available).toBe(true);
    expect(providerFromRow({ name: "c", available: false }).available).toBe(false);
    expect(providerFromRow({ name: "d" }).available).toBe(false);
  });
});

describe("the mail sender group", () => {
  const envelope = (email: Record<string, unknown>) => [
    {
      integrationStatus: {
        payload: {
          checkedAt: "2026-08-31T12:00:00Z",
          probed: false,
          integrations: [{ name: "storage" }, { name: "email", ...email }],
        },
      },
    },
  ];

  it("digs the report out of the builtin envelope by SEARCH, not a fixed path", () => {
    const status = readMailStatus(envelope({ mode: "graph", configured: "yes", health: "unknown" }));
    expect(status).toEqual({
      checkedAt: "2026-08-31T12:00:00Z",
      probed: false,
      mode: "graph",
      configured: "yes",
      health: "unknown",
      detail: "",
    });
  });

  it("renders log-only as degraded and never as healthy (memql#4477)", () => {
    const status = readMailStatus(
      envelope({ mode: "log", configured: "no", health: "degraded", detail: "No sender is configured" }),
    )!;
    expect(status.configured).toBe("no");
    expect(status.health).toBe("degraded");
    expect(mailTone(status)).toBe("unreachable");
    expect(mailHeadline(status)).toMatch(/delivered to nobody/);
    expect(mailHeadline(status)).not.toMatch(/healthy/i);
  });

  it("gives a configured sender the reachable dot only when it is healthy", () => {
    const healthy = readMailStatus(envelope({ mode: "smtp", health: "healthy" }))!;
    expect(mailTone(healthy)).toBe("reachable");
    // Configured but unprobed is "unknown" -- not a claim of health, and not
    // a claim of failure either.
    const unprobed = readMailStatus(envelope({ mode: "smtp", health: "unknown" }))!;
    expect(mailTone(unprobed)).toBe("off");
  });

  it("answers null when no email integration is reported", () => {
    expect(readMailStatus([{ integrationStatus: { payload: { integrations: [{ name: "storage" }] } } }])).toBeNull();
    expect(readMailStatus([])).toBeNull();
  });

  it("carries no credential slot map -- presence is reconnaissance too", () => {
    const status = readMailStatus(
      envelope({
        mode: "graph",
        credentials: [{ name: "clientSecret", present: true, envVar: "AZURE_CLIENT_SECRET" }],
      }),
    )!;
    expect(JSON.stringify(status)).not.toContain("clientSecret");
    expect(JSON.stringify(status)).not.toContain("AZURE_CLIENT_SECRET");
  });
});
