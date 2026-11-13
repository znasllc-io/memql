import { NextPage, type PagedFeed } from "./NextPage";

/** Existing list imports share the same scroll continuation as newer lists. */
export function LoadMore({ feed }: { feed: PagedFeed }) {
  return <NextPage feed={feed} />;
}
