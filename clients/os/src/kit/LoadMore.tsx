import { Button } from "./controls";

/** Shared continuation control: errors leave loaded rows and the cursor intact. */
export function LoadMore({ feed }: { feed: {
  hasMore: boolean; loadingMore: boolean; loadMoreError: string; loadMore: () => Promise<void>;
} }) {
  if (!feed.hasMore && !feed.loadMoreError) return null;
  return <div className="os-list-continuation">
    {feed.loadMoreError ? <p className="os-ask-error" role="alert">{feed.loadMoreError}</p> : null}
    <Button disabled={feed.loadingMore} onClick={() => void feed.loadMore()}>
      {feed.loadingMore ? "Loading…" : feed.loadMoreError ? "Try again" : "Load more"}
    </Button>
  </div>;
}
