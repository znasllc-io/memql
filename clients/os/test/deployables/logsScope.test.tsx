import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

const h = vi.hoisted(() => ({ connection: null as unknown }));
vi.mock("../../src/live/connection", () => ({ useOsConnection: () => h.connection }));

import { DeployablesApp } from "../../src/apps/deployables/DeployablesApp";
import { LocalDeployablesSettingsStore } from "../../src/apps/deployables/settings";
import { builtinReply, fakeConnection, withSession } from "./harness";

// The Deployables Logs section (epic memql#4895) reads the app's slice: lines
// tagged with the app, or about a concept the app owns. A pipeline step's
// output is about its v1:pipelines:run (epic memql#5478, issue memql#5495) and
// carries no app tag, so the run's concept is what puts a step's lines in this
// section beside the deployments they lead to.

/** One line of a step's output, as the step's capture stores it. */
const STEP_LINE: Row = {
  id: "l-step-1",
  occurredAt: new Date(Date.now() - 2_000).toISOString(),
  nodeType: "workbench",
  node: "workbench-0",
  level: "info",
  component: "pipelines.step",
  app: "",
  message: "PASS tests.unit (0.41s)",
  attributes: { stepKey: "tests.unit", workRunId: "work-7" },
  subject: "run-7",
  subjectConcept: "v1:pipelines:run",
  session: "",
  userId: "",
};

describe("the Deployables Logs section", () => {
  it("reads a pipeline run's step lines with the rest of the app's slice", async () => {
    const connection = fakeConnection({});
    const original = vi.mocked(connection.query.executeNamed).getMockImplementation()!;
    const tails: string[] = [];
    // The log store answers the tail; everything else is the harness's.
    vi.spyOn(connection.query, "executeNamed").mockImplementation(async (name, call, options) => {
      if (!call.startsWith("builtin logsTail(")) return original(name, call, options);
      tails.push(call);
      return builtinReply("logsTail", [STEP_LINE]);
    });
    h.connection = connection;
    const store = new LocalDeployablesSettingsStore({ getItem: () => null, setItem: () => {} });
    render(
      withSession(
        <DeployablesApp
          intent={undefined}
          consumeIntent={vi.fn()}
          sectionId="logs"
          navigation={{ origin: "peer", revision: 0 }}
          windowVisible
          navigate={vi.fn()}
          askContext={vi.fn()}
          store={store}
        />,
        { role: "owner", userId: "u-me" },
      ),
    );

    expect(await screen.findByText("PASS tests.unit (0.41s)")).toBeTruthy();
    expect(tails[0]).toContain('apps: ["deployables"]');
    expect(tails[0]).toContain('"v1:pipelines:run"');
    // The line keeps its subject, so a person can narrow to the one run.
    expect(screen.getByRole("button", { name: "Narrow to run run-7" })).toBeTruthy();
  });
});
