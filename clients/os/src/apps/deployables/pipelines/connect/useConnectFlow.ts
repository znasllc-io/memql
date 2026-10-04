import { useCallback, useEffect, useRef, useState } from "react";

import { useOsConnection } from "../../../../live/connection";
import { connectPipeline, previewPipeline, problemFrom } from "../calls";
import type { Compute, Delivery, PipelineRow } from "../rows";
import { NO_PREVIEW, type Answers, type ConnectMode, type ConnectOutcome, type PreviewRead } from "./flow";

// THE CONNECT FLOW'S STATE, HELD ABOVE THE SECTION (design record D14; issue
// memql#5502: "State held above the section the way useAddMachineFlow holds the
// install draft"). DeployablesApp holds it, so leaving the rail and coming back
// -- to another tab, to the source page -- keeps every answer, and nothing is
// written until Connect pipeline is pressed.
//
// ===========================================================================
// IT SURVIVES THE WINDOW'S OWN NAVIGATION, AND NOTHING ELSE
// ===========================================================================
// React state only, like the install draft. Nothing here is a secret -- the
// preview names secrets and never holds a value -- but a draft kept in browser
// storage would outlive the source it is about, and come back over a pipeline
// somebody else has since connected.
//
// ===========================================================================
// THE READ AND THE WRITE BELONG TO THE FLOW, NOT THE PAGE
// ===========================================================================
// A person who leaves while memql-package.yaml is being read, or while the
// connect is in flight, comes back to its answer rather than to a page that
// asks again: the read and the write are made here, at the app root, and land
// in the flow whether or not a page is showing it.
//
// EVERY ANSWER IS STAMPED WITH THE FLOW IT BELONGS TO (`flowId`). A read or a
// connect that comes back after Cancel, or after a flow was started over
// another source, finds a different flow and is dropped, never folded into one
// it was not asked for.

export interface ConnectFlow {
  /** The source the open flow is over, or "" when none is open. */
  packageId: string;
  /** Open the flow over a source, prefilled from its pipeline when it has one; resumes a flow already open over the same source. */
  start: (packageId: string, existing: PipelineRow | null) => void;
  /** Close the flow and drop its answers. */
  reset: () => void;
  /** Connect, or change an active pipeline: fixed when the flow starts. */
  mode: ConnectMode;
  /** The source's pipeline when the flow started: a reconnect is prefilled from it. */
  existing: PipelineRow | null;
  /** What connecting would act on: pipelinesPreview, read when the flow starts. */
  read: PreviewRead;
  /** Read memql-package.yaml again -- after a refusal, or after the manifest changed. */
  readAgain: () => void;
  answers: Answers;
  chooseCompute: (compute: Compute) => void;
  chooseDelivery: (delivery: Delivery) => void;
  /** The person moved past the stages (Continue). */
  reviewed: boolean;
  review: () => void;
  outcome: ConnectOutcome;
  /** The one write. The answers come from the page's reading of this flow. */
  connect: (input: { compute: Compute; delivery: Delivery; secretNames: readonly string[] }) => void;
  /** The stop a person opened, pinned to where the flow was when they did; null for the flow's own. */
  chosen: { step: string; at: string } | null;
  openStep: (step: string, at: string) => void;
}

interface Held {
  /** Which flow this is: bumped by every fresh start and every reset. */
  flowId: number;
  packageId: string;
  existing: PipelineRow | null;
  mode: ConnectMode;
  /** Bumped by Read again: a read answers one request, and only that one. */
  readRequest: number;
  read: PreviewRead;
  answers: Answers;
  reviewed: boolean;
  outcome: ConnectOutcome;
  chosen: { step: string; at: string } | null;
}

const NOTHING: Omit<Held, "flowId"> = {
  packageId: "",
  existing: null,
  mode: "connect",
  readRequest: 0,
  read: { state: "idle" },
  answers: { compute: null, delivery: null },
  reviewed: false,
  outcome: { state: "idle" },
  chosen: null,
};

export function useConnectFlow(): ConnectFlow {
  const connection = useOsConnection();
  const [flow, setFlow] = useState<Held>({ flowId: 0, ...NOTHING });

  // THE READ, made whenever an open flow has a request nobody has answered.
  //
  // An effect rather than a call inside `start`, for the one case a call
  // cannot cover: a flow started while the connection is down reads the
  // moment it comes back. Keyed on the request and never on the read's own
  // state, because the read writes that state -- an effect keyed on it would
  // abort the read it had just begun.
  const answered = useRef("");
  const { flowId, packageId, readRequest } = flow;
  useEffect(() => {
    if (packageId === "" || connection === null) return;
    const key = `${flowId}:${readRequest}`;
    if (answered.current === key) return;
    const abort = new AbortController();
    const mine = (held: Held) => held.flowId === flowId && held.readRequest === readRequest;
    setFlow((held) => (mine(held) ? { ...held, read: { state: "reading", startedAt: Date.now() } } : held));
    previewPipeline(connection.query, packageId, abort.signal).then(
      (preview) => {
        if (abort.signal.aborted) return;
        answered.current = key;
        setFlow((held) => (mine(held) ? { ...held, read: preview === null ? { state: "failed", problem: NO_PREVIEW } : { state: "read", preview } } : held));
      },
      (err: unknown) => {
        if (abort.signal.aborted) return;
        answered.current = key;
        setFlow((held) => (mine(held) ? { ...held, read: { state: "failed", problem: problemFrom(err) } } : held));
      },
    );
    return () => abort.abort();
  }, [connection, flowId, packageId, readRequest]);

  const start = useCallback((id: string, existing: PipelineRow | null) => {
    setFlow((held) => {
      // RESUMED, NOT RESTARTED: the same source's unfinished flow is where
      // the person left it. A FINISHED one is not resumed -- Change on a
      // pipeline just connected is a new question, not the old "Connected".
      if (id !== "" && held.packageId === id && held.outcome.state !== "connected") return held;
      return {
        ...NOTHING,
        flowId: held.flowId + 1,
        packageId: id,
        existing,
        mode: existing !== null && existing.status === "active" ? "change" : "connect",
      };
    });
  }, []);

  const reset = useCallback(() => {
    setFlow((held) => ({ ...NOTHING, flowId: held.flowId + 1 }));
  }, []);

  const readAgain = useCallback(() => {
    setFlow((held) => {
      if (held.packageId === "" || held.outcome.state === "connecting" || held.outcome.state === "connected") return held;
      // A fresh read is a fresh set of stages: the person reviews them again,
      // and a refusal of the last connect is answered by this read, not kept.
      return { ...held, readRequest: held.readRequest + 1, read: { state: "idle" }, reviewed: false, outcome: { state: "idle" }, chosen: null };
    });
  }, []);

  const chooseCompute = useCallback((compute: Compute) => {
    setFlow((held) => {
      if (held.outcome.state === "connecting" || held.outcome.state === "connected") return held;
      // CHOOSING ANSWERS THE STEP, so the stop the person opened closes and
      // the flow's own opens: the one the answer leads to. A refusal the last
      // connect gave is answered by choosing again.
      return {
        ...held,
        answers: { ...held.answers, compute },
        reviewed: true,
        outcome: held.outcome.state === "refused" ? { state: "idle" } : held.outcome,
        chosen: null,
      };
    });
  }, []);

  const chooseDelivery = useCallback((delivery: Delivery) => {
    setFlow((held) => (held.outcome.state === "connecting" || held.outcome.state === "connected" ? held : { ...held, answers: { ...held.answers, delivery } }));
  }, []);

  const review = useCallback(() => {
    setFlow((held) => ({ ...held, reviewed: true, chosen: null }));
  }, []);

  const openStep = useCallback((step: string, at: string) => {
    setFlow((held) => ({ ...held, chosen: { step, at } }));
  }, []);

  // THE ONE WRITE. A second press before the first has answered is not a
  // second connect: the button is busy from the next render, and this guard
  // covers the clicks that land before it.
  const inFlight = useRef(false);
  const connect = useCallback(
    (input: { compute: Compute; delivery: Delivery; secretNames: readonly string[] }) => {
      if (connection === null || packageId === "" || inFlight.current) return;
      inFlight.current = true;
      const mine = (held: Held) => held.flowId === flowId;
      setFlow((held) => (mine(held) ? { ...held, outcome: { state: "connecting", startedAt: Date.now() } } : held));
      connectPipeline(connection.query, { packageId, ...input })
        .then(
          (done) => setFlow((held) => (mine(held) ? { ...held, outcome: { state: "connected", pipelineId: done.pipelineId, reconnected: done.reconnected } } : held)),
          (err: unknown) => setFlow((held) => (mine(held) ? { ...held, outcome: { state: "refused", problem: problemFrom(err) } } : held)),
        )
        .finally(() => {
          inFlight.current = false;
        });
    },
    [connection, flowId, packageId],
  );

  return {
    packageId: flow.packageId,
    start,
    reset,
    mode: flow.mode,
    existing: flow.existing,
    read: flow.read,
    readAgain,
    answers: flow.answers,
    chooseCompute,
    chooseDelivery,
    reviewed: flow.reviewed,
    review,
    outcome: flow.outcome,
    connect,
    chosen: flow.chosen,
    openStep,
  };
}
