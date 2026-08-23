import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";

import { Band } from "../ui";
import { MapSurface } from "./map/MapSurface";
import { NodeDetail } from "./NodeDetail";
import { CompletionCard } from "./map/CompletionCard";
import { useGoalContext } from "./GoalLayout";
import { events as buildEvents } from "./scene/events";
import { layout } from "./scene/layout";
import { scene as sceneAt } from "./scene/scene";
import { receipt } from "./scene/receipt";
import { FULL_MOTION } from "./map/motion";
import { EventList } from "./replay/EventList";
import { Scrubber } from "./replay/Scrubber";
import {
  BEFORE_FIRST,
  atForIndex,
  clampIndex,
  indexForAt,
  isLive,
  phaseMarks,
  sceneMomentFor,
  stepMs,
} from "./replay/timeline";
import { AT_PARAM, replayNodePath, replayPath } from "./urls";

// /nexus/:planId/replay -- how the goal got here.
//
// ===========================================================================
// THE SAME SCENE, FILTERED BY TIME
// ===========================================================================
// There is no second renderer here and no second layout. Replay computes
// `scene(world, at)` and hands it to the SAME MapSurface the Map page uses,
// which is why a replayed goal looks exactly like the live one and why a
// change to the map is a change to both. That is the whole payoff of keeping
// the scene library pure.
//
// ===========================================================================
// A MOMENT IS A URL
// ===========================================================================
// `?at=<rfc3339>` pins the scrub position, written back as the scrubber moves
// and read on a cold load. DEBOUNCED and written with `replace`, because a
// history entry per drag frame turns the back button into a rewind control
// nobody asked for.
//
// The end of the timeline deliberately writes NO `?at=`: the scrubber parked
// at the last event means "show me the goal as it is", and a goal that is
// still running keeps moving. Pinning the last event's timestamp would freeze
// a live goal at the moment the page happened to load.
//
// ===========================================================================
// REDUCED MOTION NEEDS NOTHING HERE, AND THAT IS THE POINT
// ===========================================================================
// Design D7: scrubbing SNAPS and playback fades. Both fall out of the canvas
// already reading the preference for itself -- a scrub recomputes the scene
// and nothing tweens between two positions in the first place, and an arrival
// under reduced motion is a fade. This page deliberately adds no second
// reading of the preference: two components each deciding what reduced motion
// means is how one of them ends up disagreeing.

export function ReplayPage(): ReactNode {
  const { world, planId } = useGoalContext();
  const { nodeId = "" } = useParams();
  const navigate = useNavigate();
  const [search, setSearch] = useSearchParams();

  const timeline = useMemo(() => buildEvents(world), [world]);
  const [index, setIndex] = useState(() => {
    const at = search.get(AT_PARAM) ?? "";
    return at === "" ? timeline.length - 1 : indexForAt(timeline, at);
  });
  const [playing, setPlaying] = useState(false);
  const [speed, setSpeed] = useState(1);
  // Bumped whenever the scrub jumps BACKWARDS, so nodes that un-arrive and
  // arrive again replay their animation instead of being remembered as
  // already-arrived (see NexusCanvas's useArrivals).
  const [arrivalEpoch, setArrivalEpoch] = useState(0);
  const previousIndex = useRef(index);

  const live = isLive(timeline, index);
  const moment = sceneMomentFor(timeline, index, live);
  const scened = useMemo(() => sceneAt(world, moment), [world, moment]);
  const scene = useMemo(() => layout(scened), [scened]);
  // Node resolution runs over the WHOLE world, not the scened one: a link to
  // a node that has not arrived at this moment still names a real row, and
  // refusing to open it would make a shared URL depend on where the sender's
  // scrubber happened to be.
  const index_ = useMemo(() => layout(world, { clusterThreshold: Number.POSITIVE_INFINITY }), [world]);
  const card = useMemo(() => receipt(scened), [scened]);
  const marks = useMemo(
    () => phaseMarks(timeline, scene.nodes, layout(world).phases.map((phase) => phase.name)),
    [timeline, scene, world],
  );

  function move(next: number): void {
    const clamped = clampIndex(timeline, next);
    if (clamped < previousIndex.current) setArrivalEpoch((n) => n + 1);
    previousIndex.current = clamped;
    setIndex(clamped);
    setPlaying(false);
  }

  // ---- playback ---------------------------------------------------------
  useEffect(() => {
    if (!playing || timeline.length === 0) return;
    if (index >= timeline.length - 1) {
      setPlaying(false);
      return;
    }
    const timer = setTimeout(() => {
      previousIndex.current = index + 1;
      setIndex(index + 1);
    }, stepMs(speed, FULL_MOTION.arrivalSeconds));
    return () => clearTimeout(timer);
  }, [playing, index, speed, timeline.length]);

  // ---- the URL ----------------------------------------------------------
  // Debounced, `replace`, and absent at the live end. See the header.
  useEffect(() => {
    const want = live ? "" : atForIndex(timeline, index);
    const have = search.get(AT_PARAM) ?? "";
    if (want === have) return;
    const timer = setTimeout(() => {
      const next = new URLSearchParams(search);
      if (want === "") next.delete(AT_PARAM);
      else next.set(AT_PARAM, want);
      setSearch(next, { replace: true });
    }, 200);
    return () => clearTimeout(timer);
  }, [index, live, timeline, search, setSearch]);

  return (
    <div className="flex flex-col gap-5">
      <div className="grid gap-4 lg:grid-cols-[minmax(0,2fr)_minmax(0,1fr)]">
        <div className="flex min-w-0 flex-col gap-3">
          <MapSurface
            world={scened}
            scene={scene}
            selectedNodeId={nodeId}
            onSelect={(id) => navigate(replayNodePath(planId, id, live ? "" : atForIndex(timeline, index)))}
            onExpandPhase={() => {
              // Expanding a collapsed phase mid-replay would change what the
              // scrubber's positions mean while it is moving. The Map is
              // where a phase is opened up; Replay draws whatever the layout
              // decides for the moment it is showing.
            }}
            arrivalEpoch={arrivalEpoch}
            showReceipt={false}
          />
          <Scrubber
            count={timeline.length}
            index={index}
            onIndex={move}
            playing={playing}
            onPlaying={setPlaying}
            speed={speed}
            onSpeed={setSpeed}
            marks={marks}
            atLabel={index < 0 ? "before the goal" : atForIndex(timeline, index)}
            live={live}
          />
        </div>

        <Band title="Events" headingLevel="h2" meta={`${timeline.length}`}>
          <EventList
            events={timeline}
            index={index}
            onIndex={move}
            onOpen={(event) =>
              navigate(replayNodePath(planId, event.nodeId, live ? "" : atForIndex(timeline, index)))
            }
          />
        </Band>
      </div>

      {/* The receipt is present AT THE MOMENT OF SUCCESS, not only at the end
          (memql#4376): scrubbing back past the goal's completion takes it
          away again, which is the honest thing for a card that records an
          outcome that had not happened yet. */}
      {card === null ? null : <CompletionCard receipt={card} />}

      {nodeId === "" ? null : (
        <NodeDetail
          scene={index_}
          nodeId={nodeId}
          onClose={() => navigate(replayPath(planId, live ? "" : atForIndex(timeline, index)))}
        />
      )}
    </div>
  );
}

// Re-exported so the scrubber's "before the first event" sentinel has one
// definition and the page's own tests can name it.
export { BEFORE_FIRST };
