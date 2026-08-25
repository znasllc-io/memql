import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { rowBool, rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { ARTIFACTS_ROOT } from "../artifacts/urls";
import { Empty, ErrorMessage } from "../components/StatusMessage";
import {
  Band,
  Breadcrumbs,
  Button,
  ButtonLink,
  ConfirmDialog,
  Container,
  DataText,
  Field,
  FormActions,
  FormRow,
  PageHeader,
  Select,
  Skeleton,
  TextInput,
} from "../ui";
import { ExternalLink } from "../ui/icons";
import { MAX_HISTORY_VERSIONS } from "./calls";
import { SITE_STATUS_VALUES, STOREFRONT_KIND } from "./concepts";
import { deployablesPath, liveUrlFor } from "./urls";
import { useDeployableDetail, useDeployableHistory, useZipArtifacts } from "./useDeployables";

// One deployable's detail + its five actions (memql#4346, absorbing the Sites
// detail screen unchanged where it was already right):
//
//   deploy from the Library  sitePublishFromArtifact -- the new one. The zip is
//                            read from the cluster's own storage, validated and
//                            handed to edge.Publisher, which writes a new
//                            content-addressed version and only then flips the
//                            row.
//   point at a bundle ref    updateSiteBundle with a value. Kept, because it is
//                            the only way to name a file:///app/* bundle baked
//                            into the edge image (the portal's own row is one)
//                            or a prefix a CI publish already wrote.
//   roll back                the same updateSiteBundle write in the other
//                            direction, over the asOf version walk.
//   enable / disable         updateSiteStatus.
//   delete                   deleteSite, refused server-side for a systemOwned
//                            row regardless of what this page renders.
//
// NO ACCESS GATE. v1:platform:site's composite tier means siteById returns the
// row to its owner and to a cluster owner and to nobody else -- so a caller who
// may not see this deployable gets the "no deployable has that id" branch,
// which is the same answer as a wrong id and deliberately so. The Sites screen
// rendered a "this is a cluster-owner surface" explanation here; that sentence
// is false now and the component is deleted rather than ported.
export function DeployableDetailPage(): ReactNode {
  const navigate = useNavigate();
  const { siteId = "" } = useParams<{ siteId: string }>();

  const detail = useDeployableDetail(siteId);
  const history = useDeployableHistory(siteId, detail.epoch);
  const artifacts = useZipArtifacts();
  const [bundleRef, setBundleRef] = useState("");
  const [artifactId, setArtifactId] = useState("");
  const [confirmingDelete, setConfirmingDelete] = useState(false);

  // A successful delete leaves nothing at this address -- go back to the list
  // rather than rendering a "not found" a moment after the person's own action
  // caused it.
  useEffect(() => {
    if (detail.deleted) navigate(deployablesPath());
  }, [detail.deleted, navigate]);

  if (detail.error) {
    return <ErrorMessage>Could not read this deployable: {detail.error}</ErrorMessage>;
  }
  if (detail.loading && detail.site === null) {
    return <Skeleton variant="kv" rows={6} />;
  }
  if (detail.site === null) {
    return (
      <Empty>
        No deployable has that id. It may have been deleted, it may belong to someone else, or the
        link may name one from another cluster.{" "}
        <Link to={deployablesPath()} className="text-accent underline">
          Back to deployables
        </Link>
      </Empty>
    );
  }

  const site = detail.site;
  const hostname = rowString(site, "hostname");
  const kind = rowString(site, "kind");
  const status = rowString(site, "status");
  const owner = rowString(site, "ownerUserId");
  const systemOwned = rowBool(site, "systemOwned");
  const title = rowString(site, "title");
  const currentBundleRef = rowString(site, "bundleRef");
  const liveUrl = liveUrlFor(hostname);

  function submitBundle(event: FormEvent): void {
    event.preventDefault();
    const next = bundleRef.trim();
    if (next === "") return;
    detail.publishBundleRef(next);
    setBundleRef("");
  }

  return (
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          eyebrow={
            <Breadcrumbs
              items={[{ label: "Deployables", to: deployablesPath() }, { label: title || hostname }]}
            />
          }
          title={title || hostname}
          blurb={
            <>
              <DataText kind="id">{hostname}</DataText> · {kind || "unknown kind"} · status{" "}
              <span className="font-medium text-fg">{status || "unknown"}</span>
              {systemOwned ? " · system-owned" : ""}
              {owner === "" ? " · cluster-owned" : ""}
            </>
          }
          actions={
            liveUrl === "" ? undefined : (
              <ButtonLink size="xs" href={liveUrl} target="_blank" rel="noreferrer">
                Open
                <ExternalLink size={14} aria-hidden="true" />
              </ButtonLink>
            )
          }
        />

        {/* A draft answers 404 BEFORE any file lookup, so a deployable created
            here serves nothing until somebody says so. Said on the page rather
            than left to be discovered by opening the link. */}
        {status === "draft" ? (
          <p className="rounded border border-line bg-raised px-3 py-2 text-sm text-fg">
            This is a draft: the edge answers 404 for {hostname || "its hostname"} until it is set
            to live. Deploy a bundle, then set the status below.
          </p>
        ) : null}

        {detail.actionError ? <ErrorMessage>{detail.actionError}</ErrorMessage> : null}
        {detail.actionMessage ? (
          <p role="status" className="rounded border border-line bg-raised px-3 py-2 text-sm text-fg">
            {detail.actionMessage}
          </p>
        ) : null}

        <Band title="Deploy from your Library" meta="a zip you uploaded becomes this deployable's new version">
          <DeployFromLibrary
            artifacts={artifacts.rows}
            loading={artifacts.loading}
            error={artifacts.error}
            value={artifactId}
            onChange={setArtifactId}
            busy={detail.publishBusy}
            onDeploy={() => detail.publishFromArtifact(artifactId)}
          />
          {detail.publishOutcome === null ? null : (
            <p role="status" className="mt-2 text-xs text-fg">
              Deployed <DataText kind="id">{detail.publishOutcome.version}</DataText> —{" "}
              {detail.publishOutcome.fileCount}{" "}
              {detail.publishOutcome.fileCount === 1 ? "file" : "files"},{" "}
              {formatBytes(detail.publishOutcome.totalBytes)}, now serving{" "}
              <DataText kind="id">{detail.publishOutcome.bundleRef}</DataText>.
            </p>
          )}
        </Band>

        <Band title="Point at a bundle reference" meta="for a bundle CI published, or one baked into the edge image">
          {/* Capped on the form, not the page: the bundle field is one input,
              and the bands below want the full width. */}
          {/* The width cap moves to a wrapper: FormRow owns the row's
              alignment and takes no class of its own. */}
          <div className="max-w-3xl">
            <FormRow onSubmit={submitBundle}>
              <Field
                label="Bundle"
                grow
                hint="A bundleRef VALUE this cluster already has -- 'blob://sites/<id>/<version>/' for a bundle in storage, or a file:// path baked into the edge image. This only points the row at a version that exists; it uploads nothing."
              >
                <TextInput value={bundleRef} onChange={setBundleRef} placeholder={currentBundleRef} />
              </Field>
              <FormActions>
                <Button
                  type="submit"
                  busyLabel="Working…"
                  busy={detail.busy}
                  disabled={bundleRef.trim() === ""}
                >
                  Publish
                </Button>
              </FormActions>
            </FormRow>
          </div>
          <p className="mt-1 text-xs text-subtle">
            Currently serving {currentBundleRef || "(none set)"}.
          </p>
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
                    <p className="truncate text-xs">
                      <DataText kind="id">{version.bundleRef || "(empty)"}</DataText>
                    </p>
                    <p className="text-xs text-subtle">
                      {version.createdAt}
                      {index === 0 ? " · current" : ""}
                    </p>
                  </div>
                  {index === 0 ? null : (
                    <Button
                      size="xs"
                      onClick={() => detail.publishBundleRef(version.bundleRef)}
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

        {kind === STOREFRONT_KIND ? <StorefrontBinding site={site} /> : null}

        <Band title="Status">
          <div className="flex flex-wrap items-center gap-2">
            <Select
              value={status}
              onChange={(next) => detail.setStatus(next)}
              ariaLabel="Status"
            >
              {SITE_STATUS_VALUES.map((value) => (
                <option key={value} value={value}>
                  {value}
                </option>
              ))}
            </Select>
            <p className="text-xs text-subtle">
              draft resolves for nobody; live serves; disabled answers 503 rather than 404.
            </p>
          </div>
        </Band>

        <Band title="Delete">
          {systemOwned ? (
            <p className="mb-2 text-sm text-subtle">
              This deployable is system-owned -- the platform's own console, re-seeded at boot -- so
              it cannot be deleted. The cluster refuses this write even if the control below were
              enabled; nobody can brick cluster management by deleting this row.
            </p>
          ) : null}
          <Button
            size="xs"
            onClick={() => setConfirmingDelete(true)}
            disabled={detail.busy || systemOwned}
            tone="danger"
          >
            Delete deployable
          </Button>
        </Band>

        <ConfirmDialog
          open={confirmingDelete}
          title="Delete this deployable?"
          confirmLabel="Delete deployable"
          tone="danger"
          busy={detail.busy}
          onConfirm={() => {
            setConfirmingDelete(false);
            detail.remove();
          }}
          onCancel={() => setConfirmingDelete(false)}
        >
          The edge stops answering for <DataText kind="id">{hostname}</DataText> once its resolve
          cache turns over, and the name is free for somebody else to claim. Published bundle bytes
          are not removed by this; creating a deployable at the same name and deploying a version
          brings it back.
        </ConfirmDialog>
      </section>
    </Container>
  );
}

// The deploy picker: the caller's Library, narrowed to zip file artifacts.
//
// NARROWED RATHER THAN VALIDATED-ON-SUBMIT. The capability refuses a non-zip by
// name (`artifact_not_a_zip`) and this page would render that refusal
// perfectly well -- but a picker that offers a PDF and then explains why it was
// a bad choice is a worse control than one that never offered it.
function DeployFromLibrary({
  artifacts,
  loading,
  error,
  value,
  onChange,
  busy,
  onDeploy,
}: {
  artifacts: Row[];
  loading: boolean;
  error: string;
  value: string;
  onChange: (next: string) => void;
  busy: boolean;
  onDeploy: () => void;
}): ReactNode {
  if (error) return <ErrorMessage>Could not read your Library: {error}</ErrorMessage>;
  if (loading) return <Skeleton variant="rows" rows={2} />;
  if (artifacts.length === 0) {
    return (
      <p className="text-sm text-muted">
        No zip bundles in your Library yet. Build the site, zip the CONTENTS of the build directory
        (index.html at the top level), and upload it on{" "}
        <Link to={ARTIFACTS_ROOT} className="text-accent underline">
          Artifacts
        </Link>
        .
      </p>
    );
  }

  return (
    <div className="max-w-3xl">
      <FormRow>
        <Field
          label="Bundle"
          grow
          hint="The cluster reads the bytes from its own storage, checks the zip, and writes a new version before pointing this deployable at it. Nothing is uploaded from this browser."
        >
          <Select value={value} onChange={onChange} ariaLabel="Bundle">
            <option value="">Choose a bundle…</option>
            {artifacts.map((row) => {
              const id = rowString(row, "id");
              return (
                <option key={id} value={id}>
                  {rowString(row, "title") || id}
                </option>
              );
            })}
          </Select>
        </Field>
        <FormActions>
          <Button
            tone="primary"
            busy={busy}
            busyLabel="Deploying…"
            disabled={value === ""}
            onClick={onDeploy}
          >
            Deploy
          </Button>
        </FormActions>
      </FormRow>
    </div>
  );
}

// The storefront's typed binding, READ-ONLY.
//
// There is no mutation that edits it on its own: `binding` is written by
// createSite, and re-running createSite on the same id is what changes it (the
// read-merge makes that an update). That is an operator-shaped act with the
// hostname in it, so this page shows the binding and does not pretend to a
// field that would silently do nothing.
function StorefrontBinding({ site }: { site: Row }): ReactNode {
  const binding = (site["binding"] ?? {}) as Record<string, unknown>;
  const storeDomain = typeof binding["storeDomain"] === "string" ? binding["storeDomain"] : "";
  const tokenRef =
    typeof binding["storefrontTokenRef"] === "string" ? binding["storefrontTokenRef"] : "";
  return (
    <Band title="Shopify storefront" meta="injected into this site's runtime config at serve time">
      <dl className="grid max-w-3xl grid-cols-[10rem_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-xs text-muted">Store domain</dt>
        <dd>
          <DataText kind="id">{storeDomain || "(not set)"}</DataText>
        </dd>
        <dt className="text-xs text-muted">Storefront token secret</dt>
        <dd>
          <DataText kind="id">{tokenRef || "(not set)"}</DataText>
        </dd>
      </dl>
      <p className="mt-2 max-w-2xl text-xs text-subtle">
        The secret is named here, never stored here: the edge resolves it when it serves the site,
        and only a Storefront API token -- a client-side credential by Shopify's own design -- is
        ever injected. Checkout stays Shopify's hosted checkout.
      </p>
    </Band>
  );
}

// formatBytes renders a publish's total size at one decimal place. Base 1024,
// because the limits it is read against (25 MB per file, 500 MB total) are
// enforced in Go against 1024-based constants.
function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${unit === 0 ? value : value.toFixed(1)} ${units[unit]}`;
}
