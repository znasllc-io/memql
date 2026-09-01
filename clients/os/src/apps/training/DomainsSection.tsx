import { useCallback, useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { Boxes, Link2Off } from "lucide-react";

import { Button, Caption, Chip, Chips, Head, Notice, Select, Subhead } from "../../kit";
import { AccountChip } from "../accounts/AccountPicker";
import { accountIsArchived, accountName, accountNameFrom } from "../accounts/rows";
import { useAccountOptions } from "../accounts/tie";
import { formatFreshness } from "../../kit/format";
import { useNow } from "../../kit/useNow";
import { useOsConnection } from "../../live/connection";
import { CORPUS_GROUP_LABEL, REJECTED, UNVALIDATED, VALIDATED } from "./concepts";
import { chunkFromRow, groupChunksByDocument, type Chunk, type DomainMeta, type DomainRollup } from "./rows";

/** The "no client" option's value. Not "" -- that is "any client". */
const ACCOUNT_UNTAGGED = "\u0000untagged";
import type { DomainsFeed } from "./useDomains";

// What MemQL knows, by domain.
//
// ===========================================================================
// A CARD IS LABELLED BY ITS domainId, AND THAT IS NOT A PLACEHOLDER
// ===========================================================================
// `v1:knowledge:knowledgeDomain` is declared in no `.memql` file -- it is
// product-owned -- and there is no query that lists domain rows. So a domain's
// own payload (name, category, tier, active; written by
// `integrations/knowledge/seed.go`) has no client read surface at all, and
// everything on this page is CHUNK-DERIVED.
//
// THAT CHANGED IN EPIC memql#4800, and the note is kept rather than deleted
// because the shape of the fix is the point. The concept is now declared
// (dsl/knowledge/concepts.memql), projected by `knowledgeDomainFull` and read
// by `knowledgeDomainsAll`, so a card can say the domain's NAME and render the
// client it is tagged with. A card whose id matches no catalog row STILL says
// the id -- that is not a fallback, it is the truthful answer on a cluster
// whose domain rows were never written, which an engine-only build genuinely
// is: `createKnowledgeDomain` is declared in no .memql file in this tree, so
// the seeder's write resolves to nothing.
//
// The authorization judgment the old note asked for was made and filed rather
// than guessed: the concept declares NO tier, because none the engine offers
// fits an ownerless system-seeded catalog, and memql#4809 is that question.
//
// THE ACCOUNT TAG CHANGES NOTHING ELSE (D5). Routing, attachment, retrieval
// and scoring do not consult it. It is rendered here and filterable here, and
// that is the whole of its effect.

export function DomainsSection({
  feed,
  onNavigate,
}: {
  feed: DomainsFeed;
  /** Takes somebody to the dropzone from the empty state. The app's own
   *  navigation -- this never opens a window. */
  onNavigate: (sectionId: string) => void;
}) {
  const now = useNow(30_000);
  const [openDomainId, setOpenDomainId] = useState("");
  const accounts = useAccountOptions();
  // "" = no client constraint. An id narrows to domains tagged with it;
  // ACCOUNT_UNTAGGED narrows to domains carrying no tag at all, which is the
  // question somebody asks while they are tagging.
  const [accountFilter, setAccountFilter] = useState("");

  // THE FILTER IS A FOLD, not a second read. `knowledgeDomainsAll` returns the
  // whole catalog and the chunk rollups are already here; narrowing on the
  // client is a view over both, and re-reading per selection would put a round
  // trip behind a dropdown.
  //
  // A ROLLUP WITH NO CATALOG ROW COUNTS AS UNTAGGED, which is the only honest
  // reading: a domain the seeder never wrote a row for has no tag, and hiding
  // it under "no client" would make the untagged view lie about what is
  // untagged.
  const shown = feed.rollups.filter((rollup) => {
    if (accountFilter === "") return true;
    const tag = feed.domains.get(rollup.domainId)?.accountId ?? "";
    return accountFilter === ACCOUNT_UNTAGGED ? tag === "" : tag === accountFilter;
  });

  return (
    <div className="os-app-stack">
      <Head title="Domains">
        <Select
          id="training-domain-account"
          label="Client"
          value={accountFilter}
          onChange={setAccountFilter}
        >
          <option value="">Any client</option>
          <option value={ACCOUNT_UNTAGGED}>No client</option>
          {accounts.map((account) => (
            <option key={account.id} value={account.id}>
              {accountIsArchived(account) ? `${accountName(account)} (archived)` : accountName(account)}
            </option>
          ))}
        </Select>
        <Button onClick={feed.reload}>Re-read</Button>
      </Head>

      {feed.error !== "" ? (
        <Notice
          tone="error"
          sentence="This cluster did not return its knowledge domains."
          next="Nothing below is current."
          detail={feed.error}
        >
          <Button onClick={feed.reload}>Try again</Button>
        </Notice>
      ) : null}

      {feed.state === "loading" && feed.rollups.length === 0 ? (
        <Caption>Reading from the cluster...</Caption>
      ) : null}

      {feed.state === "ready" && feed.rollups.length > 0 && shown.length === 0 ? (
        <Caption>No domains are tagged with that client.</Caption>
      ) : null}

      {feed.state === "ready" && feed.rollups.length === 0 ? (
        <div className="os-train-empty">
          <p className="os-train-drop-line">No domains yet -- upload a file to start.</p>
          <Button tone="primary" onClick={() => onNavigate("upload")}>
            Go to Upload
          </Button>
        </div>
      ) : null}

      <ul className="os-train-domains" aria-label="Knowledge domains in this cluster">
        {shown.map((rollup) => (
          <li key={rollup.domainId}>
            <DomainCard
              rollup={rollup}
              meta={feed.domains.get(rollup.domainId)}
              accountLabel={accountNameFrom(
                accounts,
                feed.domains.get(rollup.domainId)?.accountId ?? "",
              )}
              open={openDomainId === rollup.domainId}
              onToggle={() =>
                setOpenDomainId((held) => (held === rollup.domainId ? "" : rollup.domainId))
              }
            />
          </li>
        ))}
      </ul>

      {/* NOT LIVE, AND SAYS SO. `v1:knowledge:*` carries no broadcast routing
          rule, so nothing here moves on its own -- and a caption claiming
          liveness the wiring does not provide would be worse than none. */}
      <Caption>
        {feed.readAt === null
          ? "Not read yet."
          : `Read ${formatFreshness(feed.readAt.toISOString(), now)}. Chunk writes are not broadcast, so this re-reads when you come back to the window.`}
      </Caption>
    </div>
  );
}

function DomainCard({
  rollup,
  meta,
  accountLabel,
  open,
  onToggle,
}: {
  rollup: DomainRollup;
  meta: DomainMeta | undefined;
  accountLabel: string;
  open: boolean;
  onToggle: () => void;
}) {
  return (
    <div className="os-train-domain" data-open={open || undefined}>
      <button
        type="button"
        className="os-row"
        data-clickable
        data-current={rollup.unvalidated > 0 || undefined}
        aria-expanded={open}
        onClick={onToggle}
      >
        <Boxes size={16} aria-hidden />
        {/* THE NAME WHERE THERE IS ONE, the id where there is not -- and the
            id keeps the mono face so a reader can tell which they are looking
            at without being told. */}
        {meta && meta.name.trim() !== "" ? (
          <span className="os-row-name">{meta.name}</span>
        ) : (
          <span className="os-row-name os-mono">{rollup.domainId}</span>
        )}
        <AccountChip name={accountLabel} />
        <span className="os-row-state">
          {/* THE THREE PARTS SUM TO THE COUNT, by construction (see
              `rollupDomains`): an unrecognised or absent status counts as
              unvalidated, which is what the concept's default says it is. A
              reader checking the arithmetic is the reader this is for. */}
          <span className="os-caption">{rollup.total} chunks</span>
          <Chip tone={rollup.unvalidated > 0 ? "accent" : "muted"}>
            {rollup.unvalidated} {UNVALIDATED}
          </Chip>
          <Chip tone="muted">
            {rollup.validated} {VALIDATED}
          </Chip>
          <Chip tone="muted">
            {rollup.rejected} {REJECTED}
          </Chip>
        </span>
      </button>
      {open ? <DomainDetail domainId={rollup.domainId} /> : null}
    </div>
  );
}

/**
 * A domain's newest chunks, grouped by the document they came from.
 *
 * ONE PAGE PER STEP, keyset-continued. The count under it names the pages
 * loaded and never a total, for the reason the review queue's does: this walk
 * has no way to know what is behind pages it has not asked for.
 */
function DomainDetail({ domainId }: { domainId: string }) {
  const connection = useOsConnection();
  const [chunks, setChunks] = useState<Chunk[]>([]);
  const [cursor, setCursor] = useState("");
  const [exhausted, setExhausted] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const load = useCallback(
    async (from: string, append: boolean) => {
      if (connection === null) return;
      setBusy(true);
      try {
        const result = await connection.query.documentChunksForDomain(
          { domainId },
          from === "" ? {} : { cursor: from },
        );
        const page = (result.rows() as Row[]).map(chunkFromRow);
        setChunks((held) => (append ? [...held, ...page] : page));
        const next = result.meta()?.cursor ?? "";
        setCursor(next);
        setExhausted(next === "");
        setError("");
      } catch (err: unknown) {
        setError(err instanceof Error ? err.message : String(err));
      } finally {
        setBusy(false);
      }
    },
    [connection, domainId],
  );

  useEffect(() => {
    setChunks([]);
    setCursor("");
    setExhausted(false);
    setError("");
    void load("", false);
  }, [load]);

  const groups = groupChunksByDocument(chunks);

  return (
    <div className="os-train-domain-detail">
      {error === "" ? null : (
        <Notice tone="error" sentence="This domain's chunks did not come back." detail={error}>
          <Button onClick={() => void load("", false)}>Try again</Button>
        </Notice>
      )}

      {groups.map((group) => (
        <section key={group.id === "" ? "corpus" : group.id} aria-label={group.label}>
          <Subhead>{group.id === "" ? CORPUS_GROUP_LABEL : group.label}</Subhead>
          <ul className="os-train-chunk-rows" aria-label={`Chunks from ${group.label}`}>
            {group.chunks.map((chunk) => (
              <li key={chunk.id} className="os-row" data-dim={chunk.superseded || undefined}>
                {/* CLAMPED BY CSS, NOT BY A SLICE. A JS slice at a fixed
                    length cuts mid-word and shows no ellipsis, so the row
                    reads as broken text rather than as shortened text -- and
                    it beats the stylesheet's own `text-overflow` to it, which
                    would have adapted to the window's width. The full text is
                    on `title`. */}
                <span className="os-row-name" title={chunk.text.trim()}>
                  {chunk.text.trim() || "(empty)"}
                </span>
                <span className="os-row-state">
                  <Chip tone="muted">{chunk.source || "source unrecorded"}</Chip>
                  {chunk.sourceRef === "" ? null : (
                    <Chip tone="muted" title={chunk.sourceRef}>
                      {chunk.sourceRef}
                    </Chip>
                  )}
                  <Chip tone={chunk.validationStatus === UNVALIDATED ? "accent" : "muted"}>
                    {chunk.validationStatus}
                  </Chip>
                  {/* Supersession is a SEPARATE axis from validation, and the
                      chip appears only when it is true: a chunk the Trainer
                      Agent retired is out of retrieval whatever its validation
                      status says. */}
                  {chunk.superseded ? (
                    <Chip tone="muted" title={chunk.supersededReason}>
                      superseded
                    </Chip>
                  ) : null}
                </span>
              </li>
            ))}
          </ul>
        </section>
      ))}

      <div className="os-refresh-row">
        <span className="os-caption">
          {chunks.length} chunk{chunks.length === 1 ? "" : "s"} loaded
          {exhausted ? " -- that is all of them" : ""}.
        </span>
        {exhausted ? null : (
          <Button onClick={() => void load(cursor, true)} busy={busy} busyLabel="Reading...">
            Load more
          </Button>
        )}
      </div>

      {/* THE INERT ENTRY POINT. Attaching a domain to an agent rides
          `skill.domainIds` (`dsl/agents/concepts.memql`) and lands with the
          Agents surface -- so the affordance says where it went rather than
          being absent, and it is disabled rather than a button that fails. */}
      <Chips label="Not here yet">
        <span className="os-train-inert" title="Agent attachment rides skill.domainIds and lands with the Agents surface.">
          <Link2Off size={13} aria-hidden /> Attach to agents -- with the Agents surface
        </span>
      </Chips>
    </div>
  );
}
