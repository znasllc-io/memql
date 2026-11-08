import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { useMemo, useState } from "react";

import {
  Caption,
  Fact,
  Facts,
  Head,
  Notice,
  Refine,
  RecordList,
  RecordRow,
  Select,
  Subhead,
  formatMoment,
  type RefineChip,
} from "../../../kit";
import { usePagedCollection } from "../../../live/useLiveCollection";
import { LoadMore } from "../../../kit/LoadMore";
import { TEXT_DEBOUNCE_MS, useDebouncedValue } from "../../../logs/useDebouncedValue";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { AUDIT_CATEGORIES, auditFromRow, outcomeTone, type AuditRow } from "./rows";

// The audit trail: the cluster's record of security decisions.
//
// ===========================================================================
// THE SECTION'S OWNER FLOOR IS THE ONLY THING THAT CAN STOP THIS SURFACE
// LYING, AND THAT IS THE FINDING
// ===========================================================================
// `v1:identity:auditEvent` declares `@rowAuthz(clusterOwner)`, and row
// admission returns ZERO ROWS, NOT AN ERROR. So a non-owner who reaches this
// query gets `[]` back, with no refusal anywhere in the reply for a surface
// to render -- and an empty list here says "nothing happened" to precisely
// the person who is not allowed to check whether anything did.
//
// There is no client-side repair for that. The engine will not tell us it
// refused, so no notice, no caption and no empty-state wording can be
// CORRECT: whatever we render, we are guessing which of the two states we are
// in. The section's `{ min: "owner" }` floor in settings.ts is the whole
// mechanism -- it means the only people who ever see this list are the people
// whose empty result genuinely means the cluster recorded nothing.
//
// That is why the floor is not a tidy mirror of the engine's tier and must
// not be relaxed to `{ min: "admin" }` for symmetry with the app. An admin
// admitted here would be shown a blank page that reads as a clean cluster.
//
// An owner's genuinely empty result therefore renders "No events recorded",
// which under that floor is a true sentence.

export function AuditSection() {
  const [category, setCategory] = useState("");
  const [search, setSearch] = useState("");
  const [openId, setOpenId] = useState("");
  const needle = useDebouncedValue(search.trim(), TEXT_DEBOUNCE_MS);
  const feed = usePagedCollection<Row>(`cluster:audit:${JSON.stringify([category, needle])}`, connection => ({
    concept: "v1:identity:auditEvent", actions: [],
    seed: async (cursor, signal) => {
      const result = await connection.query.auditEventsBrowse({ category: category || undefined, search: needle }, { cursor, signal });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
  }));
  const page = { rows: feed.snapshot.rows.map(auditFromRow), error: feed.snapshot.error, state: feed.snapshot.error ? "failed" : feed.snapshot.state === "seeding" ? "reading" : "read" };
  const visible = page.rows;

  const open = useMemo(
    () => page.rows.find((row) => row.id === openId) ?? null,
    [page.rows, openId],
  );

  const chips: RefineChip[] =
    category === ""
      ? []
      : [{ id: "category", label: category, onRemove: () => setCategory("") }];

  return (
    <div className="os-cluster">
      <Head title="Audit trail" meta={page.state === "read" && !page.error ? visible.length : undefined}>
        {/* Rule 2: the question lives behind one affordance on the Head line,
            collapsed until asked, with the active constraint as a removable
            chip beside it. Never a standing chip rail over the list. */}
        <Refine
          search={search}
          onSearch={setSearch}
          placeholder="Search audit events"
          chips={chips}
          label="Refine the audit trail"
        >
          <Select
            id="audit-category"
            label="Category"
            value={category}
            onChange={(next) => setCategory(next)}
          >
            <option value="">Every category</option>
            {AUDIT_CATEGORIES.map((c) => (
              <option key={c} value={c}>
                {c}
              </option>
            ))}
          </Select>
        </Refine>
      </Head>

      {page.state === "failed" ? (
        <Notice
          tone="error"
          sentence="The cluster did not answer the audit trail."
          detail={page.error}
        />
      ) : null}

      {page.state === "reading" && page.rows.length === 0 ? (
        <RecordListSkeleton label="Loading the trail" />
      ) : null}

      {page.state === "read" && page.rows.length === 0 ? (
        <Caption>{needle ? "No audit events match this search." : "No events recorded."}</Caption>
      ) : null}

      {page.rows.length === 0 ? null : (
        <div className="os-cluster-body">
          <div className="os-cluster-list">
            <RecordList as="ul" label="Audit events">
              {visible.map((row) => (
                <RecordRow key={row.id}
                    name={row.action || "unstated"}
                    open={openId === row.id}
                    onOpen={() => setOpenId((held) => (held === row.id ? "" : row.id))}
                    secondary={row.actorEmail || row.actorUserId || "no actor recorded"}
                    state={row.outcome} tone={outcomeTone(row.outcome) === "muted" ? "muted" : "warn"}
                  >
                    <span>{row.category}</span>
                    <span className="os-cluster-row-when">{formatMoment(row.occurredAt)}</span>
                    <span className="os-cluster-row-note">
                      {row.targetType === "" ? "" : ` -> ${row.targetType}`}
                      {row.targetId === "" ? "" : ` ${row.targetId}`}
                    </span>
                  </RecordRow>
              ))}
            </RecordList>

            <LoadMore feed={feed} />
          </div>

          {open === null ? null : (
            <div className="os-cluster-detail">
              <Subhead>{open.action || "unstated"}</Subhead>
              <Facts>
                <Fact label="When" value={formatMoment(open.occurredAt)} />
                <Fact label="Category" value={open.category} />
                <Fact label="Outcome" value={open.outcome} />
                <Fact label="Why it failed" value={open.failureReason} />
                <Fact label="Actor" value={open.actorEmail || open.actorUserId} mono />
                <Fact label="Actor role" value={open.actorRole} />
                <Fact label="Target" value={targetReading(open)} mono />
                <Fact label="Detail" value={open.detail} />
                <Fact label="Source IP" value={open.sourceIP} mono />
                <Fact label="User agent" value={open.userAgent} mono />
                <Fact label="Correlation" value={open.correlationId} mono />
              </Facts>
            </div>
          )}
        </div>
      )}

      <Caption>Newest first, ten at a time.</Caption>
    </div>
  );
}

/** The target, as one string. Type and id are separate fields and both can be
 *  blank; an event about nothing in particular says so rather than rendering
 *  a stray arrow. */
function targetReading(row: AuditRow): string {
  const parts = [row.targetType, row.targetId, row.targetEmail].filter((p) => p !== "");
  return parts.join(" ");
}
