import { useState, type ReactNode } from "react";
import { NexusApp } from "../src/apps/nexus/NexusApp";
import { LocalNexusSettingsStore } from "../src/apps/nexus/settings";
import {
  fakeConnection,
  goalRow,
  runRow,
  approvalRow,
  constructRow,
  procedureRow,
  ladderPolicyRow,
  withSession,
} from "../test/nexus/harness";
import { OsProvider } from "../src/chrome/state";
import { OS_REGISTRY } from "../src/apps/registry";
const noop = () => {};
const dates = (n: number) =>
  `2026-10-${String(n + 1).padStart(2, "0")}T12:00:00Z`;
function Scene({ section }: { section: string }) {
  const [current, setCurrent] = useState(section);
  const [store] = useState(() => new LocalNexusSettingsStore(null));
  return (
    <NexusApp
      sectionId={current}
      navigate={setCurrent}
      askContext={noop}
      store={store}
    />
  );
}
export const NEXUS_PAGED_VIEWS = Object.fromEntries(
  ["goals", "runs", "approvals", "automations"].map((section) => [
    `nexus-${section}-paged`,
    {
      connect: () =>
        fakeConnection({
          goals: Array.from({ length: 25 }, (_, n) =>
            goalRow({
              id: `goal-${n}`,
              statement:
                n === 0
                  ? "Find the ancient needle receipt"
                  : `Prepare the monthly ledger ${n}`,
              createdAt: dates(n),
            }),
          ),
          runs: Array.from({ length: 25 }, (_, n) =>
            runRow({
              id: `run-${n}`,
              goalId: `goal-${n}`,
              title:
                n === 0
                  ? "Find the ancient needle receipt"
                  : `Reconcile client payments ${n}`,
              status: n % 3 === 0 ? "waiting" : "running",
              waitingOn:
                n % 3 === 0
                  ? { kind: "approval", subject: `approval-${n}` }
                  : {},
              createdAt: dates(n),
            }),
          ),
          approvals: Array.from({ length: 25 }, (_, n) =>
            approvalRow({
              id: `approval-${n}`,
              runId: `run-${n}`,
              question:
                n === 24
                  ? "Send the ancient needle receipt?"
                  : `Approve the monthly report ${n}?`,
              createdAt: dates(n),
              requestedAt: dates(n),
            }),
          ),
          constructs: Array.from({ length: 13 }, (_, n) =>
            constructRow({
              id: `automation-${n}`,
              name:
                n === 0
                  ? "Ancient needle reconciliation"
                  : `Monthly reconciliation ${n}`,
              createdAt: dates(n * 2),
            }),
          ),
          learnedProcedures: Array.from({ length: 12 }, (_, n) =>
            procedureRow({ id: `procedure-${n}`, createdAt: dates(n * 2 + 1) }),
          ),
          ladderPolicy: [ladderPolicyRow()],
        }),
      wrap: (el: ReactNode) =>
        withSession(
          <OsProvider
            registry={OS_REGISTRY}
            actorRole="owner"
            grid={{ cols: 8, rows: 5 }}
          >
            <div className="os-window-content" style={{ height: "100dvh" }}>
              {el}
            </div>
          </OsProvider>,
        ),
      render: () => <Scene section={section} />,
    },
  ]),
);
