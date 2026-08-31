import { useState } from "react";

import { Button, Caption } from "../../kit";
import { useSession } from "../../chrome/access";
import { useConnectionStatus } from "../../chrome/connection";
import { useOs } from "../../chrome/state";
import { osBridgePath } from "../../live/connection";
import { readStoredTheme } from "../../app/theme";
import { appsForRole } from "../../system/registry";
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
  const { registry, actorRole, state } = useOs();
  const status = useConnectionStatus();
  const history = useConnectionHistory();
  const cluster = useClusterReport();

  const endpoint = resolveBridgeEndpoint(osBridgePath, globalThis.location);
  const hidden = hiddenSurfaces(registry, actorRole);
  const admitted = appsForRole(registry, actorRole);

  return (
    <div className="os-settings">
      <h3 className="os-settings-title">Diagnostics</h3>

      <section className="os-field-group" aria-label="Connection">
        <h4 className="os-subhead">Connection</h4>
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
        <h4 className="os-subhead">Permissions</h4>
        <p className="os-stub-summary">
          You are {access?.clusterRole || "unknown"}
          {access?.primaryEmail ? ` (${access.primaryEmail})` : ""}.
        </p>
        {hidden.length === 0 ? (
          <Caption>Nothing in this shell is hidden from you.</Caption>
        ) : (
          <ul className="os-hidden-list" aria-label="Hidden from this session">
            {hidden.map((h) => (
              <li key={`${h.kind}:${h.label}`}>
                {h.label} <span className="os-caption-inline">({h.kind})</span> -- requires{" "}
                {h.requires}; you are {access?.clusterRole || "unknown"}
              </li>
            ))}
          </ul>
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
              clusterRole: access?.clusterRole ?? "",
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
