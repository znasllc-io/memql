// Local models on the fleet (epic memql#4676, tasks memql#4683 and #4684).
//
// What these hold, in order of how badly each would bite:
//
//   * ELIGIBILITY IS NOT RECOMPUTED IN THE CLIENT. The gate branches on the
//     server's answer. A client that re-derived it would eventually disagree
//     with the router, and the disagreement is silent: the console looks fine
//     and every feature in it parks.
//   * A LATER OFFLINE MACHINE IS A NOTICE, NEVER AN EVICTION.
//   * SKIPPABLE ONLY WHEN AUTH IS DISABLED.
//   * The park card shows the machines that were ruled out, which is the
//     question a person actually has.
//   * A chain with no authored fallback renders `park` as its terminal step --
//     without it, every reader assumes "falls back to the cloud", which is
//     precisely what does not happen.

import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";

import { InferenceGate, InferenceNotice } from "../src/fleet/InferenceGate";
import { NoLocalModelCard, parsePark } from "../src/fleet/NoLocalModelCard";
import { LocalModelsPanel } from "../src/fleet/LocalModelsPanel";
import { modelsFromLabels, runtimesFromLabels } from "../src/fleet/labels";
import {
  chainInWords,
  formatContextWindow,
  summarizeFleet,
  toFleetModels,
  type FleetModelRow,
} from "../src/fleet/useFleetModels";
import {
  canSkipInference,
  gateStep,
  toInferenceStatus,
  type InferenceStatus,
} from "../src/fleet/useInferenceStatus";

const ONLINE_MODEL = {
  modelId: "llama3.1:8b",
  contextWindow: 131072,
  structuredOutput: true,
  embeddings: false,
  online: true,
  machineCount: 1,
  onlineCount: 1,
  machines: [
    {
      registrationId: "w-1",
      name: "laptop",
      displayName: "Jose's laptop",
      runtimes: ["ollama"],
      online: true,
      busy: false,
      activeCount: 0,
      maxConcurrent: 2,
    },
  ],
};

const ELIGIBLE: InferenceStatus = {
  eligible: true,
  doorsOpen: ["local"],
  localEligible: true,
  localModelCount: 1,
  eligibleModelIds: ["llama3.1:8b"],
  cloudConfigured: false,
  federationConfigured: false,
  fleetInferenceInstalled: true,
  minimumContextWindow: 8192,
};

const INELIGIBLE: InferenceStatus = {
  ...ELIGIBLE,
  eligible: false,
  doorsOpen: [],
  localEligible: false,
  eligibleModelIds: [],
};

describe("the model catalog decoder", () => {
  it("derives one machine label the way the machines page does", () => {
    const models = toFleetModels([ONLINE_MODEL]);
    expect(models[0]?.machines[0]?.label).toBe("Jose's laptop");

    const unnamed = toFleetModels([
      { ...ONLINE_MODEL, machines: [{ ...ONLINE_MODEL.machines[0], displayName: "" }] },
    ]);
    // Falls back to the reported name, so a model's machine list and the
    // machines page cannot disagree about what a laptop is called.
    expect(unnamed[0]?.machines[0]?.label).toBe("laptop");
  });

  it("reads a context window the machine did not report as 'not reported', never as zero", () => {
    expect(formatContextWindow(0)).toContain("not reported");
    expect(formatContextWindow(131072)).toBe("131k context");
  });
});

describe("the fleet headline", () => {
  const online: FleetModelRow[] = toFleetModels([ONLINE_MODEL]);
  const asleep: FleetModelRow[] = toFleetModels([
    { ...ONLINE_MODEL, online: false, onlineCount: 0 },
  ]);

  it("says 'fully local' only when no cloud provider is configured", () => {
    expect(summarizeFleet(online, false).tone).toBe("local");
    expect(summarizeFleet(online, false).headline).toContain("fully local");

    // With a key configured, claiming it would be a statement about spend
    // that is not true.
    expect(summarizeFleet(online, true).tone).toBe("mixed");
    expect(summarizeFleet(online, true).headline).not.toContain("fully local");
  });

  it("distinguishes an asleep fleet from an absent one", () => {
    expect(summarizeFleet(asleep, false).tone).toBe("asleep");
    expect(summarizeFleet([], false).tone).toBe("none");
  });
});

describe("the policy chain, in words", () => {
  it("renders park as the terminal step when no fallback is authored", () => {
    // Without this, every reader assumes "falls back to the cloud", which is
    // exactly what does NOT happen -- and is the reason a plan they are
    // waiting on is parked.
    expect(chainInWords(["fleet:llama3.1:8b"])).toBe("local llama3.1:8b → park");
  });

  it("does not add park when a cloud fallback IS authored", () => {
    expect(chainInWords(["fleet:llama3.1:8b", "streamClaudeSonnet"])).toBe(
      "local llama3.1:8b → streamClaudeSonnet",
    );
  });

  it("says so when nothing is configured", () => {
    expect(chainInWords([])).toBe("not configured");
  });
});

describe("the model chips on a machine card", () => {
  it("reads the machine's OWN reported labels, not an operator's", () => {
    // An operator-set model label would be a claim the machine never made,
    // and the router -- matching on what the machine reports -- would refuse
    // the call it implied.
    const reported = { "model:llama3.1:8b": "ctx=131072,structured=1", "runtime:ollama": "1" };
    expect(modelsFromLabels(reported)).toEqual(["llama3.1:8b"]);
    expect(runtimesFromLabels(reported)).toEqual(["ollama"]);
  });

  it("keeps the colon in a model tag", () => {
    // A naive split eats the tag and leaves "llama3.1", which is not a model
    // anything hosts.
    expect(modelsFromLabels({ "model:qwen2.5:7b-instruct": "1" })).toEqual(["qwen2.5:7b-instruct"]);
  });
});

describe("the first-run gate", () => {
  it("asks for a passkey before inference", () => {
    expect(
      gateStep({ authEnabled: true, hasPasskey: false, status: ELIGIBLE, alreadyEntered: false, skipped: false }),
    ).toBe("passkey");
  });

  it("asks for inference once a passkey is enrolled", () => {
    expect(
      gateStep({ authEnabled: true, hasPasskey: true, status: INELIGIBLE, alreadyEntered: false, skipped: false }),
    ).toBe("inference");
  });

  it("lets an eligible cluster straight through", () => {
    expect(
      gateStep({ authEnabled: true, hasPasskey: true, status: ELIGIBLE, alreadyEntered: false, skipped: false }),
    ).toBe("console");
  });

  it("lets a person through while the status has not answered yet", () => {
    // Deliberately the permissive direction. Holding would show the gate to
    // EVERY user on EVERY page load ahead of the page they asked for -- a
    // permanent tax on the common case (an already-configured cluster) --
    // where letting them through costs a first-run user one flash of the
    // console, once.
    expect(
      gateStep({ authEnabled: true, hasPasskey: true, status: null, alreadyEntered: false, skipped: false }),
    ).toBe("console");
  });

  it("gates only on a DEFINITE negative answer", () => {
    expect(
      gateStep({ authEnabled: true, hasPasskey: true, status: INELIGIBLE, alreadyEntered: false, skipped: false }),
    ).toBe("inference");
  });

  it("does not gate on an answer it could not READ, which is different from a negative one", () => {
    // An engine older than this construct, or a transient failure, must not
    // lock somebody out of their own console.
    expect(
      gateStep({
        authEnabled: true,
        hasPasskey: true,
        status: null,
        alreadyEntered: false,
        skipped: false,
        unreadable: true,
      }),
    ).toBe("console");
  });

  it("skips the passkey step entirely on an auth-disabled cluster", () => {
    expect(
      gateStep({ authEnabled: false, hasPasskey: false, status: INELIGIBLE, alreadyEntered: false, skipped: false }),
    ).toBe("inference");
  });

  it("is skippable ONLY when auth is disabled", () => {
    expect(canSkipInference(false)).toBe(true);
    expect(canSkipInference(true)).toBe(false);

    expect(
      gateStep({ authEnabled: false, hasPasskey: false, status: INELIGIBLE, alreadyEntered: false, skipped: true }),
    ).toBe("console");
    // The same skip on an authenticated cluster changes nothing.
    expect(
      gateStep({ authEnabled: true, hasPasskey: true, status: INELIGIBLE, alreadyEntered: false, skipped: true }),
    ).toBe("inference");
  });

  it("does NOT evict somebody whose machine went offline mid-session", () => {
    // Ejecting them would punish them for a condition that fixes itself when
    // they open the laptop -- and would take away the page they would use to
    // find that out.
    expect(
      gateStep({ authEnabled: true, hasPasskey: true, status: INELIGIBLE, alreadyEntered: true, skipped: false }),
    ).toBe("console");
  });

  it("renders the three doors with local first", () => {
    render(
      <InferenceGate status={INELIGIBLE} loading={false} error="" canSkip={false} onPairMachine={vi.fn()} />,
    );
    const headings = screen.getAllByRole("heading", { level: 3 }).map((h) => h.textContent ?? "");
    expect(headings[0]).toContain("your own machine");
    expect(headings.join(" ")).toContain("federation");
    expect(headings.join(" ")).toContain("API key");
  });

  it("offers no skip on an authenticated cluster", () => {
    const onSkip = vi.fn();
    render(<InferenceGate status={INELIGIBLE} loading={false} error="" canSkip={false} onSkip={onSkip} />);
    expect(screen.queryByRole("button", { name: /continue without inference/i })).toBeNull();
  });

  it("offers the skip on an auth-disabled cluster", () => {
    render(<InferenceGate status={INELIGIBLE} loading={false} error="" canSkip onSkip={vi.fn()} />);
    expect(screen.getByRole("button", { name: /continue without inference/i })).toBeTruthy();
  });

  it("separates 'this node has no worker service' from 'your machines are asleep'", () => {
    render(
      <InferenceGate
        status={{ ...INELIGIBLE, fleetInferenceInstalled: false }}
        loading={false}
        error=""
        canSkip={false}
      />,
    );
    expect(screen.getByText(/no worker service/i)).toBeTruthy();
  });

  it("renders a notice rather than nothing when a session becomes ineligible", () => {
    const { container } = render(<InferenceNotice status={INELIGIBLE} />);
    expect(container.textContent).toContain("paused");
    // An eligible cluster gets no notice at all.
    const { container: quiet } = render(<InferenceNotice status={ELIGIBLE} />);
    expect(quiet.textContent).toBe("");
  });
});

describe("the no_local_model_available park card", () => {
  const PARK = {
    code: "no_local_model_available",
    model: "llama3.1:8b",
    machinesTotal: 2,
    machinesRuledOut: [
      { machine: "laptop", reason: "offline" },
      { machine: "desktop", reason: "does not offer model llama3.1:8b" },
    ],
    cloudProviderConfigured: true,
  };

  it("refuses to build a card from a payload it cannot read", () => {
    // A card missing the very list it exists to show leaves the reader worse
    // off than the plain status line.
    expect(parsePark(null)).toBeNull();
    expect(parsePark({ code: "budget_approval_required" })).toBeNull();
    expect(parsePark(PARK)).not.toBeNull();
  });

  it("names every machine considered and why", () => {
    render(<NoLocalModelCard park={parsePark(PARK)!} onWakeMachine={vi.fn()} onApproveCloud={vi.fn()} />);
    expect(screen.getByText("laptop")).toBeTruthy();
    expect(screen.getByText("offline")).toBeTruthy();
    expect(screen.getByText("desktop")).toBeTruthy();
    expect(screen.getByText(/does not offer model/)).toBeTruthy();
  });

  it("says so when the user has no machines at all", () => {
    const card = parsePark({ ...PARK, machinesTotal: 0, machinesRuledOut: [] })!;
    render(<NoLocalModelCard park={card} />);
    expect(screen.getByText(/no machine is paired/i)).toBeTruthy();
  });

  it("hides approve-cloud entirely when no cloud provider is configured", () => {
    // A disabled button turns "your machines are asleep" into "you clicked the
    // fix and it did not fix it", which is much harder to act on.
    const card = parsePark({ ...PARK, cloudProviderConfigured: false })!;
    render(<NoLocalModelCard park={card} onWakeMachine={vi.fn()} onApproveCloud={vi.fn()} />);
    expect(screen.getByRole("button", { name: /wake a machine/i })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /cloud/i })).toBeNull();
  });
});

describe("the local models panel", () => {
  it("lists a model whose only machine is asleep, marked, rather than dropping it", () => {
    const asleep = toFleetModels([
      {
        ...ONLINE_MODEL,
        online: false,
        onlineCount: 0,
        machines: [{ ...ONLINE_MODEL.machines[0], online: false }],
      },
    ]);
    render(
      <LocalModelsPanel models={asleep} cloudConfigured={false} loading={false} error="" onReload={vi.fn()} />,
    );
    // The operator's question is why it is not being used; a list that
    // dropped it answers with silence.
    expect(screen.getByText("llama3.1:8b")).toBeTruthy();
    expect(screen.getByText(/0 of 1 machine online/)).toBeTruthy();
  });

  it("states plainly when the cluster is fully local", () => {
    render(
      <LocalModelsPanel
        models={toFleetModels([ONLINE_MODEL])}
        cloudConfigured={false}
        loading={false}
        error=""
        onReload={vi.fn()}
      />,
    );
    // Twice: the headline says it and the callout says it. getAllByText
    // rather than getByText, because "the page says this in two places" is
    // the intended design, not an ambiguity to disambiguate away.
    expect(screen.getAllByText(/running fully local/i).length).toBeGreaterThan(0);
  });

  it("does not claim fully local while a cloud provider is configured", () => {
    render(
      <LocalModelsPanel
        models={toFleetModels([ONLINE_MODEL])}
        cloudConfigured
        loading={false}
        error=""
        onReload={vi.fn()}
      />,
    );
    expect(screen.queryAllByText(/running fully local/i)).toHaveLength(0);
  });
});

describe("the inference status decoder", () => {
  it("keeps only doors it recognises", () => {
    const status = toInferenceStatus([
      { eligible: true, doorsOpen: ["local", "telepathy"], minimumContextWindow: 8192 },
    ]);
    expect(status?.doorsOpen).toEqual(["local"]);
  });

  it("returns null when the server sent no row", () => {
    expect(toInferenceStatus([])).toBeNull();
  });
});
