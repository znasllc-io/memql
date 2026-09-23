import { useState } from "react";

import { Button, Caption, Head, Subhead, RecordList, RecordRow } from "../../kit";
import { RoleIdentity, placeOnLadder } from "../../modules/profile/RoleIdentity";
import { useSession } from "../../chrome/access";
import { useConnectionStatus } from "../../chrome/connection";
import { useOs } from "../../chrome/state";
import { osBridgePath } from "../../live/connection";
import { readStoredTheme } from "../../app/theme";
import { appsFor } from "../../system/registry";
import { buildDiagnosticsReport } from "./buildDiagnosticsReport";
import { resolveBridgeEndpoint } from "./endpoint";
import { hiddenSurfaces } from "./hiddenSurfaces";
import { useConnectionHistory } from "./useConnectionHistory";
import { useClusterReport } from "./useClusterFacts";

// Diagnostics (memql#4744). Three panels, all roles: what the connection has
// been doing, what this session is not being shown, and one button that
// turns both into text somebody can paste.

export function DiagnosticsSection() {
  const { access, config } = useSession();
  const { registry, state } = useOs();
  const status = useConnectionStatus();
  const history = useConnectionHistory();
  const cluster = useClusterReport();

  const endpoint = resolveBridgeEndpoint(osBridgePath, globalThis.location);
  const hidden = hiddenSurfaces(registry);
  const admitted = appsFor(registry);

  return (
    <div className="os-settings">
      <Head title="Diagnostics" />

      <section className="os-field-group" aria-label="Connection">
        <Subhead meta={history.transitions.length}>Connection</Subhead>
        <dl className="os-facts">
          <dt>Status</dt>
          <dd>
            <span
              className="os-dot"
              data-os-dot={
                status === "connected" ? "reachable" : status === "reconnecting" ? "unreachable" : "off"
              }
              role="img"
              aria-label={`Cluster connection: ${status}`}
            />{" "}
            {status}
          </dd>
          <dt>Endpoint</dt>
          <dd className="os-mono">{endpoint}</dd>
          <dt>Last reconnect</dt>
          <dd>
            {history.lastReconnectAt === null
              ? "none in this session"
              : new Date(history.lastReconnectAt).toISOString()}
          </dd>
        </dl>
        {history.transitions.length === 0 ? (
          <Caption>No transitions recorded since this window opened.</Caption>
        ) : (
          <ul className="os-transitions" aria-label="Connection transitions">
            {history.transitions.map((t) => (
              <li key={`${t.at}-${t.status}-${t.attempt}`}>
                <span className="os-mono">{new Date(t.at).toISOString()}</span> {t.status}
                {t.attempt > 0 ? ` (attempt ${t.attempt})` : ""}
                {t.baseline ? " -- reading when this window opened" : ""}
                {t.error ? ` -- ${t.error}` : ""}
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="os-field-group" aria-label="Permissions">
        <Subhead meta={hidden.length}>Permissions</Subhead>
        {/* THE ROLE NAMED, NOT THE SLUG (epic memql#5166). This printed the
            raw slug at a person -- "You are support-lead" -- which is the
            machine's word for a thing they know as Support Lead. The block form
            of RoleIdentity belongs in a definition list; a sentence takes the
            name and the place beside it. */}
        <p className="os-stub-summary">
          You are <RoleIdentity access={access} inline />
          {placeOnLadder(access) === "" ? "" : `, ${placeOnLadder(access)}`}
          {access?.primaryEmail ? ` (${access.primaryEmail})` : ""}.
        </p>
        {hidden.length === 0 ? (
          <Caption>Nothing in this shell is hidden from you.</Caption>
        ) : (
          <RecordList as="ul" label="Hidden from this session">
            {hidden.map((h) => (
              <RecordRow key={`${h.kind}:${h.label}`} name={h.label} secondary={<>{h.kind} — needs {h.requires}</>}>
                <span>you are <RoleIdentity access={access} inline /></span>
              </RecordRow>
            ))}
          </RecordList>
        )}
        <Caption>
          This is presentation gating. The engine's row admission is the
          authority on every read, and a surface shown here is one this shell
          declines to draw -- not one the cluster declines to serve.
        </Caption>
      </section>

      <section className="os-field-group" aria-label="Copy diagnostics">
        <h4 className="os-subhead">Copy diagnostics</h4>
        <CopyReport
          report={() =>
            buildDiagnosticsReport({
              at: Date.now(),
              domain: config.domain,
              build: __OS_BUILD__,
              endpoint,
              userId: access?.userId ?? "",
              primaryEmail: access?.primaryEmail ?? "",
              role: access?.role ?? "",
              roleName: access?.roleName ?? "",
              rank: access?.rank ?? 0,
              connection: history,
              connectionStatus: status,
              themePack: state.themePack,
              mode: readStoredTheme(),
              reducedMotion: prefersReducedMotion(),
              admittedApps: admitted.map((a) => a.name),
              hidden,
              cluster,
            })
          }
        />
      </section>
    </div>
  );
}

/**
 * The clipboard is best-effort and the fallback is IN-SURFACE, never a
 * toast: a browser that refuses the copy (an insecure origin, a declined
 * permission) has lost nothing, and the report is right there to select by
 * hand. Same shape as the worker-install one-liner's copy.
 */
function CopyReport({ report }: { report: () => string }) {
  const [text, setText] = useState<string | null>(null);
  const [note, setNote] = useState("");

  function copy(): void {
    const body = report();
    setText(body);
    const clipboard = globalThis.navigator?.clipboard;
    if (!clipboard) {
      setNote("This browser did not offer a clipboard -- select the report below and copy it.");
      return;
    }
    void clipboard
      .writeText(body)
      .then(() => setNote("Diagnostics copied."))
      .catch(() =>
        setNote("The browser refused the copy -- select the report below and copy it."),
      );
  }

  return (
    <>
      <Button onClick={copy}>Copy diagnostics</Button>
      {note ? <Caption>{note}</Caption> : null}
      {text === null ? (
        <Caption>
          A plain-text report of this session: build, connection history, theme,
          and what is hidden from you. No tokens, no credentials, no other
          person's address.
        </Caption>
      ) : (
        <textarea
          className="os-report"
          aria-label="Diagnostics report"
          readOnly={true}
          rows={16}
          value={text}
        />
      )}
    </>
  );
}

/**
 * Read at the moment the report is built rather than subscribed to. The
 * report is a reading, and a stale flag on it would be worse than an
 * unsubscribed one.
 */
function prefersReducedMotion(): boolean {
  return globalThis.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false;
}
