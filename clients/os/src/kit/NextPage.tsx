import { useContext, useEffect, useRef, useState } from "react";

import { LiveReadVisibility } from "../live/useGraphRevision";
import { Button, Notice } from "./controls";
import { RecordListSkeleton } from "./RecordListSkeleton";
import "./nextPage.css";

/** The paged feed used by every scroll continuation. */
export interface PagedFeed {
  hasMore: boolean;
  loadingMore: boolean;
  loadMoreError: string;
  loadMore: () => void | Promise<void>;
  /** A disconnected or reseeding feed must recover before paging resumes. */
  enabled?: boolean;
  /** The read's identity, so switching filters does not inherit an old end or failure. */
  scope?: unknown;
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
 * Also backs existing `LoadMore` imports. It watches a sentinel standing
 * where the next row will be: in view is the
 * request. While the page is out the list's own skeleton rows hold that place,
 * so nothing below the list jumps. A failure stays in that place with the
 * engine's words and a retry, and does not retry by itself, because scrolling
 * past a refusal is not a request to be refused again. "End of list" is said
 * once, and only after this list grew: a list that fitted on its first page
 * has no end worth announcing.
 */
interface NextPageProps {
  feed: PagedFeed;
  /** The loading status, read by assistive technology only. */
  label?: string;
  /** How many skeleton rows stand in for the page. */
  rows?: number;
}

export function NextPage(props: NextPageProps) {
  const identity = useRef({ scope: props.feed.scope, key: 0 });
  if (!Object.is(identity.current.scope, props.feed.scope)) {
    identity.current = { scope: props.feed.scope, key: identity.current.key + 1 };
  }
  // Filters and sessions get their own continuation state. A late result,
  // old refusal or old end marker cannot cross into the new read.
  return <PageContinuation key={identity.current.key} {...props} />;
}

function PageContinuation({ feed, label = "Loading more", rows = 3 }: NextPageProps) {
  const sentinel = useRef<HTMLDivElement>(null);
  const windowVisible = useContext(LiveReadVisibility);
  const [tabVisible, setTabVisible] = useState(() => document.visibilityState !== "hidden");
  // Native identity and other request/reply feeds have no live connection
  // snapshot. Browser offline pauses their automatic continuation too.
  const [online, setOnline] = useState(() => navigator.onLine !== false);
  useEffect(() => {
    const changed = () => setOnline(navigator.onLine !== false);
    window.addEventListener("online", changed);
    window.addEventListener("offline", changed);
    return () => { window.removeEventListener("online", changed); window.removeEventListener("offline", changed); };
  }, []);
  useEffect(() => {
    const changed = () => setTabVisible(document.visibilityState !== "hidden");
    document.addEventListener("visibilitychange", changed);
    return () => document.removeEventListener("visibilitychange", changed);
  }, []);
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
  // The last load's promise rejected. A feed is meant to say why on
  // `loadMoreError`; one that rejects without saying has still not grown.
  const [rejected, setRejected] = useState(false);
  const [requestError, setRequestError] = useState("");
  const request = useRef<{ scope: unknown } | null>(null);
  const feedRef = useRef(feed);
  feedRef.current = feed;

  const load = useRef((retry = false) => {
    if (request.current || feedRef.current.loadingMore || (!retry && (feedRef.current.enabled === false || navigator.onLine === false))) return;
    const own = { scope: feedRef.current.scope };
    request.current = own;
    mark.current = { progress: feedRef.current.progress };
    setSettled(false);
    setRejected(false);
    setRequestError("");
    setAsking(true);
    setAsked((n) => n + 1);
    const settle = (error?: unknown) => {
      if (!mounted.current || request.current !== own || !Object.is(own.scope, feedRef.current.scope)) return;
      request.current = null;
      setAsking(false);
      setRejected(error !== undefined);
      if (error !== undefined) setRequestError((error instanceof Error ? error.message : String(error)) || "The page could not be read.");
      setSettled(true);
    };
    // A feed reports its own failure on `loadMoreError`; a rejection is
    // settled the same way rather than left unhandled, and never counts as
    // a page that arrived.
    try { void Promise.resolve(feedRef.current.loadMore()).then(() => settle(), error => settle(error ?? "The page could not be read.")); }
    catch (error) { settle(error ?? "The page could not be read."); }
  }).current;

  const busy = asking || feed.loadingMore;
  const error = feed.loadMoreError || requestError;
  const failed = error !== "";
  const armed = feed.hasMore && !busy && !failed && feed.enabled !== false && windowVisible && tabVisible && online;
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
    if (settled && !busy && !failed && !rejected && (moved || feed.progress === undefined)) setGrew(true);
  }, [settled, busy, failed, rejected, moved, feed.progress]);

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
      // A retained pane can become inert between an observer notification
      // and React's visibility update. That is not a request for more rows.
      if (target.parentElement?.closest('[hidden], [inert], [aria-hidden="true"]')) return;
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
      <Notice tone="error" sentence="The next page was not read." next="The rows above are unchanged." detail={error}>
        <Button onClick={() => load(true)}>Try again</Button>
      </Notice>
    );
  } else if (feed.hasMore) {
    body = <div ref={sentinel} className="os-next-page-sentinel" aria-hidden="true" />;
  } else if (grew) {
    body = <p className="os-next-page-end os-caption">End of list</p>;
  }
  return body === null ? null : <div className="os-next-page">{body}</div>;
}
