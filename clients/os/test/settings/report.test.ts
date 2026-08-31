import { describe, expect, it } from "vitest";

import {
  buildDiagnosticsReport,
  type DiagnosticsInput,
} from "../../src/apps/settings/buildDiagnosticsReport";
import { EMPTY_HISTORY, recordTransition } from "../../src/apps/settings/connectionHistory";

// Placeholders only, per the task: example.com and owner@example.com.
const BASE: DiagnosticsInput = {
  at: Date.UTC(2026, 7, 31, 12, 0, 0),
  domain: "example.com",
  build: "0.1.0",
  endpoint: "wss://os.example.com/_memql/ws",
  userId: "v1:identity:user:abc123",
  primaryEmail: "owner@example.com",
  clusterRole: "owner",
  connection: EMPTY_HISTORY,
  connectionStatus: "connected",
  themePack: "graphite",
  mode: "system",
  reducedMotion: false,
  admittedApps: ["Artifacts", "Settings"],
  hidden: [],
  cluster: { state: "not-admitted" },
};

describe("the diagnostics report (memql#4744)", () => {
  it("is deterministic given fixed inputs", () => {
    expect(buildDiagnosticsReport(BASE)).toMatchInlineSnapshot(`
      "MemQL OS -- diagnostics
      Generated: 2026-08-31T12:00:00.000Z

      Session
        Cluster domain:   example.com
        Shell build:      0.1.0
        Signed in as:     owner@example.com
        User id:          v1:identity:user:abc123
        Cluster role:     owner

      Connection
        Status:           connected
        Endpoint:         wss://os.example.com/_memql/ws
        Last reconnect:   none in this session
        Transitions:      none recorded

      Appearance
        Theme:            graphite
        Mode:             system
        Reduced motion:   off

      Apps you can open
        Artifacts
        Settings

      Hidden from this session (presentation gating; row authz is the engine's)
        nothing hidden

      Cluster facts
        not admitted
      "
    `);
  });

  it("is plain text -- no markup anywhere", () => {
    const history = recordTransition(
      recordTransition(EMPTY_HISTORY, { status: "connected", attempt: 0, error: "" }, 1000),
      { status: "reconnecting", attempt: 2, error: "stream closed" },
      2000,
    );
    const report = buildDiagnosticsReport({
      ...BASE,
      connection: history,
      hidden: [{ kind: "app", label: "Users", requires: "admin" }],
      cluster: { state: "facts", lines: [["Cluster", "dev"]] },
    });
    // Plain text: no HTML, and none of the markdown constructs a paste
    // target would re-render. An underscore is NOT one of them -- the WS
    // endpoint is legitimately `/_memql/ws`, and banning the character
    // would make this assertion about the fixture rather than the format.
    expect(report).not.toMatch(/<[a-z/]/i);
    expect(report).not.toMatch(/\*\*|`|^#|^\s*[-*+] |\|/m);
  });

  it("tells 'not admitted' apart from 'the read failed'", () => {
    expect(buildDiagnosticsReport({ ...BASE, cluster: { state: "not-admitted" } })).toContain(
      "not admitted",
    );
    const failed = buildDiagnosticsReport({
      ...BASE,
      cluster: { state: "unavailable", reason: "providerAuthStatus is owner-only" },
    });
    expect(failed).toContain("unavailable -- providerAuthStatus is owner-only");
    expect(failed).not.toContain("not admitted");
  });

  it("names the hidden surfaces and what each needs", () => {
    const report = buildDiagnosticsReport({
      ...BASE,
      clusterRole: "writer",
      hidden: [
        { kind: "app", label: "Users", requires: "admin" },
        { kind: "section", label: "Settings -- Cluster", requires: "admin" },
      ],
    });
    expect(report).toContain("Users (app) -- requires admin");
    expect(report).toContain("Settings -- Cluster (section) -- requires admin");
  });

  it("carries no token, credential or foreign address", () => {
    // Planted values, deliberately long and distinctive -- the same method
    // the server-side status_test uses, and for the same reason: a short
    // planted value makes the sweep vacuous.
    const PLANTED = [
      "mql_pat_PLANTED_BEARER_DO_NOT_EMIT_0000000000",
      "PLANTED-PROVIDER-SECRET-DO-NOT-EMIT",
      "someone.else@other.example.com",
    ];
    const report = buildDiagnosticsReport({
      ...BASE,
      // Every field a caller controls gets a planted value nowhere near it,
      // so a field the builder started echoing would show up here.
      cluster: {
        state: "facts",
        lines: [
          ["Cluster", "dev"],
          ["Mail sender", "graph"],
        ],
      },
    });
    for (const planted of PLANTED) {
      expect(report).not.toContain(planted);
    }
    // The negative control: the sweep can see a value when one IS present,
    // so its silence above is evidence rather than an empty grep.
    const leaky = buildDiagnosticsReport({
      ...BASE,
      cluster: { state: "facts", lines: [["Token", PLANTED[0]!]] },
    });
    expect(leaky).toContain(PLANTED[0]!);
  });

  it("keeps the session's own address and no other", () => {
    const report = buildDiagnosticsReport(BASE);
    expect(report).toContain("owner@example.com");
    expect(report.match(/@/g)).toHaveLength(1);
  });
});
