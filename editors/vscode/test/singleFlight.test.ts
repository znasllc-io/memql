// singleFlight.test.ts -- the keyed join behind per-cluster sign-in
// (memql#4596).
//
// Nothing used to stop two sign-ins to one cluster running at once: the
// dial-failure toast's "Sign in" action, the palette command, the ownership
// walk and "Sign In with Code" all reached signInToCluster independently, and
// two in-flight flows meant two listeners, two browser tabs or code
// notifications, and two progress notifications. The join makes the second
// request OBSERVE the first instead of duplicating it.

import test from "node:test";
import assert from "node:assert/strict";

import { SingleFlight } from "../src/async/singleFlight.js";

function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void } {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

test("concurrent same-key calls share one execution and one result", async () => {
  const flights = new SingleFlight<string>();
  const gate = deferred<string>();
  let runs = 0;

  const first = flights.run("znas", () => {
    runs += 1;
    return gate.promise;
  });
  const second = flights.run("znas", () => {
    runs += 1;
    return Promise.resolve("never-this");
  });

  gate.resolve("signed-in");
  assert.equal(await first, "signed-in");
  assert.equal(await second, "signed-in", "the joiner observes the first attempt's outcome");
  assert.equal(runs, 1, "the underlying flow must run once");
});

test("a rejection reaches every joiner and releases the slot", async () => {
  const flights = new SingleFlight<string>();
  const gate = deferred<string>();

  const first = flights.run("znas", () => gate.promise);
  const second = flights.run("znas", () => Promise.resolve("never-this"));

  gate.reject(new Error("issuer unreachable"));
  await assert.rejects(first, /issuer unreachable/);
  await assert.rejects(second, /issuer unreachable/, "a joiner shares the failure too");

  // The slot is free again: the NEXT attempt runs fresh rather than
  // replaying the dead promise.
  const third = await flights.run("znas", () => Promise.resolve("retried"));
  assert.equal(third, "retried");
});

test("different keys do not serialize", async () => {
  const flights = new SingleFlight<string>();
  const znas = deferred<string>();

  const blocked = flights.run("znas", () => znas.promise);
  const other = await flights.run("local", () => Promise.resolve("independent"));

  assert.equal(other, "independent", "cluster B must not wait on cluster A's flow");
  znas.resolve("done");
  assert.equal(await blocked, "done");
});

test("the slot is reusable after a resolution", async () => {
  const flights = new SingleFlight<number>();
  let runs = 0;
  const run = (): Promise<number> =>
    flights.run("znas", () => {
      runs += 1;
      return Promise.resolve(runs);
    });

  assert.equal(await run(), 1);
  assert.equal(await run(), 2, "a settled flight must not be replayed to a later caller");
  assert.equal(runs, 2);
});

test("a synchronously-throwing flow still rejects and releases", async () => {
  const flights = new SingleFlight<string>();
  await assert.rejects(
    flights.run("znas", () => {
      throw new Error("threw before returning a promise");
    }),
    /threw before returning a promise/,
  );
  const after = await flights.run("znas", () => Promise.resolve("recovered"));
  assert.equal(after, "recovered");
});
