import { useMemo, type ReactNode } from "react";
import { useParams } from "react-router-dom";

import { ErrorMessage } from "../components/StatusMessage";
import { Badge, Band, Button, Container, PageHeader, Skeleton } from "../ui";
import type { DomainState } from "./health";
import { StoresRefused } from "./StoresRefused";
import { useStoreActions } from "./useStoreActions";
import { useStores } from "./useStores";

// One store: what it is allowed to see, what it has mirrored, how much the
// live path is missing, and the three things an operator can do about it.
//
// The numbers on this page are the whole diagnostic story, so each one says
// what it MEANS rather than only what it is. "driftLast: 40" is not
// actionable; "40 rows the live path should have carried and did not" is.

export function StoreDetailPage(): ReactNode {
  const { storeId = "" } = useParams();
  const { health, healthLoading, healthError, refreshHealth, role, isOwner, accessResolved } = useStores();
  const actions = useStoreActions(refreshHealth);

  const store = useMemo(() => health.find((s) => s.storeId === storeId), [health, storeId]);

  if (!accessResolved) {
    return <Skeleton variant="text" width="w-40" />;
  }
  if (!isOwner) {
    return <StoresRefused role={role} resolved={accessResolved} />;
  }

  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          eyebrow={storeId}
          title={store?.domain ?? storeId}
          blurb="Shopify owns this store's data and MemQL holds a generated mirror of it. Webhooks keep the mirror current; reconciliation repairs what they lose, and the drift below is the measurement of how much that is."
        />

        {healthError ? <ErrorMessage>{healthError}</ErrorMessage> : null}
        {actions.error ? <ErrorMessage>{actions.error}</ErrorMessage> : null}
        {actions.note !== "" ? (
          <p className="rounded border border-ok bg-ok-subtle px-3 py-2 text-sm text-fg">{actions.note}</p>
        ) : null}

        {store === undefined ? (
          healthLoading ? (
            <Skeleton variant="rows" />
          ) : (
            <p className="text-sm text-muted">No store with that id is configured.</p>
          )
        ) : (
          <>
            <Band title="Configuration" panel>
              <dl className="grid gap-3 text-sm sm:grid-cols-2">
                <Detail label="Status" value={store.status} />
                <Detail label="Protected customer data" value={store.protectedDataLevel || "none"} />
                <Detail
                  label="API version"
                  value={
                    store.apiVersion === "" ? `${store.mirrorApiVersion} (the mirror's)` : store.apiVersion
                  }
                  warn={store.apiVersion !== "" && store.apiVersion !== store.mirrorApiVersion}
                  note={
                    store.apiVersion !== "" && store.apiVersion !== store.mirrorApiVersion
                      ? `The mirror was generated from ${store.mirrorApiVersion}. A call is refused rather than attempted: another version returns fields the concepts do not declare and omits fields they require.`
                      : undefined
                  }
                />
                <Detail
                  label="Cost bucket"
                  value={
                    store.costBucket === undefined
                      ? "not observed yet"
                      : `${Math.round(store.costBucket.currentlyAvailable)} of ${Math.round(
                          store.costBucket.maximumAvailable,
                        )} points, restoring ${store.costBucket.restoreRate}/s`
                  }
                />
              </dl>
            </Band>

            <Band title="Scopes" panel>
              {store.scopesGranted.length === 0 ? (
                <p className="text-sm text-muted">
                  The installation has not reported its granted scopes yet, so nothing can be checked against
                  what the mirror needs. The connector needs {store.scopesNeeded.length} scope(s) in total.
                </p>
              ) : store.scopesMissing.length === 0 ? (
                <p className="text-sm text-muted">
                  All {store.scopesNeeded.length} scopes the mirror needs are granted.
                </p>
              ) : (
                <div className="flex flex-col gap-2">
                  <p className="text-sm text-fg">
                    {store.scopesMissing.length} scope(s) the mirror needs are not granted. Shopify returns
                    null for the fields they cover, so those domains are quietly incomplete rather than
                    broken.
                  </p>
                  <div className="flex flex-wrap gap-1">
                    {store.scopesMissing.map((scope) => (
                      <Badge key={scope} tone="warn">
                        {scope}
                      </Badge>
                    ))}
                  </div>
                </div>
              )}
            </Band>

            <Band
              title="Subscriptions"
              meta={
                <Button size="xs" onClick={actions.ensureSubscriptions} disabled={actions.busy !== ""}>
                  {actions.busy === "subscriptions" ? "Reconciling..." : "Reconcile now"}
                </Button>
              }
              panel
            >
              <SubscriptionSummary health={store.health} />
            </Band>

            <Band title="Ingestion" panel>
              <div className="mb-3 flex flex-wrap items-center gap-2">
                <Button
                  size="xs"
                  onClick={() => actions.setStatus(store.storeId, store.status === "paused" ? "live" : "paused")}
                  disabled={actions.busy !== ""}
                >
                  {store.status === "paused" ? "Resume ingestion" : "Pause ingestion"}
                </Button>
              </div>
              <p className="mb-3 text-xs text-muted">
                Pausing a store still STAGES deliveries -- a pause loses telemetry rather than events,
                and resuming needs no backfill. Backfilling, reconciling and pausing an individual
                DOMAIN belong to every connector, so they live on the Data origins surface rather than
                being repeated here.
              </p>
              <DomainTable domains={store.domains} />
            </Band>
          </>
        )}
      </section>
    </Container>
  );
}

function Detail({
  label,
  value,
  warn = false,
  note,
}: {
  label: string;
  value: string;
  warn?: boolean;
  note?: string;
}): ReactNode {
  return (
    <div>
      <dt className="text-xs uppercase tracking-wide text-muted">{label}</dt>
      <dd className={`text-sm ${warn ? "text-danger" : "text-fg"}`}>{value}</dd>
      {note === undefined ? null : <p className="mt-1 text-xs text-muted">{note}</p>}
    </div>
  );
}

function SubscriptionSummary({ health }: { health: Record<string, unknown> }): ReactNode {
  const subs = health["subscriptions"] as Record<string, unknown> | undefined;
  if (subs === undefined) {
    return (
      <p className="text-sm text-muted">
        No subscription reconcile has been recorded for this store yet. Shopify deletes a subscription after
        eight consecutive delivery failures, so a store that has gone quiet and a store nobody has checked
        look the same until this runs.
      </p>
    );
  }
  const failed = Array.isArray(subs["failed"]) ? (subs["failed"] as unknown[]) : [];
  return (
    <div className="flex flex-col gap-2 text-sm">
      <p>
        {String(subs["existing"] ?? 0)} registered of {String(subs["desired"] ?? 0)} the mirror wants, last
        checked {String(subs["at"] ?? "never")}.
      </p>
      {failed.length > 0 ? (
        <div>
          <p className="text-danger">{failed.length} topic(s) could not be registered:</p>
          <ul className="mt-1 list-disc pl-5 text-xs text-muted">
            {failed.slice(0, 10).map((f, i) => (
              <li key={i}>{String(f)}</li>
            ))}
          </ul>
        </div>
      ) : null}
    </div>
  );
}

function DomainTable({ domains }: { domains: DomainState[] }): ReactNode {
  if (domains.length === 0) {
    return (
      <p className="text-sm text-muted">
        No domain has been synced yet. Step a backfill above to start one.
      </p>
    );
  }
  const sorted = [...domains].sort((a, b) => b.driftLast - a.driftLast || a.concept.localeCompare(b.concept));
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-sm">
        <thead className="text-xs uppercase tracking-wide text-muted">
          <tr>
            <th className="py-1 pr-4">Domain</th>
            <th className="py-1 pr-4">Phase</th>
            <th className="py-1 pr-4" title="Rows the last reconcile wrote that live delivery should have carried">
              Drift
            </th>
            <th className="py-1 pr-4" title="Writes refused because the stored row was newer">
              Stale
            </th>
            <th className="py-1 pr-4">Tombstoned</th>
            <th className="py-1">Last applied</th>
          </tr>
        </thead>
        <tbody>
          {sorted.map((d) => (
            <tr key={d.concept} className="border-t border-line">
              <td className="py-1 pr-4 font-mono text-xs">{d.concept}</td>
              <td className="py-1 pr-4">
                <Badge tone={d.phase === "error" ? "danger" : d.phase === "idle" ? "neutral" : "warn"}>
                  {d.phase || "idle"}
                </Badge>
              </td>
              <td className="py-1 pr-4">
                {d.driftLast}
                {d.driftTotal > 0 ? <span className="text-muted"> / {d.driftTotal}</span> : null}
              </td>
              <td className="py-1 pr-4">{d.staleWrites}</td>
              <td className="py-1 pr-4">{d.tombstoned}</td>
              <td className="py-1 text-xs text-muted">{d.lastAppliedAt || "never"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
