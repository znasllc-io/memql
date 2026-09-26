import { useEffect, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Caption, Head, Notice, RefreshButton } from "../../../kit";
import { InfoDetail } from "../../../kit/InfoDetail";
import { Measure } from "../../../kit/MeasureView";
import { OverviewBreakdown, type OverviewSegment } from "../../../kit/Overview";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { useNow } from "../../../kit/useNow";
import { LiveList } from "../../../live/LiveList";
import { useLiveView } from "../../../live/liveView";
import { feedIsBehind } from "../../../live/useLiveCollection";
import { MeshNodePage } from "./MeshNodePage";
import { formatCount, nodeIcon } from "./present";
import {
  groupByType,
  hearingState,
  linkCount,
  meshFingerprint,
  meshNodeFromRow,
  meshSentenceParts,
  reportFigure,
  runningNodes,
  stateSentence,
  stateTone,
  stateWord,
  tally,
  typeLabel,
  type MeshNode,
} from "./rows";
import { useMeshNodes } from "./useMeshNodes";

// Mesh: what every engine replica hears of the events the cluster broadcasts
// (epic memql#5338).
//
// ===========================================================================
// WHY THIS PAGE EXISTS
// ===========================================================================
// A node nobody dialed used to hear nothing, for its whole life, and nothing
// said so: the product bff and the edge had logged six trigger firings against
// hundreds on every other node, and it took an investigation to find. Every
// node now writes what it hears onto its own row once a minute, and this page
// reads those rows. Its one job is the question an operator arrives with --
// is any node deaf, and which -- so that answer comes first, in words, and
// everything else is one click further in.
//
// ===========================================================================
// WHAT IT DELIBERATELY IS NOT
// ===========================================================================
// Not a graph of the whole mesh. The bffs link to nearly everything, so a
// drawing of every link at sixteen replicas is a hairball that hides the one
// node worth finding. A node's OWN links are drawn on its page, where there
// are few enough to read. And not a time series: the report is counts since a
// process started, and a chart of them would invent a history nobody kept.
//
// RULE 11: the node's page REPLACES the list, with a quiet way back, for the
// reason Modules gives -- the page is taller than a side column can hold.

export function MeshSection({
  intent,
  consumeIntent,
}: {
  intent?: { id: string; payload: Record<string, unknown> };
  consumeIntent?: (intentId: string) => void;
}) {
  const feed = useMeshNodes();
  const now = useNow(30_000);
  const [openId, setOpenId] = useState("");

  // A node named by whoever opened this window -- another surface's link to a
  // replica. Consumed by id, so acting on a stale render never eats a newer
  // instruction.
  const intentNode = typeof intent?.payload["nodeId"] === "string" ? (intent.payload["nodeId"] as string) : "";
  useEffect(() => {
    if (intent && intentNode !== "") {
      setOpenId(intentNode);
      consumeIntent?.(intent.id);
    }
  }, [intent, intentNode, consumeIntent]);

  // PROJECT, then keep the running replicas, then order them the way the
  // groups read -- in the VIEW, so the list's caption, its arrival cues and
  // its empty text all describe the rows it actually draws. The running
  // window is judged when a snapshot lands; every running node rewrites its
  // row each minute, so a node that stops falls off within a minute of the
  // window closing on it.
  const view = useLiveView<Row, MeshNode>(feed.source, "mesh", (rows) => {
    const projected = rows.map(meshNodeFromRow).filter((n) => n.id !== "");
    return groupByType(runningNodes(projected, new Date()).running).flatMap((g) => g.nodes);
  });
  const allView = useLiveView<Row, MeshNode>(feed.source, "mesh:all", (rows) =>
    rows.map(meshNodeFromRow).filter((n) => n.id !== ""),
  );
  const shown = view?.snapshot.rows ?? [];
  const everything = allView?.snapshot.rows ?? [];
  const hidden = runningNodes(everything, now).hidden;

  const byId = useMemo(() => new Map(everything.map((n) => [n.id, n])), [everything]);
  const t = useMemo(() => tally(shown), [shown]);
  const firstOfType = useMemo(() => {
    const first = new Set<string>();
    const seen = new Set<string>();
    for (const n of shown) {
      if (seen.has(n.nodeType)) continue;
      seen.add(n.nodeType);
      first.add(n.id);
    }
    return first;
  }, [shown]);
  const typeCounts = useMemo(() => {
    const counts = new Map<string, number>();
    for (const n of shown) counts.set(n.nodeType, (counts.get(n.nodeType) ?? 0) + 1);
    return counts;
  }, [shown]);

  const open = openId === "" ? null : (byId.get(openId) ?? null);
  if (open !== null) {
    return (
      <MeshNodePage
        node={open}
        byId={byId}
        onBack={() => setOpenId("")}
        onOpen={(id) => setOpenId(id)}
      />
    );
  }

  const live = feed.source !== null && view?.snapshot.state === "live";
  const segments: OverviewSegment[] = [
    { label: "Hearing", count: t.hearing, tone: "good" },
    { label: "Not hearing", count: t.notHearing, tone: "warn" },
    { label: "Quiet", count: t.quiet, tone: "warn" },
    { label: "Sends only", count: t.sendsOnly },
    { label: "Starting", count: t.starting },
    { label: "Not reported", count: t.notReported },
  ];
  const attention = t.notHearing + t.quiet;

  return (
    <div className="os-cluster os-cluster-mesh">
      <Head title="Mesh" meta={shown.length === 0 ? null : `${shown.length} running ${shown.length === 1 ? "node" : "nodes"}`}>
        {/* OFFERED ONLY WHEN THE FEED IS BEHIND. The list is live; a refresh
            beside a live list says "this may be stale" about rows that arrive
            on their own. When the feed says it IS behind, the same control is
            exactly right, and its appearance is itself the signal. */}
        {feedIsBehind(feed.state) ? <RefreshButton label="Reconnect" onClick={feed.reseed} /> : null}
      </Head>

      <div className="os-cluster-mesh-context">
        <p>What each node hears of the events the cluster broadcasts.</p>
        <InfoDetail title="Mesh">
          <p className="os-caption">
            Nodes pass the cluster&apos;s broadcast events to each other over the streams between
            them, in both directions, whichever node opened the stream. A node linked to the mesh
            by any stream hears every event once.
          </p>
          <p className="os-caption">
            Each node reports what it has heard since it started, on its own row, once a minute.
            So every figure here is up to a minute old, and dated.
          </p>
          <p className="os-caption">
            Identity sends its own events and takes none, by design. A node that has not reported
            is shown as not reported, never as hearing nothing.
          </p>
        </InfoDetail>
      </div>

      {feed.error ? (
        <Notice
          tone="error"
          sentence="The node rows could not be updated."
          next={shown.length > 0 ? "Showing the last rows that arrived." : "Nothing was loaded."}
          detail={feed.error}
        />
      ) : null}

      {/* THE ANSWER, FIRST. A current distribution -- never a history -- and
          the one sentence that names a node needing a look when there is one.
          Absent until the feed is live: a band drawn over an empty seed would
          announce "no node hears" about a page that has not loaded. */}
      {live && shown.length > 0 ? (
        // A PLAIN CONTAINER, not a second region: the breakdown is already
        // the "Delivery" region, and a landmark inside a landmark of the
        // same name is announced twice.
        <div className="os-cluster-mesh-band">
          <OverviewBreakdown title="Delivery" segments={segments} />
          <p className={attention > 0 ? "os-cluster-row-attention os-cluster-mesh-verdict" : "os-cluster-fact os-cluster-mesh-verdict"}>
            {/* A NODE THE SENTENCE NAMES OPENS FROM THE SENTENCE. The reader
                who has just been told which node is deaf wants that node's
                page next, and should not have to find its row to get there. */}
            {meshSentenceParts(t).map((part, i) =>
              typeof part === "string" ? (
                part
              ) : (
                <button
                  key={`${part.node}:${i}`}
                  type="button"
                  className="os-cluster-mesh-name os-mono"
                  onClick={() => setOpenId(part.node)}
                  aria-label={`Open ${part.node}`}
                >
                  {part.node}
                </button>
              ),
            )}
          </p>
        </div>
      ) : null}

      <RecordList label="Nodes">
        <LiveList<MeshNode>
          source={view}
          rowId={(n) => n.id}
          fingerprint={meshFingerprint}
          label="Running nodes"
          emptyText="No node has written its row in the last five minutes."
          renderRow={(n) => (
            <>
              {firstOfType.has(n.id) ? (
                <p className="os-cluster-mesh-group">
                  <span>{typeLabel(n.nodeType)}</span>
                  <span className="os-cluster-mesh-group-count">{typeCounts.get(n.nodeType) ?? 0}</span>
                </p>
              ) : null}
              <MeshRow node={n} onOpen={() => setOpenId(n.id)} />
            </>
          )}
        />
      </RecordList>

      {hidden === 0 ? null : (
        <Caption>
          {hidden} {hidden === 1 ? "node has" : "nodes have"} stopped or not written a row for five
          minutes, and {hidden === 1 ? "is" : "are"} not listed.
        </Caption>
      )}
    </div>
  );
}

function MeshRow({ node, onOpen }: { node: MeshNode; onOpen: () => void }) {
  const state = hearingState(node);
  const links = linkCount(node);
  const word = stateWord(state);
  return (
    <RecordRow
      icon={nodeIcon(node.nodeType)}
      name={<span className="os-mono">{node.id}</span>}
      secondary={node.address || undefined}
      state={word}
      tone={stateTone(state)}
      stateTitle={stateSentence(node, state)}
      current={state === "hearing"}
      label={`Open ${node.id}, ${word.toLowerCase()}`}
      onOpen={onOpen}
    >
      {node.report === null ? null : (
        <>
          <span>
            {links} {links === 1 ? "link" : "links"}
          </span>
          {node.report.receives ? (
            <span>
              <Measure figure={reportFigure(node, (r) => r.heard)} format={formatCount} /> heard
            </span>
          ) : null}
        </>
      )}
    </RecordRow>
  );
}
