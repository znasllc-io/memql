import { useEffect, useRef, useState } from "react";

import { Button, Notice } from "./controls";
import { RecordListSkeleton } from "./RecordListSkeleton";
import "./nextPage.css";

/** The paged feed both continuation controls take, so a list moves between them in one line. */
export interface PagedFeed {
  hasMore: boolean;
  loadingMore: boolean;
  loadMoreError: string;
  loadMore: () => Promise<void>;
  /**
   * A value that changes when a page lands: the row count, or the cursor.
   * Optional, so a feed `LoadMore` takes still fits. With it, a sentinel
   * still in view after a page arrived asks again at once; without it, every
   * settled load waits for the sentinel to leave the view and come back.
   */
  progress?: unknown;
}

/**
 * A record list's next page, loaded when the end of the list comes into view
 * (DESIGN.md, Record lists).
 *
 * Takes exactly the feed `LoadMore` takes. Where `LoadMore` waits for a press,
 * this watches a sentinel standing where the next row will be: in view is the
 * request. While the page is out the list's own skeleton rows hold that place,
 * so nothing below the list jumps. A failure stays in that place with the
 * engine's words and a retry, and does not retry by itself, because scrolling
 * past a refusal is not a request to be refused again. "End of list" is said
 * once, and only after this list grew: a list that fitted on its first page
 * has no end worth announcing.
 */
export function NextPage({ feed, label = "Loading more", rows = 3 }: {
  feed: PagedFeed;
  /** The loading status, read by assistive technology only. */
  label?: string;
  /** How many skeleton rows stand in for the page. */
  rows?: number;
}) {
  const sentinel = useRef<HTMLDivElement>(null);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);
  // OUR OWN "ASKED" FLAG beside the feed's `loadingMore`. A feed may settle a
  // load without ever reporting it as loading (a refusal caught at once, a
  // cursor that was already spent), and the observer is disarmed for the
  // length of a request; without this the sentinel would stay unwatched.
  const [asking, setAsking] = useState(false);
  const [grew, setGrew] = useState(false);
  // Counts loads asked for. The observer is rebuilt on every one: a load that
  // settles inside the same render batch it started in never shows `asking`,
  // so `armed` alone would not change and the sentinel would go unwatched.
  const [asked, setAsked] = useState(0);
  // What `progress` read when the last load was asked for, and whether that
  // load has settled. Null before the first load.
  const mark = useRef<{ progress: unknown } | null>(null);
  const [settled, setSettled] = useState(false);
  const feedRef = useRef(feed);
  feedRef.current = feed;

  const load = useRef(() => {
    mark.current = { progress: feedRef.current.progress };
    setSettled(false);
    setAsking(true);
    setAsked((n) => n + 1);
    const settle = () => {
      if (!mounted.current) return;
      setAsking(false);
      setSettled(true);
    };
    // A feed reports its own failure on `loadMoreError`; a rejection is
    // settled the same way rather than left unhandled.
    void feedRef.current.loadMore().then(settle, settle);
  }).current;

  const busy = asking || feed.loadingMore;
  const failed = feed.loadMoreError !== "";
  const armed = feed.hasMore && !busy && !failed;
  const moved = mark.current !== null && feed.progress !== undefined && !Object.is(feed.progress, mark.current.progress);
  // STALLED: the last load settled, without a failure, and the feed shows no
  // sign that anything arrived. A browser's observer reports a target's state
  // the moment it starts watching, so a sentinel still in view would ask again
  // at once, and again, in a tight loop. A stalled list asks only once the
  // sentinel has left the view and come back, or once the feed moves on.
  const stalled = settled && !busy && !failed && !moved;

  // THE END IS EARNED BY A PAGE. A settled load counts as growth only when it
  // did not fail and, where the feed says, something actually arrived. A feed
  // that stops offering more after a failure has not grown.
  useEffect(() => {
    if (settled && !busy && !failed && (moved || feed.progress === undefined)) setGrew(true);
  }, [settled, busy, failed, moved, feed.progress]);

  // A NEW OBSERVER EVERY TIME IT RE-ARMS. An observer reports a target's
  // state when it starts watching, so a sentinel still in view after a short
  // page asks again at once, rather than waiting for a scroll that may never
  // come on a tall window -- unless the list is stalled, above.
  useEffect(() => {
    if (!armed) return;
    const target = sentinel.current;
    if (target === null || typeof IntersectionObserver === "undefined") return;
    let left = !stalled;
    const observer = new IntersectionObserver((entries) => {
      if (!entries.some((entry) => entry.isIntersecting)) {
        left = true;
        return;
      }
      if (!left) return;
      observer.disconnect();
      load();
    }, { rootMargin: "0px 0px 240px 0px" });
    observer.observe(target);
    return () => observer.disconnect();
  }, [armed, stalled, asked, load]);

  let body = null;
  if (busy) {
    body = <RecordListSkeleton label={label} rows={rows} />;
  } else if (failed) {
    body = (
      <Notice tone="error" sentence="The next page was not read." next="The rows above are unchanged." detail={feed.loadMoreError}>
        <Button onClick={load}>Try again</Button>
      </Notice>
    );
  } else if (feed.hasMore) {
    body = <div ref={sentinel} className="os-next-page-sentinel" aria-hidden="true" />;
  } else if (grew) {
    body = <p className="os-next-page-end os-caption">End of list</p>;
  }
  return body === null ? null : <div className="os-next-page">{body}</div>;
}
