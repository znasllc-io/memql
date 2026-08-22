import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { rowBool, rowString } from "@znasllc-io/memql-sdk-core/client";

import { useMyAccess } from "../cluster/useMyAccess";
import { Empty, ErrorMessage } from "../components/StatusMessage";
import { Breadcrumbs, Button, ConfirmDialog, Container, DataText, Field, PageHeader, Skeleton, TextInput } from "../ui";
import { Band } from "../ui";
import { EnumSelect } from "./EnumSelect";
import { SitesRefused } from "./SitesRefused";
import { sitesPath } from "./urls";
import { useSiteDetail } from "./useSiteDetail";
import { useSiteHistory } from "./useSiteHistory";
import { MAX_HISTORY_VERSIONS } from "./history";

// One site's detail + actions (memql#3717): publish / roll back
// (updateSiteBundle, both directions of the same write -- ruling 1: this is
// a bundleRef VALUE, never a file upload), enable / disable
// (updateSiteStatus), and delete (deleteSite, refused server-side for a
// systemOwned row regardless of what this page renders -- ruling 3).
//
// status has no schema-driven selector the way SitesPage's kind field does.
// siteById's rows are shaped through siteFull (dsl/platform/shapes.memql),
// and a struct-form shape projects ONLY the paths it names -- it does not
// carry the `schema` row intrinsic unless the shape explicitly lists it,
// unlike the UNSHAPED generic browse SitesPage's kind selector reads
// (sdk/ts/src/client/conceptBrowser.ts's browseConceptPage sends a bare
// sort(paginate(...)) call with no shape() at all, which takes the
// default-projection path and keeps every intrinsic). Extending siteFull to
// carry schema would fix this, but `schema` is not among the row
// intrinsics CLAUDE.md documents as author-facing (row.id / concept / type /
// createdAt / createdBy / provenance.<leaf>), so this stays a fixed
// three-value list rather than guessing at an unreviewed shape change.
const SITE_STATUS_VALUES = ["draft", "live", "disabled"] as const;

export function SiteDetailPage(): ReactNode {
  const navigate = useNavigate();
  const { siteId = "" } = useParams<{ siteId: string }>();
  const { access, loading: accessLoading } = useMyAccess();
  const role = access?.clusterRole ?? "";
  const isOwner = role === "owner";
  const accessResolved = !accessLoading && access !== null;

  const detail = useSiteDetail(siteId);
  const history = useSiteHistory(siteId);
  const [bundleRef, setBundleRef] = useState("");
  const [confirmingDelete, setConfirmingDelete] = useState(false);

  // A successful delete leaves nothing at this address -- go back to the
  // list rather than rendering a "site not found" a moment after the
  // operator's own action caused it.
  useEffect(() => {
    if (detail.deleted) navigate(sitesPath());
  }, [detail.deleted, navigate]);

  if (!accessResolved) return <Skeleton variant="text" width="w-40" />;
  if (!isOwner) return <SitesRefused role={role} resolved={accessResolved} />;

  if (detail.error) {
    return <ErrorMessage>Could not read this site: {detail.error}</ErrorMessage>;
  }
  if (detail.loading && detail.site === null) {
    return <Skeleton variant="kv" rows={6} />;
  }
  if (detail.site === null) {
    return (
      <Empty>
        No site has that id. It may have been deleted, or the link may name a site from
        another cluster.{" "}
        <Link to={sitesPath()} className="text-accent underline">
          Back to sites
        </Link>
      </Empty>
    );
  }

  const site = detail.site;
  const hostname = rowString(site, "hostname");
  const kind = rowString(site, "kind");
  const status = rowString(site, "status");
  const systemOwned = rowBool(site, "systemOwned");
  const title = rowString(site, "title");
  const currentBundleRef = rowString(site, "bundleRef");

  function submitBundle(event: FormEvent): void {
    event.preventDefault();
    const next = bundleRef.trim();
    if (next === "") return;
    detail.publish(next);
    setBundleRef("");
  }

  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          eyebrow={<Breadcrumbs items={[{ label: "Sites", to: sitesPath() }, { label: title || hostname }]} />}
          title={title || hostname}
          blurb={
            <>
              <DataText kind="id">{hostname}</DataText> · {kind || "unknown kind"} · status{" "}
              <span className="font-medium text-fg">{status || "unknown"}</span>
              {systemOwned ? " · system-owned" : ""}
            </>
          }
        />

        {detail.actionError ? <ErrorMessage>{detail.actionError}</ErrorMessage> : null}
        {detail.actionMessage ? (
          <p role="status" className="rounded border border-line bg-raised px-3 py-2 text-sm text-fg">
            {detail.actionMessage}
          </p>
        ) : null}

        <Band title="Publish" meta="point this site at a bundle version">
          {/* Capped on the form, not the page: the bundle field is one input,
              and the status + delete bands below want the full width. */}
          <form onSubmit={submitBundle} className="flex max-w-3xl flex-wrap items-end gap-2">
            <Field
              label="Bundle"
              grow
              hint="A bundleRef VALUE this cluster already has -- 'blob://sites/<id>/<version>/' for an uploaded bundle, or a file:// path baked into the edge image. Uploading the bytes themselves is a CI/CLI action; this only points the row at a version that exists."
            >
              <TextInput value={bundleRef} onChange={setBundleRef} placeholder={currentBundleRef} />
            </Field>
            <Button type="submit" size="xs" busyLabel="Working…" busy={detail.busy} disabled={bundleRef.trim() === ""}>
              Publish
            </Button>
          </form>
          <p className="mt-1 text-xs text-subtle">Currently serving {currentBundleRef || "(none set)"}.</p>
        </Band>

        <Band
          title="Version history"
          meta={`the last ${MAX_HISTORY_VERSIONS <= 1 ? "version" : `up to ${MAX_HISTORY_VERSIONS} versions`}`}
        >
          {history.loading ? (
            <Skeleton variant="rows" rows={4} />
          ) : history.error ? (
            <ErrorMessage>Could not read version history: {history.error}</ErrorMessage>
          ) : history.versions.length === 0 ? (
            <Empty>No version history yet.</Empty>
          ) : (
            <ul className="flex flex-col gap-1.5">
              {history.versions.map((version, index) => (
                <li
                  key={version.createdAt || index}
                  className="flex items-center justify-between gap-3 rounded-lg border border-line bg-surface px-3 py-2"
                >
                  <div className="min-w-0">
                    <p className="truncate text-xs"><DataText kind="id">{version.bundleRef || "(empty)"}</DataText></p>
                    <p className="text-xs text-subtle">
                      {version.createdAt}
                      {index === 0 ? " · current" : ""}
                    </p>
                  </div>
                  {index === 0 ? null : (
                    <Button size="xs"
                      onClick={() => detail.publish(version.bundleRef)}
                      disabled={detail.busy || version.bundleRef === ""}
                    >
                      Roll back to this
                    </Button>
                  )}
                </li>
              ))}
            </ul>
          )}
        </Band>

        <Band title="Status">
          <div className="flex flex-wrap items-center gap-2">
            <EnumSelect
              value={status}
              onChange={(next) => detail.setStatus(next)}
              values={SITE_STATUS_VALUES}
              placeholder="Change status…"
            />
            <p className="text-xs text-subtle">
              draft resolves for nobody; live serves; disabled answers 503 rather than 404.
            </p>
          </div>
        </Band>

        <Band title="Delete">
          {systemOwned ? (
            <p className="mb-2 text-sm text-subtle">
              This site is system-owned -- the platform's own console, re-seeded at boot -- so it
              cannot be deleted. The cluster refuses this write even if the control below were
              enabled; an operator cannot brick cluster management by deleting this row.
            </p>
          ) : null}
          <Button
            size="xs"
            onClick={() => setConfirmingDelete(true)}
            disabled={detail.busy || systemOwned}
            tone="danger"
          >
            Delete site
          </Button>
        </Band>

        <ConfirmDialog
          open={confirmingDelete}
          title="Delete this site?"
          confirmLabel="Delete site"
          tone="danger"
          busy={detail.busy}
          onConfirm={() => {
            setConfirmingDelete(false);
            detail.remove();
          }}
          onCancel={() => setConfirmingDelete(false)}
        >
          The edge stops answering for <DataText kind="id">{hostname}</DataText> once its resolve
          cache turns over. Uploaded bundle bytes are not removed by this; re-creating the site at
          the same hostname and re-publishing a version brings it back.
        </ConfirmDialog>
      </section>
    </Container>
  );
}
