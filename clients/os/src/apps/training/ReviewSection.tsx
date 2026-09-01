import { useMemo } from "react";
// `Check` is aliased: the kit exports a control by that name, and two
// different `Check`s in one file is a rename waiting to go wrong.
import { Check as CheckIcon, FileStack, X } from "lucide-react";

import { Button, Caption, Chip, Chips, Head, Notice, ProvenanceDot, Subhead } from "../../kit";
import { REJECTED, VALIDATED, sourceMeaning, type ChunkDecision } from "./concepts";
import { groupChunksByDocument, type Chunk, type ChunkGroup } from "./rows";
import type { ChunkDecisions } from "./actions";
import type { ReviewQueue } from "./useReviewQueue";

// What MemQL learned, held for a person before it enters retrieval.
//
// ===========================================================================
// THE REVIEWABLE UNIT IS THE CHUNK, AND THAT IS A FACT ABOUT THIS ENGINE
// ===========================================================================
// The owner's scenario is entity-level -- "MemQL identified a new customer,
// should I add it?" -- and the concepts that would carry it
// (`domainEntitySchema`, `entityIndex`, `validationEvent`) are declared in no
// `.memql` file in this tree. What IS declared is
// `documentChunk.validationStatus`, defaulting to `unvalidated`, where
// `validated` means ingestible into retrieval and `rejected` means
// soft-deleted from it.
//
// So this section reviews chunks, and says so. Rendering invented entity cards
// over chunk rows would be a surface that agreed with the ask and disagreed
// with the cluster.

export function ReviewSection({
  queue,
  decisions,
  onDecide,
  domainsError,
}: {
  queue: ReviewQueue;
  decisions: ChunkDecisions;
  onDecide: (chunkId: string, status: ChunkDecision) => void;
  /** The domain read's own failure, if it failed. The queue cannot walk what
   *  it was never told about, and reporting an empty queue in that case would
   *  be a wrong answer rather than a missing one. */
  domainsError: string;
}) {
  const groups = useMemo(() => groupChunksByDocument(queue.chunks), [queue.chunks]);
  const awaiting = queue.chunks.filter((c) => c.validationStatus === "unvalidated").length;

  return (
    <div className="os-app-stack">
      <Head title="Review">
        <Button onClick={queue.reload}>Re-read</Button>
      </Head>

      {domainsError !== "" ? (
        <Notice
          tone="error"
          sentence="This cluster did not return its knowledge domains, so there is nothing to walk."
          next="The queue below is empty because the read failed, not because there is no work."
          detail={domainsError}
        />
      ) : null}

      {queue.error !== "" ? (
        <Notice
          tone="error"
          sentence="Reading the queue failed part-way."
          next="Anything already loaded is still shown and still decidable."
          detail={queue.error}
        >
          <Button onClick={queue.loadMore}>Try again</Button>
        </Notice>
      ) : null}

      {groups.length === 0 && queue.state === "ready" ? (
        <Caption>Nothing awaiting review.</Caption>
      ) : null}

      {queue.state === "loading" && groups.length === 0 ? (
        <Caption>Reading from the cluster...</Caption>
      ) : null}

      {groups.map((group) => (
        <ReviewGroup
          key={group.id === "" ? "corpus" : group.id}
          group={group}
          decisions={decisions}
          onDecide={onDecide}
        />
      ))}

      {/* THE COUNT NAMES THE PAGES, NEVER A TOTAL. The walk reads a domain's
          newest fifty at a time and has no way to know how many chunks are
          behind the pages it has not asked for -- so "12 awaiting review" here
          means twelve in what has been loaded, and the sentence says which. */}
      <div className="os-refresh-row">
        <span className="os-caption">
          {awaiting} awaiting review in {queue.pagesRead} page{queue.pagesRead === 1 ? "" : "s"}{" "}
          read
          {queue.exhausted ? " -- every page has been walked" : ""}.
        </span>
        {queue.exhausted ? null : (
          <Button onClick={queue.loadMore} busy={queue.state === "loading"} busyLabel="Reading...">
            Load more
          </Button>
        )}
      </div>
    </div>
  );
}

function ReviewGroup({
  group,
  decisions,
  onDecide,
}: {
  group: ChunkGroup;
  decisions: ChunkDecisions;
  onDecide: (chunkId: string, status: ChunkDecision) => void;
}) {
  return (
    <section className="os-train-group" aria-label={group.label}>
      <Subhead>
        <FileStack size={14} aria-hidden /> {group.label}
      </Subhead>
      {/* The corpus group is the standing pile rather than one upload, and a
          reader should not have to infer that from an empty-looking id. */}
      {group.id === "" ? (
        <Caption>
          Chunks with no source document -- seeded content, chat augments and Trainer Agent
          output.
        </Caption>
      ) : null}
      <ul className="os-train-cards" aria-label={`Chunks from ${group.label}`}>
        {group.chunks.map((chunk) => (
          <li key={chunk.id}>
            <ChunkCard
              chunk={chunk}
              busy={decisions.busyChunkId === chunk.id}
              refusal={decisions.refusals[chunk.id] ?? ""}
              onDecide={onDecide}
            />
          </li>
        ))}
      </ul>
    </section>
  );
}

function ChunkCard({
  chunk,
  busy,
  refusal,
  onDecide,
}: {
  chunk: Chunk;
  busy: boolean;
  refusal: string;
  onDecide: (chunkId: string, status: ChunkDecision) => void;
}) {
  const decided = chunk.validationStatus !== "unvalidated";

  // A DECIDED CARD COLLAPSES IN PLACE rather than disappearing. A list that
  // removes the thing somebody just clicked moves everything under the cursor,
  // so the next click lands on a card they never read -- and in a queue of
  // near-identical cards that is a decision they did not make.
  if (decided) {
    return (
      <div className="os-train-card" data-decided={chunk.validationStatus}>
        <span className="os-train-card-compact">
          {/* The dot's own language: an approved chunk is reachable by
              retrieval, a rejected one is not. That is literally what the two
              statuses mean, so the shell's word for it is the right one. */}
          <ProvenanceDot
            tone={chunk.validationStatus === VALIDATED ? "reachable" : "unreachable"}
          />
          <span className="os-mono">{chunk.validationStatus}</span>
          <span className="os-caption">{firstLine(chunk.text)}</span>
        </span>
      </div>
    );
  }

  return (
    <div className="os-train-card" tabIndex={0} aria-label={`Chunk ${chunk.id}`}>
      <p className="os-train-card-text">{clamp(chunk.text)}</p>

      <Chips label="Provenance">
        {/* The enum value in the data voice -- it is the word somebody greps
            for -- and its meaning beside it. An unrecognised member renders as
            itself with no sentence: a newer engine writing a seventh source is
            a thing to show, not to erase. */}
        <Chip tone="accent" title={sourceMeaning(chunk.source)}>
          {chunk.source || "source unrecorded"}
        </Chip>
        {chunk.sourceRef === "" ? null : (
          <Chip tone="muted" title={chunk.sourceRef}>
            {chunk.sourceRef}
          </Chip>
        )}
        {chunk.sourceTopic === "" ? null : <Chip tone="muted">{chunk.sourceTopic}</Chip>}
        {chunk.tokenCount > 0 ? <Chip tone="muted">{chunk.tokenCount} tokens</Chip> : null}
      </Chips>

      <div className="os-train-card-actions">
        <Button
          tone="primary"
          busy={busy}
          busyLabel="Writing..."
          onClick={() => onDecide(chunk.id, VALIDATED)}
          ariaLabel={`Approve chunk ${chunk.id}`}
        >
          <CheckIcon size={14} aria-hidden /> Approve
        </Button>
        <Button
          busy={busy}
          busyLabel="Writing..."
          onClick={() => onDecide(chunk.id, REJECTED)}
          ariaLabel={`Reject chunk ${chunk.id}`}
        >
          <X size={14} aria-hidden /> Reject
        </Button>
      </div>

      {/* IN SURFACE, ON THE CARD THAT PRODUCED IT. A shared error line would
          put one card's refusal under another in a list of near-identical
          cards, which names a failure somebody did not cause. */}
      {refusal === "" ? null : <p className="os-notice-detail os-mono">{refusal}</p>}
    </div>
  );
}

const CLAMP_CHARS = 480;

/** The card shows enough to decide on; the whole chunk is not the point. An
 *  ellipsis is appended only when something was actually cut. */
export function clamp(text: string): string {
  const trimmed = text.trim();
  if (trimmed.length <= CLAMP_CHARS) return trimmed;
  return `${trimmed.slice(0, CLAMP_CHARS).trimEnd()}...`;
}

/** What a collapsed card still says: enough to recognise which one it was. */
export function firstLine(text: string): string {
  const line = text.trim().split("\n", 1)[0] ?? "";
  return line.length <= 90 ? line : `${line.slice(0, 90).trimEnd()}...`;
}
