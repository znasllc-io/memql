import { useMemo, useState, type FormEvent, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import { useConcepts } from "../cluster/useConcepts";
import { liveBandIsEmpty } from "../concepts/liveBand";
import { enumValuesForField } from "../concepts/schema";
import { RowList } from "../components/RowList";
import { ErrorMessage } from "../components/StatusMessage";
import { Button, Container, EmptyState, Field, PageHeader, Skeleton, TextInput } from "../ui";
import { Band, PopulationMeta } from "../views/ViewLayout";
import { SITE_CONCEPT_ID } from "./concepts";
import { EnumSelect } from "./EnumSelect";
import { SitesRefused } from "./SitesRefused";
import { sitePath } from "./urls";
import { useSites, type CreateSiteInput } from "./useSites";

// The sites LIST screen (memql#3717): every site in the cluster, live, plus
// the create form. The row-level actions (publish / roll back / enable /
// disable / delete) live one level down at /sites/:siteId -- SiteDetailPage
// -- the same split CampaignsPage/CampaignEditorPage use, and for the same
// reason: an operator picks a site deliberately before touching it, rather
// than reaching for a button in a list row.
//
// DO NOT HAND-ROLL THE LIST. RowList (components/RowList.tsx) is the ONLY
// renderer used here, driven entirely by v1:platform:site's declared
// @displayCard -- enforced by portal_render_path_test.go, which fails the
// build on a concept-id literal inside RowList.tsx itself. This file feeds
// it rows; it does not decide how a row looks.

export function SitesPage(): ReactNode {
  const navigate = useNavigate();
  const { concepts, loading: conceptsLoading, error: conceptsError } = useConcepts();
  const { rows, role, isOwner, accessResolved, createBusy, createError, createSite } = useSites();

  const concept = concepts.find((c) => c.id === SITE_CONCEPT_ID);
  const kindValues = useMemo(() => enumValuesForField(rows.walk.rows, "kind"), [rows.walk.rows]);

  const select = (id: string) => navigate(sitePath(id));

  // Access is a courtesy render decision, not the gate -- see useSites.ts's
  // header. Resolving is its own state so "you are not the owner" is never
  // said before the connection has actually answered who you are.
  if (!accessResolved) {
    return <Skeleton variant="text" width="w-40" />;
  }
  if (!isOwner) {
    return <SitesRefused role={role} resolved={accessResolved} />;
  }

  return (
    <Container variant="data">
      <section className="flex min-h-full flex-col gap-6 pb-8">
      <PageHeader
        eyebrow={SITE_CONCEPT_ID}
        title="Sites"
        blurb="Every hosted surface this cluster's edge answers for -- the platform's own portal included. A site is data, not infrastructure: publishing and rolling back point its row at a bundle version, and the edge picks up the change on its next resolve for that hostname."
      />

      <Band title="New site">
        <NewSiteForm
          busy={createBusy}
          error={createError}
          kindValues={kindValues}
          onCreate={createSite}
        />
      </Band>

      <Band
        title="Sites"
        meta={
          <PopulationMeta
            count={rows.walk.rows.length}
            status={rows.walk.status}
            error={rows.walk.error}
            onLoadMore={rows.loadMore}
            onRetry={rows.retry}
          />
        }
        panel
      >
        {rows.liveDegraded ? (
          <p className="mb-3 rounded border border-warn bg-warn-subtle px-3 py-2 text-xs text-fg">
            Live updates are off for this list: {rows.liveDegraded}. Sites already loaded are
            still accurate; new ones will not appear until you reload.
          </p>
        ) : null}

        {liveBandIsEmpty(rows.live) ? null : (
          <div className="mb-3 overflow-hidden rounded-lg border border-accent bg-accent-subtle/40">
            <div className="flex flex-wrap items-center justify-between gap-2 border-b border-accent/40 px-3 py-1.5">
              <span className="text-xs font-medium text-fg">
                New since you opened this
                {rows.live.created.length > 0 ? ` — ${rows.live.created.length}` : ""}
                {rows.live.changedIds.length > 0
                  ? `, ${rows.live.changedIds.length} existing ${
                      rows.live.changedIds.length === 1 ? "site" : "sites"
                    } changed`
                  : ""}
              </span>
              <Button size="xs" onClick={rows.reload}>
                Reload the list
              </Button>
            </div>
            {rows.live.created.length > 0 && concept ? (
              <RowList rows={[...rows.live.created]} concept={concept} onSelect={select} />
            ) : null}
          </div>
        )}

        {conceptsError ? (
          <ErrorMessage>Could not read the concept registry: {conceptsError}</ErrorMessage>
        ) : rows.walk.status === "failed" && rows.walk.rows.length === 0 ? (
          <ErrorMessage>Could not read sites: {rows.walk.error}</ErrorMessage>
        ) : rows.walk.rows.length === 0 ? (
          conceptsLoading || rows.walk.status === "loading" || rows.walk.status === "idle" ? (
            <Skeleton variant="rows" rows={5} />
          ) : (
            <EmptyState statement="No sites yet. Name a hostname in the form above to create the first one." />
          )
        ) : concept ? (
          <RowList rows={[...rows.walk.rows]} concept={concept} onSelect={select} />
        ) : (
          <Skeleton variant="rows" rows={5} />
        )}
      </Band>
      </section>
    </Container>
  );
}

function NewSiteForm({
  busy,
  error,
  kindValues,
  onCreate,
}: {
  busy: boolean;
  error: string;
  kindValues: readonly string[];
  onCreate: (input: CreateSiteInput) => void;
}): ReactNode {
  const [hostname, setHostname] = useState("");
  const [kind, setKind] = useState("");
  const [bundleRef, setBundleRef] = useState("");

  const incomplete = hostname.trim() === "" || kind === "" || bundleRef.trim() === "";

  function submit(event: FormEvent): void {
    event.preventDefault();
    if (incomplete) return;
    onCreate({ hostname: hostname.trim(), kind, bundleRef: bundleRef.trim() });
    setHostname("");
    setKind("");
    setBundleRef("");
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-2">
      {error ? <ErrorMessage>{error}</ErrorMessage> : null}
      <div className="flex flex-wrap items-end gap-2">
        <Field label="Hostname" grow hint="Fully qualified, e.g. shop.example.com. Cluster-unique.">
          <TextInput value={hostname} onChange={setHostname} placeholder="shop.example.com" />
        </Field>
        <Field label="Kind">
          {kindValues.length === 0 ? (
            <p className="rounded-lg border border-line bg-surface px-3 py-2 text-sm text-subtle">
              Loading…
            </p>
          ) : (
            <EnumSelect value={kind} onChange={setKind} values={kindValues} />
          )}
        </Field>
      </div>
      <Field
        label="Bundle"
        grow
        hint="A bundleRef VALUE, not a file upload -- 'blob://sites/<id>/<version>/' for an uploaded bundle, or 'file:///app/sites/<name>' for one baked into the edge image. Uploading the bytes themselves is a CI/CLI action; this points the row at a version that already exists."
      >
        <TextInput
          value={bundleRef}
          onChange={setBundleRef}
          placeholder="blob://sites/shop/2026-08-13T00-00-00Z/"
        />
      </Field>
      <div>
        <Button type="submit" size="xs" busyLabel="Working…" busy={busy} disabled={incomplete}>
          Create site
        </Button>
      </div>
    </form>
  );
}
