import { useCallback, useEffect, useRef, useState } from "react";

import { Caption, Fact, Facts, useNow } from "../../../../kit";
import { formatFreshness, formatMoment } from "../../../../kit/format";
import { useOsConnection } from "../../../../live/connection";
import { useDeployablesSettings } from "../../settingsContext";
import type { SiteRow } from "../../rows";
import {
  DEFAULT_TRAFFIC_WINDOW,
  SYSTEM_OWNED_TRAFFIC_NOTE,
  TRAFFIC_WINDOWS,
  countLabel,
  fetchTraffic,
  seriesSummary,
  unmeasuredSentence,
  windowLabel,
  windowSpec,
  type TrafficBucket,
  type TrafficReading,
  type TrafficWindow,
} from "../../traffic";

// Is anybody using this deployable, and is it healthy (epic memql#4906).
//
// Mounted on the deployable's own detail beside the lifecycle and the domains
// panel. When the Compose rail lands it moves onto the Live stop, which is
// where the program record puts it -- the component does not care: it reads a
// site row and renders a section, and the rail's stop body and this panel are
// the same container.
//
// ===========================================================================
// THE SHAPE IS THE PICTURE; THE NUMBERS ARE WORDS BESIDE IT
// ===========================================================================
// One strip of bucket columns and four labelled facts, and the division of
// labour between them is deliberate. The totals are what a person came for
// and they belong in the shell's own fact grammar, where every other panel in
// this app puts a labelled value. What a table cannot say is the SHAPE --
// steady, spiky, or stopped three hours ago -- so that is the one thing drawn,
// and it is drawn small.
//
// It is deliberately not a row of stat tiles with a sparkline in each: four
// numbers about one deployable are four facts, not four cards, and a second
// container language beside Panel is exactly what DESIGN.md rule 8 removes.
//
// ===========================================================================
// ONE SERIES, ONE HUE, AND ERRORS ARE A STATUS RATHER THAN A SERIES
// ===========================================================================
// Requests are the series and they are the accent: one series needs no legend
// because the thing above it names what it is. Errors are NOT a second series
// -- they are a state, so they wear the shell's error token and they wear it
// in a band of their own beneath the baseline rather than as a stacked
// segment. At a strip thirty-four pixels tall a stacked segment would need a
// separating gap wider than the mark it separates.
//
// Colour is never the only carrier: the error band is accompanied by the
// Errors fact, by each column's own title, and by the series summary a screen
// reader gets.
//
// ===========================================================================
// AN ABSENT FIGURE AND A ZERO ARE DIFFERENT ANSWERS
// ===========================================================================
// A window nothing measured says so in words and draws no strip at all. A
// window with requests and no errors draws the strip and says 0 errors. The
// two must never look alike: one means nobody came, the other means nothing
// was recording, and they send a person to different places.
//
// ===========================================================================
// IT REFRESHES ON THE WINDOW'S OWN CADENCE, NEVER THROUGH THE ARRIVAL CUE
// ===========================================================================
// The cue announces a change to a row somebody is looking at. A figure that
// moves on a timer is the opposite kind of thing -- it would fire forever, on
// nothing anybody did, which is the strobe the OS README's heartbeat rule
// exists to prevent. So this polls on the bucket's own clock: a minute for an
// hour-long window, five for a day, fifteen for a week. Asking more often
// than a bucket can close is asking a question whose answer cannot have
// changed.

/** How tall the strip stands. Small on purpose: it is a shape, not a chart. */
const STRIP_HEIGHT = 34;

export function TrafficPanel({ site }: { site: SiteRow }) {
  const connection = useOsConnection();
  const now = useNow();
  // THE WINDOW IS A REMEMBERED CHOICE, NOT A PER-PAGE ONE (the app's settings
  // document). Somebody troubleshooting moves between deployables asking the
  // same question, so re-picking the window on each one is exactly the
  // clicking this is here to stop -- and the read is the settings document
  // rather than local state so it survives the page being closed.
  //
  // The DEFAULT is the hour; a person who picks otherwise keeps their pick.
  const { settings, update } = useDeployablesSettings();
  const window = settings.trafficWindow ?? DEFAULT_TRAFFIC_WINDOW;
  const setWindow = (next: TrafficWindow) => update({ trafficWindow: next });
  const [reading, setReading] = useState<TrafficReading | null>(null);
  const [state, setState] = useState<"loading" | "ready" | "failed">("loading");
  const [failure, setFailure] = useState("");
  // The instant the figure was read, so the surface can say when it looked --
  // an on-demand read that does not print its own age reads as live.
  const [readAt, setReadAt] = useState("");

  // A DRAFT DEPLOYABLE HAS NEVER SERVED ANYTHING, and a strip of empty
  // buckets under a sentence about nobody visiting would be an answer to a
  // question it is too early to ask. The stop simply is not there yet.
  const servedEver = site.status !== "draft";
  const siteId = site.id;
  const systemOwned = site.systemOwned;

  // A ref, not state, so a slow read that lands after the window changed is
  // discarded rather than painting the old window's figure under the new
  // window's pills.
  const asked = useRef(0);

  const read = useCallback(async () => {
    if (connection === null || siteId === "" || systemOwned || !servedEver) return;
    const mine = ++asked.current;
    try {
      const { reading: next } = await fetchTraffic(connection.query, siteId, window, new Date());
      if (mine !== asked.current) return;
      setReading(next);
      setState("ready");
      setFailure("");
      setReadAt(new Date().toISOString());
    } catch (err: unknown) {
      if (mine !== asked.current) return;
      // VERBATIM. A read the cluster refused says why in its own words, and a
      // friendlier paraphrase would drop the one fact that helps -- the same
      // rule every refusal on this page follows.
      setReading(null);
      setState("failed");
      setFailure(err instanceof Error ? err.message : String(err));
    }
  }, [connection, siteId, window, systemOwned, servedEver]);

  useEffect(() => {
    void read();
    const spec = windowSpec(window);
    const timer = setInterval(() => void read(), spec.refreshMs);
    return () => clearInterval(timer);
    // KEYED ON `read`, which is keyed on the identities that decide WHAT is
    // read -- the connection, the deployable, the window. Never on the
    // reading itself: an effect that re-registered when its own result
    // arrived would poll as fast as the network allows.
  }, [read, window]);

  if (systemOwned) {
    return (
      <section className="os-report-part">
        <h4 className="os-report-heading">Traffic</h4>
        <Caption>{SYSTEM_OWNED_TRAFFIC_NOTE}</Caption>
      </section>
    );
  }
  if (!servedEver) return null;

  return (
    <section className="os-report-part">
      <div className="os-traffic-head">
        <h4 className="os-report-heading">Traffic</h4>
        <div className="os-choice-row os-traffic-windows" role="radiogroup" aria-label="Traffic window">
          {TRAFFIC_WINDOWS.map((w) => (
            <button
              key={w}
              type="button"
              role="radio"
              aria-checked={window === w}
              className="os-choice"
              onClick={() => {
                setWindow(w);
                setState("loading");
              }}
            >
              {windowLabel(w)}
            </button>
          ))}
        </div>
      </div>

      {state === "failed" ? (
        <>
          <Caption>This cluster did not answer for {windowLabel(window).toLowerCase()}.</Caption>
          <p className="os-notice-detail os-mono">{failure}</p>
        </>
      ) : reading === null ? (
        <Caption>{state === "loading" ? "Reading the traffic figures." : unmeasuredSentence(window)}</Caption>
      ) : (
        <>
          <TrafficStrip reading={reading} window={window} />
          <Facts>
            <Fact label="Requests" value={countLabel(reading.requests)} />
            <Fact label="Errors" value={countLabel(reading.errors)} />
            <Fact label="Not found" value={countLabel(reading.notFound)} />
            <Fact
              label="Last served"
              value={reading.lastServedAt === "" ? "" : formatFreshness(reading.lastServedAt, now)}
              title={reading.lastServedAt === "" ? undefined : formatMoment(reading.lastServedAt)}
            />
          </Facts>
          <Caption>
            {`Counted from what the edge served, ${windowLabel(window).toLowerCase()}. Read ${
              readAt === "" ? "just now" : formatFreshness(readAt, now)
            }; it refreshes on its own.`}
          </Caption>
        </>
      )}
    </section>
  );
}

/**
 * The strip: one column per bucket, oldest on the left.
 *
 * COLUMNS RATHER THAN A LINE, because the buckets are discrete bins of small
 * integers. A line between one request and none draws a slope through a value
 * nothing measured; a column at each bucket says exactly what was counted
 * there, and gives every bucket a hover target of its own.
 *
 * A BUCKET WITH NO REQUESTS DRAWS NOTHING, and the gap is the information: a
 * quiet night has to look quiet. The baseline runs under the whole strip so
 * an empty stretch still reads as part of the series rather than as the strip
 * ending.
 */
function TrafficStrip({ reading, window }: { reading: TrafficReading; window: TrafficWindow }) {
  const peak = reading.buckets.reduce((max, b) => Math.max(max, b.requests), 0);
  const unit = windowSpec(window).bucket === "1m" ? "minute" : "hour";
  return (
    <figure className="os-traffic-strip" style={{ ["--os-traffic-h" as string]: `${STRIP_HEIGHT}px` }}>
      {/* THE EXTREME, LABELLED, and nothing else. Without it the strip has no
          scale at all: a reader can see that one hour was busier than another
          and cannot tell whether the tallest column is nine requests or nine
          hundred. Labelling every column instead would be a number beside
          every mark, which goes unread. */}
      <span className="os-traffic-peak">{`peak ${countLabel(peak)} an ${unit}`}</span>
      <ol className="os-traffic-buckets" aria-label={`Requests per ${unit}, oldest first`}>
        {reading.buckets.map((b) => (
          <li key={b.at} className="os-traffic-bucket" title={bucketTitle(b)} data-errors={b.errors > 0 ? "true" : "false"}>
            <span
              className="os-traffic-bar"
              style={{ height: barHeight(b.requests, peak) }}
              aria-hidden
            />
            <span className="os-traffic-tick" aria-hidden />
          </li>
        ))}
      </ol>
      <figcaption className="os-sr-only">{seriesSummary(reading, window)}</figcaption>
    </figure>
  );
}

/**
 * A bucket's own line, for its hover and for a pointer that lands on it.
 *
 * Every number in the strip is reachable here and in the series summary, so
 * the hover enhances the reading rather than gating it.
 */
function bucketTitle(b: TrafficBucket): string {
  const when = b.at === "" ? "" : formatMoment(b.at);
  const parts = [`${countLabel(b.requests)} requests`];
  if (b.errors > 0) parts.push(`${countLabel(b.errors)} errors`);
  if (b.notFound > 0) parts.push(`${countLabel(b.notFound)} not found`);
  return when === "" ? parts.join(", ") : `${when} -- ${parts.join(", ")}`;
}

/**
 * A column's height as a percentage of the strip.
 *
 * A NON-ZERO COUNT NEVER DRAWS NOTHING: one request in a window whose peak is
 * four hundred is two pixels rather than none, because a bucket that served
 * somebody and a bucket that served nobody must not look the same. Zero draws
 * zero, which is the gap the strip's own note is about.
 */
function barHeight(requests: number, peak: number): string {
  if (requests <= 0) return "0";
  if (peak <= 0) return "0";
  const share = requests / peak;
  return `max(2px, ${(share * 100).toFixed(1)}%)`;
}
