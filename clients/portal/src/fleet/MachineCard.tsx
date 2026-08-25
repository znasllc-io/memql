import { useRef, useState, type ReactNode } from "react";

import {
  Badge,
  Button,
  ConfirmDialog,
  DataText,
  Field,
  LabelChips,
  Panel,
  StatusDot,
  TextInput,
} from "../ui";
import { formatFreshness, formatMoment } from "./format";
import { chipsFromMap, mapFromChips, parseLabelChip, type MergedLabel } from "./labels";
import { MachineActivity } from "./MachineActivity";
import { isWorkerOnline, ONLINE_WINDOW_SECONDS } from "./online";
import type { Machine, MachineApp } from "./rows";

// One machine.
//
// ===========================================================================
// THE ONLINE DOT IS DERIVED, AND THE HEADER OF online.ts SAYS WHY
// ===========================================================================
// Short version: the stream this browser is on belongs to ONE replica, and a
// machine is online if ANY replica holds it. So the dot comes from the row's
// own lastSeenAt against a shared window, not from whether we happen to be
// talking to the node that has it.
//
// ===========================================================================
// LABELS: WHAT IS EDITABLE AND WHAT IS NOT
// ===========================================================================
// The list shown is the MERGE the router matches on, so it agrees with the
// routing an operator is looking at it to understand. Only the operator's own
// half is editable, and a value that overrode a reported one is marked, because
// that is the case where the machine is reporting one thing and the routing is
// acting on another.
//
// The reported half is not editable HERE and could not usefully be: the whole
// `labels` map is overwritten from the Register message on every reconnect, so
// an edit would survive until the machine next restarts and then vanish.

export function MachineCard({
  machine,
  now,
  showOwner,
  busy,
  onRename,
  onSetLabels,
  onRevoke,
}: {
  machine: Machine;
  // Passed in rather than read here: every card on the page must decide
  // online-ness against the SAME instant, or two machines with identical
  // lastSeenAt can render differently.
  now: Date;
  // The all-machines view adds an owner column. Off for a person's own list,
  // where every row has the same answer.
  showOwner: boolean;
  busy: boolean;
  onRename: (displayName: string) => Promise<boolean>;
  onSetLabels: (labels: Record<string, string>) => Promise<boolean>;
  onRevoke: (reason: string) => Promise<boolean>;
}): ReactNode {
  const revoked = machine.revokedAt.trim() !== "";
  const online = isWorkerOnline(machine.lastSeenAt, machine.revokedAt, now);

  const [renaming, setRenaming] = useState(false);
  const [nameDraft, setNameDraft] = useState(machine.displayName || machine.name);
  const [confirming, setConfirming] = useState(false);
  const [revokeReason, setRevokeReason] = useState("");
  const [expanded, setExpanded] = useState(false);
  const [chipError, setChipError] = useState("");

  // The operator labels, optimistically. Re-seeded when the ROW's value
  // changes -- the subscription carrying the write back -- and rolled back when
  // a write is refused. Adjusting state during render rather than in an effect,
  // the same choice RoutingPolicyEditor makes and for the same reason.
  const rowChips = chipsFromMap(machine.operatorLabels);
  // The separator is an ESCAPE, not a literal NUL. Written as a raw byte it
  // is the same string at runtime and makes this file BINARY to every tool
  // that reads the tree -- grep skips it, git diffs it as "Binary files
  // differ", and a search for anything in this component silently finds
  // nothing. It is a separator that cannot appear in a label, which is the
  // only property being asked of it.
  const rowKey = rowChips.join("\u0000");
  const seededFrom = useRef(rowKey);
  const [chips, setChips] = useState<string[]>(rowChips);
  if (seededFrom.current !== rowKey) {
    seededFrom.current = rowKey;
    setChips(rowChips);
  }

  function commitLabels(next: string[]): void {
    const before = chips;
    setChips(next);
    void onSetLabels(mapFromChips(next)).then((ok) => {
      // The write did not happen, so the chip must not keep claiming it did.
      if (!ok) setChips(before);
    });
  }

  function addChip(text: string): void {
    if (parseLabelChip(text) === null) {
      setChipError(`"${text}" is not a label. Write it as key=value, for example role=render.`);
      return;
    }
    setChipError("");
    commitLabels([...chips, text]);
  }

  function commitRename(): void {
    const next = nameDraft.trim();
    if (next === "") return;
    void onRename(next).then((ok) => {
      if (ok) setRenaming(false);
    });
  }

  return (
    <Panel>
      <div className="flex flex-col gap-3">
        {/* Identity */}
        <div className="flex flex-wrap items-center gap-2">
          <StatusDot
            tone={revoked ? "danger" : online ? "ok" : "neutral"}
            label={revoked ? "Revoked" : online ? "Online" : "Offline"}
          />
          {renaming ? (
            <span className="flex flex-wrap items-center gap-2">
              <span className="w-56">
                <TextInput value={nameDraft} onChange={setNameDraft} placeholder={machine.name} />
              </span>
              <Button
                size="xs"
                tone="primary"
                busy={busy}
                busyLabel="Saving…"
                disabled={nameDraft.trim() === ""}
                onClick={commitRename}
              >
                Save name
              </Button>
              <Button
                size="xs"
                onClick={() => {
                  setNameDraft(machine.displayName || machine.name);
                  setRenaming(false);
                }}
              >
                Cancel
              </Button>
            </span>
          ) : (
            <span className="flex flex-wrap items-center gap-2">
              <h3 className="text-sm font-semibold break-all">{machine.label}</h3>
              <Button size="xs" onClick={() => setRenaming(true)}>
                Rename
              </Button>
            </span>
          )}

          <span className="ml-auto flex flex-wrap items-center gap-1.5">
            {revoked ? <Badge tone="danger">revoked</Badge> : null}
            {machine.buildTag === "" ? null : <Badge>{machine.buildTag}</Badge>}
            {machine.capabilities.map((one) => (
              <Badge key={one}>{one}</Badge>
            ))}
          </span>
        </div>

        {machine.displayName.trim() === "" ? null : (
          <p className="text-xs text-subtle">
            The machine reports itself as <DataText kind="string">{machine.name}</DataText>. That
            name is re-stamped on every reconnect, which is why yours is kept separately.
          </p>
        )}

        {/* Readings */}
        <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-0.5 text-xs sm:grid-cols-[max-content_1fr_max-content_1fr]">
          {showOwner ? <Reading label="Owner" value={machine.ownerUserId || "--"} kind="id" /> : null}
          <Reading
            label="Platform"
            value={
              [machine.os, machine.arch].filter((part) => part !== "").join(" / ") || "not reported"
            }
          />
          <Reading label="Host" value={machine.hostname || "not reported"} kind="id" />
          <Reading
            label="Display server"
            value={machine.displayServer || "not reported"}
            hint={
              machine.displayServer === ""
                ? "This machine's build predates the capability descriptor."
                : undefined
            }
          />
          <Reading label="Version" value={machine.version || "not reported"} />
          <Reading
            label="Last seen"
            value={formatFreshness(machine.lastSeenAt, now)}
            kind="time"
            hint={
              machine.lastSeenAt === ""
                ? "This machine has never sent a heartbeat."
                : `${formatMoment(machine.lastSeenAt)}. Online means a heartbeat within ${ONLINE_WINDOW_SECONDS} seconds.`
            }
          />
          <Reading
            label="Connected replica"
            value={machine.connectedNodeId || "none"}
            kind="id"
            hint={
              machine.connectedNodeId === ""
                ? "No replica is holding this machine's stream."
                : "The node holding this machine's stream. A dispatch from any other replica is forwarded to it."
            }
          />
          <Reading
            label="In flight"
            value={`${machine.activeCount}${concurrencySuffix(machine.concurrency)}`}
            kind="number"
            hint="As of this machine's most recent heartbeat, so up to one interval stale. It is a routing input, never a correctness one -- the machine's own admission is the real valve."
          />
          <Reading label="Registered" value={formatMoment(machine.registeredAt)} kind="time" />
          {machine.lastSelectedAt === "" ? null : (
            <Reading
              label="Last chosen"
              value={formatFreshness(machine.lastSelectedAt, now)}
              kind="time"
              hint="What roundRobin orders on."
            />
          )}
        </dl>

        {revoked ? (
          <p className="text-xs text-danger">
            Revoked {formatMoment(machine.revokedAt)}
            {machine.revokeReason === "" ? "" : ` -- ${machine.revokeReason}`}. Dispatch refuses,
            and the machine&rsquo;s stream is dropped at its next health check.
          </p>
        ) : null}

        {/* Labels */}
        <div>
          <p className="text-xs font-medium text-muted">
            Labels the router matches on
          </p>
          <MergedLabelList labels={machine.mergedLabels} />
          <p className="mt-2 text-xs font-medium text-muted">Your labels</p>
          <p className="mt-0.5 mb-1.5 text-xs text-subtle">
            Written as key=value. These survive a reconnect and win over the machine&rsquo;s own.
          </p>
          {chipError === "" ? null : (
            <p role="alert" className="mb-1 text-xs text-danger">
              {chipError}
            </p>
          )}
          <LabelChips
            labels={chips}
            busy={busy}
            readOnly={revoked}
            onAdd={addChip}
            onRemove={(text) => commitLabels(chips.filter((one) => one !== text))}
          />
        </div>

        {/* Local apps (memql#4359). "Selectable" is the ENGINE's rule --
            a known id, allowed by this machine's own policy.yaml, AND
            signed in -- not the presence of the entry. A card that said
            otherwise would show a ready row for a machine the router will
            never pick, and the reader would go hunting a routing bug that
            is not there. */}
        <div className="border-t border-line pt-3">
          <h4 className="text-xs font-medium text-fg">Local apps</h4>
          <AppList apps={machine.apps} />
        </div>

        {/* Verbs */}
        <div className="flex flex-wrap items-center gap-2 border-t border-line pt-3">
          <Button size="xs" pressed={expanded} onClick={() => setExpanded((open) => !open)}>
            {expanded ? "Hide recent calls" : "Recent calls"}
          </Button>
          <Button
            size="xs"
            tone="danger"
            disabled={revoked}
            onClick={() => setConfirming(true)}
            title={revoked ? "Already revoked" : undefined}
          >
            Revoke
          </Button>
        </div>

        {expanded ? <MachineActivity workerId={machine.id} asOperator={showOwner} /> : null}
      </div>

      <ConfirmDialog
        open={confirming}
        title={`Revoke ${machine.label}`}
        confirmLabel="Revoke this machine"
        tone="danger"
        busy={busy}
        onCancel={() => setConfirming(false)}
        onConfirm={() => {
          setConfirming(false);
          void onRevoke(revokeReason);
          setRevokeReason("");
        }}
      >
        <p>
          Dispatch to this machine is refused from now on, and its stream is dropped at the next
          health check. The registration row stays -- it is the audit trail -- and the machine
          needs a fresh token to come back.
        </p>
        <div className="mt-3">
          <Field label="Why (optional)">
            <TextInput
              value={revokeReason}
              onChange={setRevokeReason}
              placeholder="Laptop returned"
            />
          </Field>
        </div>
      </ConfirmDialog>
    </Panel>
  );
}

function concurrencySuffix(concurrency: Record<string, number>): string {
  const entries = Object.entries(concurrency).sort(([a], [b]) => a.localeCompare(b));
  if (entries.length === 0) return "";
  return ` of ${entries.map(([key, value]) => `${value} ${key}`).join(", ")}`;
}

function MergedLabelList({ labels }: { labels: readonly MergedLabel[] }): ReactNode {
  if (labels.length === 0) {
    return (
      <p className="mt-0.5 text-xs text-subtle">
        None. Every machine is a candidate for work that requires no labels.
      </p>
    );
  }
  return (
    <ul className="mt-1 flex flex-wrap items-center gap-1.5">
      {labels.map((one) => (
        <li key={one.key}>
          <Badge tone={one.source === "operator" ? "ok" : "neutral"}>
            <span className="font-mono">
              {one.key}={one.value}
            </span>
            {one.source === "operator" ? (
              <span className="ml-1.5 text-subtle" title={
                one.overrides
                  ? "Yours, replacing the value this machine reported."
                  : "Yours."
              }>
                {one.overrides ? "yours, overriding" : "yours"}
              </span>
            ) : null}
          </Badge>
        </li>
      ))}
    </ul>
  );
}

function Reading({
  label,
  value,
  kind,
  hint,
}: {
  label: string;
  value: string;
  kind?: "id" | "time" | "number";
  hint?: string;
}): ReactNode {
  return (
    <>
      <dt className="text-subtle">{label}</dt>
      <dd className="min-w-0 break-words text-muted" {...(hint === undefined ? {} : { title: hint })}>
        {kind === undefined ? value : <DataText kind={kind}>{value}</DataText>}
      </dd>
    </>
  );
}

// AppList renders a machine's reported local apps, saying for each one
// whether a delegated run can actually land on it -- and when it cannot,
// which half is missing.
function AppList({ apps }: { apps: readonly MachineApp[] }): ReactNode {
  if (apps.length === 0) {
    return (
      <p className="mt-0.5 text-xs text-subtle">
        This machine reports no local apps. A cockpit older than the app
        protocol reports none, and so does one that found neither app on PATH.
      </p>
    );
  }
  return (
    <ul className="mt-1 flex flex-col gap-1">
      {apps.map((app) => (
        <li key={app.id} className="flex flex-wrap items-center gap-2 text-xs">
          <span className="font-medium text-fg">{app.label}</span>
          <span className="text-subtle">{app.version || "version unknown"}</span>
          <Badge tone={app.runnable ? "ok" : "neutral"}>
            {app.runnable ? "selectable" : "not selectable"}
          </Badge>
          {app.why === "" ? null : <span className="text-subtle">{app.why}</span>}
          {app.subscription === "present" ? <Badge tone="ok">subscription</Badge> : null}
          {app.subscription === "unknown" ? (
            <span className="text-subtle">subscription unreported</span>
          ) : null}
        </li>
      ))}
    </ul>
  );
}
