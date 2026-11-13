import { act, waitFor } from "@testing-library/react";
import { intersect } from "./intersectionObserver";

/** jsdom has no layout. Send the same visibility notification a scrolled
 * list sends in the browser, while retaining the real feed and its requests. */
export async function scrollNextPage(root: ParentNode = document) {
  const find = () => {
    const targets = root.querySelectorAll(".os-next-page-sentinel");
    if (targets.length !== 1) throw new Error(`Expected one next-page sentinel, found ${targets.length}`);
    return targets[0]!;
  };
  const target = root.querySelector(".os-next-page-sentinel") ? find() : await waitFor(find);
  await act(async () => intersect(target));
}
