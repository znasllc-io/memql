import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import {
  Button,
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
import { useOsConnection } from "../../../live/connection";
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

/** The engine's page size. `paginate 50` is a literal in the query -- the
 *  struct-query directive takes nothing else -- so it is a fixed fact here
 *  rather than a parameter this surface chooses. */
const PAGE_SIZE = 50;

interface Page {
  rows: AuditRow[];
  cursor: string;
  state: "unread" | "reading" | "read" | "failed";
  error: string;
  at: Date | null;
}

const EMPTY_PAGE: Page = { rows: [], cursor: "", state: "unread", error: "", at: null };

export function AuditSection() {
  const connection = useOsConnection();

  const [category, setCategory] = useState("");
  const [search, setSearch] = useState("");
  const [page, setPage] = useState<Page>(EMPTY_PAGE);
  const [openId, setOpenId] = useState("");
  // Guards the in-flight case, so a double click on "Load more" cannot walk
  // the cursor twice and lose a page between the two reads.
  const running = useRef(false);

  const load = useCallback(
    async (from: string) => {
      if (connection === null || running.current) return;
      running.current = true;
      setPage((held) => ({ ...held, state: "reading", error: "" }));
      try {
        const result = await connection.query.recentAuditEvents(
          category === "" ? {} : { category },
          // A keyset continuation, not an offset: the cursor is bound to the
          // query's own ordering and carries no server session, so it
          // resolves on whichever replica the front door picks next.
          from === "" ? {} : { cursor: from },
        );
        const rows = result.rows().map(auditFromRow);
        const next = result.meta()?.cursor ?? "";
        setPage((held) => ({
          rows: from === "" ? rows : [...held.rows, ...rows],
          cursor: next,
          state: "read",
          error: "",
          at: new Date(),
        }));
      } catch (err: unknown) {
        setPage((held) => ({
          ...held,
          state: "failed",
          error: err instanceof Error ? err.message : String(err),
        }));
      } finally {
        running.current = false;
      }
    },
    [connection, category],
  );

  // A category change is a DIFFERENT SET, not a filter over this one: the
  // cursor is bound to the query it came from, so carrying it across would
  // continue a walk of the wrong set. The page resets and re-reads.
  useEffect(() => {
    setPage(EMPTY_PAGE);
    setOpenId("");
    if (connection === null) return;
    void load("");
    // `load` closes over `category`, which is what the reset is keyed on.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [connection, category]);

  const visible = useMemo(() => {
    const needle = search.trim().toLowerCase();
    if (needle === "") return page.rows;
    return page.rows.filter((row) =>
      [row.action, row.category, row.actorEmail, row.targetType, row.targetId, row.outcome]
        .join(" ")
        .toLowerCase()
        .includes(needle),
    );
  }, [page.rows, search]);

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
          placeholder="Filter these events"
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
        <Caption>Reading the trail.</Caption>
      ) : null}

      {page.state === "read" && page.rows.length === 0 ? (
        <Caption>No events recorded.</Caption>
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

            {visible.length === 0 && page.rows.length > 0 ? (
              <Caption>
                Nothing loaded here matches that. The filter runs over the {page.rows.length} events
                already read, not over the whole trail -- load more to widen it.
              </Caption>
            ) : null}

            {page.cursor === "" ? (
              <Caption>
                That is the end of the trail{category === "" ? "" : ` for ${category}`}.
              </Caption>
            ) : (
              <Button
                tone="quiet"
                busy={page.state === "reading"}
                busyLabel="Reading"
                onClick={() => void load(page.cursor)}
              >
                Load {PAGE_SIZE} more
              </Button>
            )}
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

      {page.at === null ? null : (
        <Caption>
          Read {page.at.toLocaleTimeString()}, newest first, {PAGE_SIZE} at a time. The trail is not
          live -- an audit row is a record, and a list that rearranged itself while being read would
          be a worse one.
        </Caption>
      )}
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
