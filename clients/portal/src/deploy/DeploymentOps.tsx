import { useState, type ReactNode } from "react";

import { Button, ConfirmDialog, DataText, Field, TextInput } from "../ui";
import type { DeployConsoleState } from "./useDeployConsole";

// The deploy VERBS, with the confirmations they have always needed
// (memql#4264).
//
// # Why this is a component and not a band on the view
//
// It renders inside the Deployments view's Ship band, but it cannot live in
// src/views/: portal_view_composition_test.go forbids iteration there and the
// guard is right to. This module is the same shape src/people/PersonActions.tsx
// took for the Users view's verbs -- the view supplies the slot, the verbs
// live outside it.
//
// # What consolidating actually fixed
//
// There were two deploy surfaces: this view's Ship band and a separate Cluster
// ops page. They were not merely redundant -- they DISAGREED about how
// dangerous these actions are. The view's buttons fired `ship` and `rollBack`
// on a single click with no confirmation; the ops page confirmed every one of
// them and made repair type-to-confirm. An operator's protection depended on
// which of two doors they had walked through, and the rail offered both.
//
// One surface, and it is the careful one. Every verb states what will happen
// before it happens, and the progress afterwards is the deployment RECORD's
// status -- the History band below is graph state re-read live, not a
// client-side guess.
//
// Repair (memql#4209) is the cluster-side half of the extension's repair: the
// identity node asks ArgoCD to hard-refresh and re-sync this installation's
// Application from the committed overlay (prune included) and watches it until
// it is synced and healthy. Nothing changes version. The host-side half --
// tools, hosts file, local CA, checkout, the k3d cluster itself -- is not
// reachable from inside the cluster and stays with the VS Code extension. This
// surface does not shell around its own wire contract.

// The phrase an owner types to arm repair. Lower-case, the verb itself: the
// point is a deliberate keystroke, not a password.
const REPAIR_CONFIRM_PHRASE = "repair";

type Pending =
  | { kind: "cut"; bump: "patch" | "minor" }
  | { kind: "ship"; deploymentId: string }
  | { kind: "rollback"; deploymentId: string }
  | { kind: "repair" }
  | null;

export function DeploymentOps({
  console_,
  selectedRowId,
  canRollBackToSelection,
}: {
  console_: DeployConsoleState;
  // The deployment the view has selected. Deploy and roll back act on it, and
  // are disabled without one -- acting on "whatever was last clicked" is how
  // the wrong version ships.
  selectedRowId: string;
  // Whether the selection is a legitimate rollback target. The view computes
  // it, because the view holds the rows; see isRollbackTarget there for why a
  // repair record and an unsucceeded one are not.
  canRollBackToSelection: boolean;
}): ReactNode {
  const [pending, setPending] = useState<Pending>(null);
  const [repairPhrase, setRepairPhrase] = useState("");
  const { permissions, busy } = console_;

  const closeRepair = (): void => {
    setPending(null);
    setRepairPhrase("");
  };

  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
        {permissions.canShip ? (
          <>
            <Button size="xs" onClick={() => setPending({ kind: "cut", bump: "patch" })} disabled={busy}>
              Cut a patch version
            </Button>
            <Button size="xs" onClick={() => setPending({ kind: "cut", bump: "minor" })} disabled={busy}>
              Cut a minor version
            </Button>
            <Button
              size="xs"
              onClick={() => setPending({ kind: "ship", deploymentId: selectedRowId })}
              disabled={busy || selectedRowId === ""}
            >
              Deploy the selected version
            </Button>
          </>
        ) : null}
        {permissions.canRollBack ? (
          <Button
            size="xs"
            tone="danger"
            onClick={() => setPending({ kind: "rollback", deploymentId: selectedRowId })}
            disabled={busy || !canRollBackToSelection}
          >
            Roll back to the selected version
          </Button>
        ) : null}
        {permissions.canRepair ? (
          <Button size="xs" tone="danger" onClick={() => setPending({ kind: "repair" })} disabled={busy}>
            Repair this installation
          </Button>
        ) : null}
      </div>

      {permissions.canRepair ? (
        <p className="mt-2 text-xs text-subtle">
          Repair re-converges this installation onto its committed overlay: ArgoCD re-fetches the
          manifests and syncs the application, pruning tracked resources Git no longer describes.
          Nothing changes version. Progress lands on the history below as a repair record.
        </p>
      ) : null}

      <ConfirmDialog
        open={pending?.kind === "cut"}
        title={pending?.kind === "cut" && pending.bump === "minor" ? "Cut a minor version?" : "Cut a patch version?"}
        confirmLabel="Cut the version"
        busy={busy}
        onConfirm={() => {
          if (pending?.kind === "cut") console_.cut(pending.bump);
          setPending(null);
        }}
        onCancel={() => setPending(null)}
      >
        Records a new deployment at the next version. Nothing rolls until that deployment is
        deployed from the history below; the record&rsquo;s status is the progress you will see.
      </ConfirmDialog>

      <ConfirmDialog
        open={pending?.kind === "ship"}
        title="Deploy this version?"
        confirmLabel="Deploy"
        busy={busy}
        onConfirm={() => {
          if (pending?.kind === "ship") console_.ship(pending.deploymentId);
          setPending(null);
        }}
        onCancel={() => setPending(null)}
      >
        Rolls every node type to{" "}
        <DataText kind="id">{pending?.kind === "ship" ? pending.deploymentId : ""}</DataText>{" "}
        through the GitOps path. Progress and failure land on the deployment record below as its
        status changes.
      </ConfirmDialog>

      <ConfirmDialog
        open={pending?.kind === "rollback"}
        title="Roll back to this deployment?"
        confirmLabel="Roll back"
        tone="danger"
        busy={busy}
        onConfirm={() => {
          if (pending?.kind === "rollback") console_.rollBack(pending.deploymentId);
          setPending(null);
        }}
        onCancel={() => setPending(null)}
      >
        Owner-only, enforced against you personally on the identity node. Re-pins the cluster to{" "}
        <DataText kind="id">{pending?.kind === "rollback" ? pending.deploymentId : ""}</DataText>{" "}
        and supersedes the current deployment; the history records both sides of the move.
      </ConfirmDialog>

      <ConfirmDialog
        open={pending?.kind === "repair"}
        title="Repair this installation?"
        confirmLabel="Repair"
        tone="danger"
        busy={busy}
        confirmDisabled={repairPhrase.trim() !== REPAIR_CONFIRM_PHRASE}
        onConfirm={() => {
          if (pending?.kind === "repair") console_.repair();
          closeRepair();
        }}
        onCancel={closeRepair}
      >
        <p>
          Owner-only, enforced against you personally on the identity node. ArgoCD re-fetches the
          committed manifests and syncs this installation&rsquo;s application, pruning tracked
          resources Git no longer describes; drifted or deleted workloads are re-applied. Nothing
          changes version. A repair record lands on the history at in_progress and is resolved to
          succeeded or failed from what the node observes &mdash; not from this button.
        </p>
        <div className="mt-3">
          <Field label={`Type "${REPAIR_CONFIRM_PHRASE}" to confirm`}>
            <TextInput
              value={repairPhrase}
              onChange={setRepairPhrase}
              placeholder={REPAIR_CONFIRM_PHRASE}
            />
          </Field>
        </div>
      </ConfirmDialog>
    </>
  );
}
