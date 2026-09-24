import type { ReactNode } from "react";

import { Caption, Fact, Facts, Head, Panel, Subhead } from "../../../kit";
import { absent, type Figure } from "../../../kit/measure";
import { formatMoment } from "../../../kit/format";
import { Measure } from "../../../kit/MeasureView";
import { formatCount, nodeIcon } from "./present";
import {
  hearingState,
  linkCount,
  linkSides,
  needsAttention,
  reportFigure,
  spanOf,
  stateSentence,
  stateTone,
  stateWord,
  typeLabel,
  type LinkedPeer,
  type MeshNode,
} from "./rows";

// One node's page: what it hears, and what links it to the mesh.
//
// ===========================================================================
// THE LINKS ARE DRAWN, AND THEY ARE THE ONLY THING THAT IS
// ===========================================================================
// A node's links are the one relationship on this page, and the question they
// answer -- what connects this node, and who opened each stream -- is a
// question about SHAPE: an edge with one link to one bff reads differently
// from a bff with eleven. So they are laid out as a patch bay: the node in the
// middle, the streams it opened on one side, the streams opened to it on the
// other. Every peer is a button to that peer's own page, and carries that
// peer's own state, so "linked only to a node that is itself deaf" is visible
// without opening anything. It is the list of links, said once, arranged to
// show direction -- not a second drawing of it.
//
// A peer linked both ways is on BOTH sides. Two streams exist, and a page that
// merged them would hide the duplicate dial that is often the reason.
//
// Figures belong to the node's latest report; Last heard is relative to it.

export function MeshNodePage({
  node,
  byId,
  onBack,
  onOpen,
}: {
  node: MeshNode;
  byId: ReadonlyMap<string, MeshNode>;
  onBack: () => void;
  onOpen: (id: string) => void;
}) {
  const state = hearingState(node);
  const r = node.report;
  const sides = linkSides(node, byId);
  const peers = linkCount(node);
  // Identity takes nothing, so what the panel holds is what it SENDS.
  const panelTitle = r !== null && !r.receives ? "What it sends" : "What it hears";

  return (
    <div className="os-cluster os-cluster-mesh">
      <Head title={node.id} meta={typeLabel(node.nodeType)} back={{ label: "Mesh", onSelect: onBack }} />

      <div className="os-cluster-mesh-verdict-line">
        <span className="os-record-status" data-tone={stateTone(state)}>
          {stateWord(state)}
        </span>
        {needsAttention(state) ? <p className="os-cluster-row-attention">
          {stateSentence(node, state)}
        </p> : null}
      </div>

      <Panel label={panelTitle}>
        <Subhead>{panelTitle}</Subhead>
        <Facts>
          {r === null || r.receives ? (
            <Fact label="Heard" value={<Counted node={node} pick={(x) => x.heard} unit="events" />} />
          ) : null}
          {r === null || r.receives ? (
            <Fact label="Last heard" value={lastHeardValue(node)} title={r?.lastHeardAt || undefined} />
          ) : null}
          <Fact label="Put on the mesh" value={<Counted node={node} pick={(x) => x.originated} unit="events" />} />
          {r === null || r.receives ? (
            <Fact label="Relayed" value={<Counted node={node} pick={(x) => x.relayed} unit="events" />} />
          ) : null}
          {r === null || r.receives ? (
            <Fact label="Repeats set aside" value={<Counted node={node} pick={(x) => x.duplicates} unit="copies" />} />
          ) : null}
          <Fact label="Copies dropped" value={<Counted node={node} pick={(x) => x.dropped} unit="copies" />} />
        </Facts>
        {r !== null && r.dropped.kind === "measured" && r.dropped.value > 0 ? (
          <Caption>
            A dropped copy is one a peer&apos;s stream could not take because the peer was not reading
            fast enough. That peer missed the event unless another link carried it.
          </Caption>
        ) : null}
        {r !== null && r.hopLimited.kind === "measured" && r.hopLimited.value > 0 ? (
          <p className="os-cluster-row-attention">
            {formatCount(r.hopLimited.value)} {r.hopLimited.value === 1 ? "copy" : "copies"} reached the
            hop limit here and went no further. A copy travels at most sixteen links, so this mesh has
            a path longer than that.
          </p>
        ) : null}
      </Panel>

      <Panel label="Links">
        <div className="os-cluster-mesh-links-head">
          <Subhead>Links</Subhead>
          {r === null ? null : (
            <span className="os-head-meta">
              {peers} {peers === 1 ? "peer" : "peers"}
            </span>
          )}
        </div>
        {r === null ? (
          <Caption>This node has not reported its links.</Caption>
        ) : peers === 0 ? (
          <Caption>No stream links this node to any other.</Caption>
        ) : (
          <div className="os-cluster-mesh-bay">
           <div className="os-cluster-mesh-bay-grid">
            <PeerSide
              side="opened"
              title="Streams it opened"
              empty="It opened no streams."
              peers={sides.opened}
              onOpen={onOpen}
            />
            <div className="os-cluster-mesh-hub" aria-hidden>
              <span className="os-cluster-mesh-hub-mark">{nodeIcon(node.nodeType)}</span>
            </div>
            <PeerSide
              side="accepted"
              title="Streams opened to it"
              empty="No node opened a stream to it."
              peers={sides.accepted}
              onOpen={onOpen}
            />
           </div>
          </div>
        )}
        {r !== null && !r.receives && peers > 0 ? (
          <Caption>These streams carry identity&apos;s own events out. Nothing is sent to it.</Caption>
        ) : null}
      </Panel>
    </div>
  );
}

function PeerSide({
  side,
  title,
  empty,
  peers,
  onOpen,
}: {
  side: "opened" | "accepted";
  title: string;
  empty: string;
  peers: readonly LinkedPeer[];
  onOpen: (id: string) => void;
}) {
  return (
    <div className="os-cluster-mesh-side" data-side={side}>
      <p className="os-cluster-mesh-side-title">{title}</p>
      {peers.length === 0 ? (
        <p className="os-cluster-mesh-side-empty">{empty}</p>
      ) : (
        <ul className="os-cluster-mesh-peers">
          {peers.map((p) => (
            <li key={p.id} className="os-cluster-mesh-peer-slot" data-side={side}>
              <Peer peer={p} onOpen={onOpen} />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function Peer({ peer, onOpen }: { peer: LinkedPeer; onOpen: (id: string) => void }) {
  // A PEER THIS PAGE DOES NOT LIST has nothing to open: it stopped, or never
  // wrote a row. It is still a link this node reported, so it is drawn -- as
  // text, not as a control that would open nothing (DESIGN.md rule 12).
  if (peer.node === null) {
    return (
      <span className="os-cluster-mesh-peer" data-unlisted>
        <span className="os-cluster-mesh-peer-id os-mono">{peer.id}</span>
        <span className="os-cluster-mesh-peer-type">{peer.type ? `${typeLabel(peer.type)}, not listed` : "not listed"}</span>
      </span>
    );
  }
  const state = hearingState(peer.node);
  return (
    <button
      type="button"
      className="os-cluster-mesh-peer"
      onClick={() => onOpen(peer.id)}
      aria-label={`Open ${peer.id}, ${typeLabel(peer.type || peer.node.nodeType)}, ${stateWord(state).toLowerCase()}`}
      title={stateSentence(peer.node, state)}
    >
      <span className="os-cluster-mesh-peer-dot" data-tone={stateTone(state)} aria-hidden />
      <span className="os-cluster-mesh-peer-id os-mono">{peer.id}</span>
      <span className="os-cluster-mesh-peer-type">{typeLabel(peer.type || peer.node.nodeType)}</span>
    </button>
  );
}

function Counted({
  node,
  pick,
  unit,
}: {
  node: MeshNode;
  pick: (r: NonNullable<MeshNode["report"]>) => Figure;
  unit: string;
}) {
  return (
    <span className="os-cluster-mesh-count">
      <Measure figure={reportFigure(node, pick)} format={formatCount} suffix={` ${unit}`} />
    </span>
  );
}

function lastHeardValue(node: MeshNode): ReactNode {
  const r = node.report;
  // No report is an ABSENT answer, drawn the way every other absent figure on
  // this page is -- not "Nothing yet", which is what a report says of a node
  // that has heard nothing.
  if (r === null) return <Measure figure={absent("unmeasured")} />;
  if (r.lastHeardAt === "") return "Nothing yet";
  const heard = Date.parse(r.lastHeardAt);
  const reported = Date.parse(node.lastSeen);
  if (!Number.isFinite(heard) || !Number.isFinite(reported)) return formatMoment(r.lastHeardAt);
  const seconds = Math.max(0, Math.round((reported - heard) / 1000));
  if (seconds < 60) return `${seconds} ${seconds === 1 ? "second" : "seconds"} before the report`;
  return `${spanOf(Math.floor(seconds / 60))} before the report`;
}
