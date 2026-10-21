import { useEffect, useRef, useState } from "react";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";
import { Caption, CopyValue, Fact, Facts, Head, Notice, Panel, RecordList, RecordRow, formatMoment } from "../../kit";
import { ActionBar } from "../../kit/ActionBar";
import { useOsConnection } from "../../live/connection";
import { computerPermission } from "./computerPermissions";

/** Reads the active receipts again when the app's existing approval feed moves. */
export function ComputerPermissionsPage({ approvalVersion, onBack }: { approvalVersion: number; onBack: () => void }) {
  const connection = useOsConnection();
  const currentConnection = useRef(connection);
  currentConnection.current = connection;
  const [loadedFor, setLoadedFor] = useState<typeof connection>(null);
  const [rows, setRows] = useState<Row[]>([]);
  const [selectedId, setSelectedId] = useState("");
  const currentSelection = useRef(selectedId);
  currentSelection.current = selectedId;
  const [ready, setReady] = useState(false);
  const [error, setError] = useState("");
  const [revoking, setRevoking] = useState(false);
  const [writeError, setWriteError] = useState("");
  const [revision, setRevision] = useState(0);
  useEffect(() => {
    const controller = new AbortController();
    setError("");
    if (!connection) {
      setRows([]);
      setReady(false);
      setError("Not connected to the cluster.");
      return;
    }
    void connection.query.workComputerPermissions({}, { signal: controller.signal }).then(result => {
      if (controller.signal.aborted) return;
      setRows(result.rows());
      setLoadedFor(connection);
      setReady(true);
    }).catch((cause: unknown) => {
      if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : String(cause));
    });
    return () => controller.abort();
  }, [connection, approvalVersion, revision]);

  const visibleRows = loadedFor === connection ? rows : [];
  const loaded = ready && loadedFor === connection;
  const selected = visibleRows.find(row => rowString(row, "id") === selectedId);
  const scope = computerPermission(selected?.subject);
  const back = selectedId ? () => { setSelectedId(""); setWriteError(""); } : onBack;
  const title = selectedId ? "Saved permission" : "Saved permissions";
  async function revoke() {
    if (!connection || !selected || revoking) return;
    setRevoking(true);
    setWriteError("");
    try {
      await connection.query.revokeComputerPermission({ permissionId: selectedId });
      if (currentConnection.current !== connection) return;
      setRows(held => held.filter(row => rowString(row, "id") !== selectedId));
      setSelectedId(current => current === selectedId ? "" : current);
      setRevision(value => value + 1);
    } catch (cause) {
      if (currentConnection.current === connection && currentSelection.current === selectedId) setWriteError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setRevoking(false);
    }
  }
  return <div className="os-nexus-approvals">
    <Head navigationKey={`computer-permissions:${selectedId}`} title={title} meta={!selectedId && loaded && !error ? visibleRows.length : undefined}
      back={{ label: selectedId ? "Saved permissions" : "Approvals", onSelect: back }}
      breadcrumbs={[{ label: "Approvals", onSelect: onBack }, ...(selectedId ? [{ label: "Saved permissions", onSelect: () => setSelectedId("") }] : []), { label: title }]} />
    <div className="os-nexus-approval-content">
      {error ? <Notice tone="error" sentence="Could not refresh saved permissions." detail={error} /> : null}
      {!loaded && !error ? <Caption>Loading saved permissions.</Caption> : null}
      {!selectedId ? <>
        {loaded && visibleRows.length === 0 && !error ? <Caption>No saved permissions.</Caption> : null}
        <RecordList label="Saved computer permissions" as="ul">{visibleRows.map(row => {
          const permission = computerPermission(row.subject);
          const id = rowString(row, "id");
          return <RecordRow key={id} name={permission?.kind === "directory_read" ? permission.directory : permission?.command ?? "Permission details unavailable"}
            secondary={permission?.machineLabel} state={permission?.kind === "directory_read" ? "Read directory" : permission ? "Exact command" : "Saved"}
            onOpen={() => { setWriteError(""); setSelectedId(id); }} />;
        })}</RecordList>
      </> : selected ? <Panel label="Computer access">
        {scope ? <div className="os-nexus-permission-scope"><Facts>
          <Fact label="Computer" value={scope.machineLabel} />
          <Fact label="Access" value={scope.kind === "directory_read" ? "Read files, list folders and inspect file details" : "Run this exact command"} />
          {scope.kind === "directory_read" ? <Fact label="Directory" value={<CopyValue value={scope.directory!} label="directory" />} /> : <>
            <Fact label="Command" value={<CopyValue value={scope.command!} label="command" />} />
            <Fact label="Working directory" value={<CopyValue value={scope.cwd!} label="working directory" />} />
            <Fact label="Time limit" value={`${scope.timeoutSeconds} seconds`} />
          </>}
          <Fact label="Allowed" value={formatMoment(String(selected.decidedAt ?? selected.createdAt ?? ""))} />
        </Facts></div> : <Notice tone="warn" sentence="The details of this saved permission cannot be read." next="You can still revoke it." />}
      </Panel> : loaded ? <Caption>This permission is no longer active.</Caption> : null}
    </div>
    {selected ? <ActionBar state="Allowed" detail="Revoke to ask again before future use." acts={[{ label: "Revoke permission", busy: revoking, onAct: () => { void revoke(); } }]}>{writeError ? <span role="alert">{writeError}</span> : null}</ActionBar>
      : error ? <ActionBar state="Unavailable" acts={[{ label: "Try again", onAct: () => setRevision(value => value + 1) }]} /> : null}
  </div>;
}
