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
  const feedRef = useRef(feed);
  feedRef.current = feed;

  const load = useRef(() => {
    setAsking(true);
    const settle = () => {
      if (!mounted.current) return;
      setAsking(false);
      setGrew(true);
    };
    // A feed reports its own failure on `loadMoreError`; a rejection is
    // settled the same way rather than left unhandled.
    void feedRef.current.loadMore().then(settle, settle);
  }).current;

  const busy = asking || feed.loadingMore;
  const armed = feed.hasMore && !busy && feed.loadMoreError === "";

  // A NEW OBSERVER EVERY TIME IT RE-ARMS. An observer reports a target's
  // state when it starts watching, so a sentinel still in view after a short
  // page asks again at once, rather than waiting for a scroll that may never
  // come on a tall window.
  useEffect(() => {
    if (!armed) return;
    const target = sentinel.current;
    if (target === null || typeof IntersectionObserver === "undefined") return;
    const observer = new IntersectionObserver((entries) => {
      if (!entries.some((entry) => entry.isIntersecting)) return;
      observer.disconnect();
      load();
    }, { rootMargin: "0px 0px 240px 0px" });
    observer.observe(target);
    return () => observer.disconnect();
  }, [armed, load]);

  let body = null;
  if (busy) {
    body = <RecordListSkeleton label={label} rows={rows} />;
  } else if (feed.loadMoreError !== "") {
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
