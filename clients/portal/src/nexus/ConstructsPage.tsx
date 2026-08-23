import { useState, type ReactNode } from "react";

import { Badge, Band, Button, Callout, ConfirmDialog, DataText, Panel } from "../ui";
import { useCluster } from "../cluster/ClusterProvider";
import { DependencyGraph } from "./constructs/DependencyGraph";
import { useGoalContext } from "./GoalLayout";
import type { ConstructRow } from "./scene/world";

// /nexus/:planId/constructs -- what the goal built.
//
// The Map answers "what is happening" with motion. This page answers "what
// did it build" with reading: the bundle and its status, each construct with
// its SOURCE, and the dependency graph flat enough to follow.
//
// ===========================================================================
// TWO VERBS, AND ONLY THE TWO THE ENGINE ALREADY HAS
// ===========================================================================
// Stage (`setConstructStatus` to `staged`) and promote
// (`activateAuthoringBundle`). Both behind ConfirmDialog, both through the
// existing owner-gated mutations, and neither invented here.
//
// The runnable actions from the construct catalog are epic memql#4274's and
// are deliberately absent: this page uses the verbs that exist today rather
// than anticipating a surface that does not.
//
// ===========================================================================
// THE HONEST BANNER
// ===========================================================================
// Runtime authoring is gated by MEMQL_AUTHORING_CAPTURE_MODE=author and is
// OFF BY DEFAULT (dsl/authoring). A goal on a cluster with capture off
// produces no bundle at all -- so this page would be empty, and an empty page
// reads as "the goal built nothing", which is a claim about the GOAL rather
// than about the cluster.
//
// The banner is INFERRED rather than read from configuration, and the
// inference is stated so a reader can judge it: a goal that SUCCEEDED and
// left no bundle is the signature of capture being off, because a succeeded
// goal with capture on writes a bundle even when the bundle is empty. A goal
// still running has simply not got there yet, and says nothing. That
// inference is the honest maximum available to a browser: the setting is a
// node's environment variable, and the portal is served by the edge, whose
// runtime-config document is cluster-wide identity discovery for EVERY
// hosted site rather than a place to publish one application's feature flags
// (component/edge/runtimeconfig.go, and the guard that keeps it generic).

function toneForBundleStatus(status: string): "ok" | "warn" | "danger" | "neutral" {
  switch (status) {
    case "active":
      return "ok";
    case "failed":
      return "danger";
    case "validated":
    case "dryRunPassed":
      return "warn";
    default:
      return "neutral";
  }
}

function toneForConstructStatus(status: string): "ok" | "warn" | "danger" | "neutral" {
  switch (status) {
    case "active":
      return "ok";
    case "staged":
      return "warn";
    case "retired":
      return "danger";
    default:
      return "neutral";
  }
}

function Report({ title, report }: { title: string; report: Record<string, unknown> | null }): ReactNode {
  if (report === null) return null;
  return (
    <div className="min-w-64">
      <div className="text-xs font-semibold tracking-wide text-muted uppercase">{title}</div>
      <pre className="mt-1 max-h-40 overflow-auto rounded border border-line bg-raised p-2 text-xs">
        {JSON.stringify(report, null, 2)}
      </pre>
    </div>
  );
}

export function ConstructsPage(): ReactNode {
  const { world } = useGoalContext();
  const { query } = useCluster();
  const [pending, setPending] = useState<
    { verb: "stage"; construct: ConstructRow } | { verb: "promote" } | null
  >(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [done, setDone] = useState("");

  const bundle = world.bundle;
  const captureLikelyOff = bundle === null && world.plan?.status === "succeeded";

  function run(): void {
    if (query === null || pending === null) return;
    setBusy(true);
    setError("");
    const call =
      pending.verb === "stage"
        ? query.setConstructStatus({ constructId: pending.construct.id, status: "staged" })
        : query.activateAuthoringBundle({ bundleId: bundle?.id ?? "" });
    void call
      .then(() => {
        setDone(
          pending.verb === "stage"
            ? `Staged ${pending.construct.name}.`
            : "Promoted the bundle. Its constructs are live.",
        );
        // No reload: the feed is subscribed to both concepts and the status
        // arrives through the same path every other change does (design 5,
        // "the status updates live through the feed"). Re-reading here would
        // be a second answer racing the first.
        setPending(null);
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setBusy(false));
  }

  if (bundle === null) {
    return (
      <div className="flex flex-col gap-4">
        {captureLikelyOff ? (
          <Callout tone="neutral" title="This goal produced no constructs because authoring capture is off">
            Runtime authoring is gated by <DataText kind="string">MEMQL_AUTHORING_CAPTURE_MODE</DataText>
            {" "}and is off by default, so a goal on this cluster does its work without recording the
            queries, mutations and automations it would have authored. The goal itself succeeded --
            nothing here is a failure. See docs/public/language/training.md for how the authoring
            loop works and what turning it on changes.
          </Callout>
        ) : (
          <Callout tone="neutral" title="No bundle yet">
            This goal has not authored anything. A bundle appears when the goal reaches the point
            of writing constructs -- or never, if this cluster has authoring capture off.
          </Callout>
        )}
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-6">
      <Band title="Bundle" headingLevel="h2">
        <Panel>
          <div className="flex flex-col gap-3">
            <div className="flex flex-wrap items-center gap-3">
              <span className="font-medium">{bundle.title === "" ? bundle.id : bundle.title}</span>
              <Badge tone={toneForBundleStatus(bundle.status)}>{bundle.status}</Badge>
              <DataText kind="id">{bundle.id}</DataText>
              <div className="ml-auto">
                <Button
                  size="xs"
                  tone="primary"
                  disabled={bundle.status === "active" || query === null}
                  onClick={() => setPending({ verb: "promote" })}
                >
                  Promote
                </Button>
              </div>
            </div>
            {bundle.summary === "" ? null : <p className="text-sm text-muted">{bundle.summary}</p>}
            {bundle.failureReason === "" ? null : (
              <Callout tone="danger" title="The bundle failed">
                {bundle.failureReason}
              </Callout>
            )}
            <div className="flex flex-wrap gap-6">
              <Report title="Validation" report={bundle.validationReport} />
              <Report title="Dry run" report={bundle.dryRunReport} />
            </div>
          </div>
        </Panel>
      </Band>

      {error === "" ? null : (
        <Callout tone="danger" title="The write was refused">
          {error}
        </Callout>
      )}
      {done === "" ? null : (
        <Callout tone="ok" title="Done">
          {done}
        </Callout>
      )}

      <Band title={`Constructs (${world.constructs.length})`} headingLevel="h2">
        <div className="flex flex-col gap-3">
          {world.constructs.length === 0 ? (
            <p className="text-sm text-muted">This bundle holds no constructs.</p>
          ) : (
            world.constructs.map((construct) => (
              <Panel key={construct.id}>
                <div className="flex flex-col gap-2">
                  <div className="flex flex-wrap items-center gap-3">
                    <span className="text-xs tracking-wide text-muted uppercase">{construct.kind}</span>
                    <span className="font-medium">{construct.name}</span>
                    <Badge tone={toneForConstructStatus(construct.status)}>{construct.status}</Badge>
                    {construct.targetNamespace === "" ? null : (
                      <DataText kind="string">{construct.targetNamespace}</DataText>
                    )}
                    <div className="ml-auto">
                      <Button
                        size="xs"
                        disabled={construct.status !== "draft" || query === null}
                        onClick={() => setPending({ verb: "stage", construct })}
                      >
                        Stage
                      </Button>
                    </div>
                  </div>
                  {/* Read-only, and read-only on purpose: editing a construct
                      is the authoring loop's job, not a text area in a
                      console that could not validate what it wrote. */}
                  <pre className="max-h-64 overflow-auto rounded border border-line bg-raised p-2 font-mono text-xs">
                    {construct.source}
                  </pre>
                </div>
              </Panel>
            ))
          )}
        </div>
      </Band>

      <Band title="Dependencies" headingLevel="h2">
        <Panel>
          <DependencyGraph constructs={world.constructs} edges={world.edges} />
        </Panel>
      </Band>

      <ConfirmDialog
        open={pending !== null}
        title={pending?.verb === "stage" ? "Stage this construct?" : "Promote this bundle?"}
        confirmLabel={pending?.verb === "stage" ? "Stage" : "Promote"}
        busy={busy}
        onConfirm={run}
        onCancel={() => setPending(null)}
      >
        {pending?.verb === "stage" ? (
          <>
            Staging moves <DataText kind="string">{pending.construct.name}</DataText> out of draft
            into the owner-scoped tier. It becomes reachable to you, and to nobody else, until the
            bundle is promoted.
          </>
        ) : (
          <>
            Promoting activates the bundle and registers every construct in it against this
            cluster's running engine. Names it claims are claimed for everyone.
          </>
        )}
      </ConfirmDialog>
    </div>
  );
}
