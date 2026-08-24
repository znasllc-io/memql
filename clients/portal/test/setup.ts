import { beforeEach } from "vitest";

import { resetActivity } from "../src/cluster/activity";

// vitest setup: run once per test file, before any test.
//
// React 19 + @testing-library/react need IS_REACT_ACT_ENVIRONMENT so state
// updates inside render/fireEvent are wrapped in act() rather than warning on
// every assertion.
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

// MODULE-LEVEL STATE MUST BE RESET BETWEEN TESTS, and the activity clock is the
// one piece of it this suite has.
//
// src/cluster/activity.ts keeps `lastActivityAt` at module scope -- correct in
// a browser, where one document lives for the life of the page. Under vitest a
// module instance is shared across the tests in a file, so a row-walk or feed
// settle in one test leaves the clock warm for the next.
//
// What that produces is an ORDER-DEPENDENT failure rather than a loud one: the
// rail mark renders "streaming" instead of "connected", and the test cannot
// wait it out, because the activity window is 2500ms and waitFor's default
// timeout is 1000ms. It passes in isolation, passes in most orderings, and
// fails in CI depending on which files a worker picked up first -- which is
// exactly the shape that gets re-run rather than fixed.
//
// Reproduce it with `npx vitest run --sequence.shuffle`, which fails without
// this reset and passes with it.
beforeEach(() => {
  resetActivity();
});
