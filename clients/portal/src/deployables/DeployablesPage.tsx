import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { rowString, type Concept, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useConcepts } from "../cluster/useConcepts";
import { ArrangedPage } from "../pages/ArrangedPage";
import { DEPLOYABLES_PAGE, DEPLOYABLES_PAGE_ID } from "./manifest";
import { RowList } from "../components/RowList";
import {
  Band,
  Button,
  DataText,
  EmptyState,
  ErrorNotice,
  Field,
  Select,
  Skeleton,
  TextInput,
} from "../ui";
import { ExternalLink } from "../ui/icons";
import { DEPLOYABLE_KINDS, SITE_CONCEPT_ID, STOREFRONT_KIND } from "./concepts";
import { hostnameFor, validateSlug } from "./hostname";
import { deployablePath, liveUrlFor } from "./urls";
import { useDeployables, type CreateDeployableInput } from "./useDeployables";

// The deployables LIST screen (memql#4346): everything this cluster hosts for
// you, plus the form that makes another one. The row-level actions -- deploy,
// roll back, enable / disable, delete -- live one level down at
// /deployables/:siteId, the same split CampaignsPage / CampaignEditorPage use
// and for the same reason: a person picks a deployable deliberately before
// touching it, rather than reaching for a button in a list row.
//
// THIS SCREEN IS NOT GATED, and that is the change from the Sites page it
// replaces. v1:platform:site declares the composite tier now, so an ordinary
// caller has deployables of their own; the old "this is a cluster-owner
// surface" screen said something that is no longer true and is deleted rather
// than ported. What the role still decides is the owner column -- see
// useDeployables.ts's header.
//
// DO NOT HAND-ROLL THE ROW. RowList (components/RowList.tsx) renders every card
// here, driven entirely by v1:platform:site's declared @displayCard -- enforced
// for the concept-agnostic browse path by portal_render_path_test.go. What this
// page owns is the column BESIDE the card: the live URL, and the owner when
// that is a fact worth showing. Same shape ArtifactsPage uses for export and
// archive, and for the same reason: a per-row action without teaching a
// concept-agnostic component about one concept.
// The page (epic memql#4661, task memql#4674). An ARRANGEMENT: the header, the
// version strip and the regenerate control come from ArrangedPage.
//
// One widget, because most of this page is a create form plus a list whose
// rows carry a deploy state no element expresses. The reading the arrangement
// adds beside it is the count.
export function DeployablesPage(): ReactNode {
  return (
    <ArrangedPage
      manifest={DEPLOYABLES_PAGE}
      pageId={DEPLOYABLES_PAGE_ID}
      selectedRowId=""
      onSelect={() => {}}
    />
  );
}

// DeployablesBody is what the `deployables` widget renders.
export function DeployablesBody(): ReactNode {
  const navigate = useNavigate();
  const { concepts, loading: conceptsLoading, error: conceptsError } = useConcepts();
  const {
    rows,
    loading,
    error,
    reload,
    isClusterOwner,
    domain,
    createBusy,
    createError,
    createdId,
    createDeployable,
  } = useDeployables();

  const concept = concepts.find((c) => c.id === SITE_CONCEPT_ID);
  const select = (id: string) => navigate(deployablePath(id));

  // A created deployable has nothing published yet, and deploying is where
  // anyone goes next -- so the create lands on the detail page rather than
  // leaving a draft row in a list to be found again.
  useEffect(() => {
    if (createdId !== "") navigate(deployablePath(createdId));
  }, [createdId, navigate]);

  return (
    <>
      <section className="flex min-w-0 flex-col gap-6">
        {/* The cluster-owner sentence stays HERE rather than in the manifest's
            blurb: it is conditional on who is reading, and a manifest is data
            that does not know. Saying "across every user" to somebody who is
            seeing only their own would be a page lying about its own scope. */}
        {isClusterOwner ? (
          <p className="text-sm text-muted">
            You are seeing every user&apos;s deployables because you are the cluster owner;
            everyone else sees their own.
          </p>
        ) : null}
        <div className="flex justify-end">
          <Button size="xs" onClick={reload}>
            Refresh
          </Button>
        </div>

        <Band title="New deployable" meta="created as a draft; deploy a bundle to it from your Library">
          <NewDeployableForm
            busy={createBusy}
            error={createError}
            domain={domain}
            onCreate={createDeployable}
          />
        </Band>

        <Band title="Deployables" meta={`${rows.length}`} panel>
          {conceptsError ? (
            <ErrorNotice sentence="Could not read the concept registry, so this page cannot be drawn." detail={conceptsError} />
          ) : error ? (
            <ErrorNotice sentence="Could not read what this cluster hosts." next="Reload the page to read it again." detail={error} />
          ) : rows.length === 0 ? (
            loading || conceptsLoading ? (
              <Skeleton variant="rows" rows={5} />
            ) : (
              <EmptyState statement="Nothing hosted yet. Name one above, then deploy a zip from your Library to it." />
            )
          ) : concept ? (
            <DeployableRows
              rows={rows}
              concept={concept}
              showOwner={isClusterOwner}
              onSelect={select}
            />
          ) : (
            <Skeleton variant="rows" rows={5} />
          )}
        </Band>
      </section>
    </>
  );
}

// DeployableRows renders the list: one RowList per row for the card, and this
// page's own column for the live URL and (for a cluster owner) the owner. See
// the file header for why the split is here rather than inside RowList.
function DeployableRows({
  rows,
  concept,
  showOwner,
  onSelect,
}: {
  rows: Row[];
  concept: Concept;
  showOwner: boolean;
  onSelect: (rowId: string) => void;
}): ReactNode {
  return (
    <div className="flex flex-col gap-1">
      {rows.map((row) => {
        const id = rowString(row, "id");
        const hostname = rowString(row, "hostname");
        const owner = rowString(row, "ownerUserId");
        const url = liveUrlFor(hostname);
        return (
          <div key={id} className="flex items-center gap-3">
            <div className="min-w-0 flex-1">
              <RowList rows={[row]} concept={concept} onSelect={onSelect} />
            </div>
            {showOwner ? (
              // An EMPTY ownerUserId is the cluster-owned state, not a missing
              // value -- the seeded portal row carries it, and the Go owner
              // stamp produces it for anything a cluster owner creates. Saying
              // "cluster" is the honest rendering; blank would read as unknown.
              <span className="shrink-0 text-xs text-subtle" title="Owner">
                {owner === "" ? "cluster" : <DataText kind="id">{owner}</DataText>}
              </span>
            ) : null}
            {url === "" ? null : (
              <a
                href={url}
                target="_blank"
                rel="noreferrer"
                title={`Open ${hostname}`}
                className="inline-flex shrink-0 items-center gap-1 text-xs text-accent underline"
              >
                {hostname}
                <ExternalLink size={12} aria-hidden="true" />
              </a>
            )}
          </div>
        );
      })}
    </div>
  );
}

// The create form.
//
// IT TAKES A NAME, NOT A HOSTNAME. A user's deployable is <slug>.<domain> for
// the domain this cluster serves: one label, because the front door routes
// every site through a single `*.<domain>` Ingress wildcard and a wildcard
// matches exactly one label. The domain is therefore never typed -- it is
// derived (src/cluster/editorLink.ts) from the same MEMQL_DOMAIN every host in
// the cluster derives from -- and the hostname is shown as it is composed.
//
// A CUSTOM HOSTNAME IS NOT OFFERED HERE AT ALL, including to a cluster owner
// who is allowed one. An apex or a second domain needs its own DNS record and
// its own Certificate, and the portal can create neither; a field that accepted
// one would mint a row the cluster cannot serve. That stays an operator action
// outside this form.
function NewDeployableForm({
  busy,
  error,
  domain,
  onCreate,
}: {
  busy: boolean;
  error: string;
  domain: string;
  onCreate: (input: CreateDeployableInput) => void;
}): ReactNode {
  const [slug, setSlug] = useState("");
  const [kind, setKind] = useState("spa");
  const [title, setTitle] = useState("");
  const [storeDomain, setStoreDomain] = useState("");
  const [storefrontTokenRef, setStorefrontTokenRef] = useState("");

  const slugProblem = validateSlug(slug, domain);
  const hostname = hostnameFor(slug, domain);
  const storefront = kind === STOREFRONT_KIND;
  const bindingIncomplete =
    storefront && (storeDomain.trim() === "" || storefrontTokenRef.trim() === "");
  const incomplete =
    slug.trim() === "" || slugProblem !== "" || kind === "" || domain === "" || bindingIncomplete;

  function submit(event: FormEvent): void {
    event.preventDefault();
    if (incomplete) return;
    onCreate({ slug: slug.trim(), kind, title: title.trim(), storeDomain, storefrontTokenRef });
    setSlug("");
    setTitle("");
    setStoreDomain("");
    setStorefrontTokenRef("");
  }

  return (
    <form onSubmit={submit} className="flex max-w-3xl flex-col gap-2">
      {error ? <ErrorNotice sentence="The deployable was not created." next="Check the fields below and try again." detail={error} /> : null}
      {domain === "" ? (
        <ErrorNotice
          sentence="This cluster has not told the console which domain it serves, so a hostname cannot be composed here."
          next="Ask a cluster owner to set the cluster's domain."
        />
      ) : null}

      <div className="flex flex-wrap items-start gap-2">
        <Field
          label="Name"
          grow
          {...(slugProblem === "" ? {} : { error: slugProblem })}
          hint={
            hostname === ""
              ? "3 to 40 characters: lowercase letters, digits and hyphens."
              : `Lives at ${hostname}`
          }
        >
          <TextInput value={slug} onChange={setSlug} placeholder="shop" />
        </Field>
        <Field label="Kind" hint={kindBlurb(kind)}>
          <KindPicker value={kind} onChange={setKind} />
        </Field>
      </div>

      <Field label="Title" grow hint="What this shows up as in the list. Defaults to the hostname.">
        <TextInput value={title} onChange={setTitle} placeholder="Shop" />
      </Field>

      {storefront ? (
        <div className="flex flex-wrap items-start gap-2 rounded-lg border border-line bg-surface p-3">
          <Field
            label="Store domain"
            grow
            hint="The myshopify.com host of the store this storefront fronts."
          >
            <TextInput
              value={storeDomain}
              onChange={setStoreDomain}
              placeholder="example.myshopify.com"
            />
          </Field>
          <Field
            label="Storefront token secret"
            grow
            // The name of a secret, never the secret. Worth saying on the
            // control itself: somebody who pastes the token here has put a
            // credential in a graph row that is readable by every cluster
            // owner, and the field would happily accept it.
            hint="The NAME of a global secret holding the Storefront API token -- not the token. The edge resolves it at serve time. Never an Admin API token."
          >
            <TextInput
              value={storefrontTokenRef}
              onChange={setStorefrontTokenRef}
              placeholder="shopify-storefront-token"
            />
          </Field>
        </div>
      ) : null}

      <div>
        <Button type="submit" size="xs" busyLabel="Working…" busy={busy} disabled={incomplete}>
          Create deployable
        </Button>
      </div>
    </form>
  );
}

function kindBlurb(value: string): string {
  return DEPLOYABLE_KINDS.find((k) => k.available && k.value === value)?.blurb ?? "";
}

// The kind picker. The three unavailable entries are DISABLED options rather
// than absent ones, and that is the whole point of rendering them: "we do not
// do Android yet" is a different answer from silence, and silence is what a
// schema-driven selector would give -- there is no enum value to read, by
// design (see concepts.ts).
function KindPicker({
  value,
  onChange,
}: {
  value: string;
  onChange: (next: string) => void;
}): ReactNode {
  return (
    <Select value={value} onChange={onChange} ariaLabel="Kind">
      {DEPLOYABLE_KINDS.map((kind, index) => (
        <option
          // An unavailable kind has no `value` to key on -- it has no schema at
          // all -- so the index is the only stable key for those three.
          key={kind.available ? kind.value : `soon-${index}`}
          value={kind.value}
          disabled={!kind.available}
        >
          {kind.available ? kind.label : `${kind.label} — coming soon`}
        </option>
      ))}
    </Select>
  );
}
