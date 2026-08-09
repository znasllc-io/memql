import { useMemo, type ReactNode } from "react";
import { Link } from "react-router-dom";
import {
  CHECKLIST_ELEMENT,
  PROPORTION_BAR_ELEMENT,
  TABLE_ELEMENT,
} from "@znasllc-io/memql-view-kit";

import { Empty, ErrorMessage, Loading } from "../components/StatusMessage";
import { Band, MetaButton } from "../views/ViewLayout";
import { ViewElement } from "../views/ViewElement";
import { useIntegrationStatus } from "./useIntegrationStatus";
import {
  CREDENTIAL_CONCEPT,
  INTEGRATION_CONCEPT,
  SETTING_CONCEPT,
  credentialRows,
  emailReport,
  integrationRows,
  settingRows,
} from "./rows";
import { campaignsPath } from "./urls";

// The integration surface (memql#3323).
//
// THE PAGE IS BUILT AROUND ONE DISTINCTION and everything on it serves that
// distinction: CONFIGURED and HEALTHY are different questions, and an
// integration can be neither, either, or both.
//
// The reason it needs saying at all is that the email integration DEGRADES
// RATHER THAN FAILING. With no credentials the engine falls back to a
// log-only sender: every send returns success, a line lands in the node log,
// and no mail is delivered. A console that only reported "the last send did
// not error" would show a green tick over a system that has never delivered
// anything. So log-only is its own state -- configured: no, health: degraded --
// and the page prints the sentence out loud rather than encoding it in a
// colour.
//
// WHAT THE PAGE WILL NEVER SHOW is a credential value. The Go handler behind
// this reply reports presence, source and a rotation command; it has no value
// field to render, and integrations/email/status_test.go sweeps the serialized
// reply for planted secrets. There is likewise no field here that WRITES a
// secret and there is not going to be one: rotating a credential is
// `make secret-set`, and the page says so.
//
// The settings half is different, and that difference is the whole reason
// "configurable from the portal" is achievable honestly. The non-secret
// settings -- sender address, from-name, tenant id, relay host -- already live
// in v1:platform:globalVariable rows, which are ordinary graph state. The page
// reports which of them came from a row (changeable) and which from the
// environment (a redeploy), because a field that looks editable and silently
// does nothing is worse than a field that is not offered.

export function IntegrationsPage(): ReactNode {
  const { role, canView, status, loading, error, probing, probeError, refresh, probe } =
    useIntegrationStatus();

  const reports = status?.integrations ?? [];
  const email = useMemo(() => emailReport(reports), [reports]);
  const registry = useMemo(() => integrationRows(reports), [reports]);
  const settings = useMemo(() => settingRows(email?.settings ?? []), [email]);
  const credentials = useMemo(() => credentialRows(email?.credentials ?? []), [email]);

  return (
    <section className="mx-auto flex min-h-full max-w-5xl flex-col gap-6 pb-8">
      <header className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3 border-b border-line pb-4">
        <div className="min-w-0">
          <h1 className="text-xl font-semibold tracking-tight">Integrations</h1>
          <p className="mt-1 max-w-2xl text-sm text-muted">
            What this node has wired to the outside world. Registration, configuration and
            health are three separate facts and this page keeps them separate.
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Link
            to={campaignsPath()}
            className="rounded border border-line bg-surface px-2.5 py-1 text-xs font-medium text-fg hover:bg-raised"
          >
            Email campaigns
          </Link>
          <MetaButton onClick={refresh}>Refresh</MetaButton>
          <MetaButton onClick={probe} disabled={probing || !canView}>
            {probing ? "Checking…" : "Check now"}
          </MetaButton>
        </div>
      </header>

      {!canView ? (
        <Empty>
          Reading integration configuration needs the owner or admin role; this session holds
          {role === "" ? " none" : ` "${role}"`}. The cluster enforces that server-side — this
          page is only declining to ask.
        </Empty>
      ) : error ? (
        <ErrorMessage>Could not read the integration registry: {error}</ErrorMessage>
      ) : loading && status === null ? (
        <Loading what="the integration registry" />
      ) : reports.length === 0 ? (
        <Empty>This node registered no integration plug-ins.</Empty>
      ) : (
        <>
          {probeError ? (
            <ErrorMessage>The live check could not run: {probeError}</ErrorMessage>
          ) : null}

          <Band title="Configured" meta="registration is not configuration">
            <ViewElement
              element={PROPORTION_BAR_ELEMENT}
              rows={registry}
              concept={INTEGRATION_CONCEPT}
              options={{ bindings: { value: [] } }}
            />
          </Band>

          <Band title="Registered on this node" panel>
            <ViewElement
              element={TABLE_ELEMENT}
              rows={registry}
              concept={INTEGRATION_CONCEPT}
              options={{ sort: { field: "name", direction: "asc" } }}
            />
          </Band>

          <EmailSummary detail={email?.detail ?? ""} probed={status?.probed === true} />

          <Band title="Email settings" meta="non-secret, and the source decides whether it is changeable">
            <p className="mb-3 max-w-3xl text-sm text-muted">
              A setting sourced from <code className="font-mono text-xs">globalVariable</code> is a
              row in the graph, so it can be changed without a redeploy. One sourced from{" "}
              <code className="font-mono text-xs">env</code> comes from the bootstrap envelope, and
              the resolver reads the environment first and stops — writing a row for it would have
              no effect.
            </p>
            <div className="overflow-x-auto rounded-lg border border-line bg-surface p-1">
              <ViewElement
                element={TABLE_ELEMENT}
                rows={settings}
                concept={SETTING_CONCEPT}
                options={{ sort: { field: "name", direction: "asc" } }}
              />
            </div>
          </Band>

          <Band title="Email credentials" meta="presence only — no value is ever sent to this page">
            <p className="mb-3 max-w-3xl text-sm text-muted">
              Secrets are sealed under the cluster master key and are never returned to a client,
              in any form. To set or rotate one, run{" "}
              <code className="font-mono text-xs">
                make secret-set NAME=&lt;variable&gt; VALUE=&lt;new value&gt; SCOPE=global
              </code>{" "}
              against the cluster; the exact variable name for each slot is in the table below.
            </p>
            <ViewElement
              element={CHECKLIST_ELEMENT}
              rows={credentials}
              concept={CREDENTIAL_CONCEPT}
              options={{ bindings: { group: [] } }}
            />
            <div className="mt-3 overflow-x-auto rounded-lg border border-line bg-surface p-1">
              <ViewElement
                element={TABLE_ELEMENT}
                rows={credentials}
                concept={CREDENTIAL_CONCEPT}
                options={{ sort: { field: "name", direction: "asc" } }}
              />
            </div>
          </Band>
        </>
      )}
    </section>
  );
}

// EmailSummary is the one place the page speaks in sentences rather than in
// rows. It is prose because the thing being reported is a JUDGEMENT the server
// made -- which lane resolved, what that means, and what has and has not been
// verified -- and compressing that into a badge is how "log-only" comes to look
// like "fine".
function EmailSummary({ detail, probed }: { detail: string; probed: boolean }): ReactNode {
  if (detail === "") return null;
  return (
    <Band title="Email">
      <div className="rounded-lg border border-line bg-surface p-4">
        <p className="max-w-3xl text-sm text-fg">{detail}</p>
        {probed ? null : (
          <p className="mt-2 max-w-3xl text-xs text-subtle">
            No live check has run in this session. “Check now” asks the provider for a token (or
            completes a relay handshake) without sending any mail.
          </p>
        )}
      </div>
    </Band>
  );
}
