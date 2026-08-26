// Synapse: the AI affordance (memql#4658, decisions D6-D8).
//
// Three things are worth pinning and one of them is a HARD RULE.
//
//   * patch coercion, because it is the last of three defences against a
//     value nobody chose landing in a form somebody is about to submit;
//   * the never-submit rule, which is the whole reason this is an affordance
//     and not an agent;
//   * the token float's arithmetic, because a spend indicator that is wrong
//     is worse than none.
//
// What is NOT asserted is what the assemble or the float LOOK like. jsdom
// applies no stylesheet; that is the visual QA pass's job.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { useState, type ReactNode } from "react";
import type { Connection } from "@znasllc-io/memql-sdk-core/client";

import { AuthProvider } from "../src/auth/AuthProvider";
import { ClusterProvider } from "../src/cluster/ClusterProvider";
import { coercePatches } from "../src/synapse/patches";
import { SynapseButton, SynapseProvider, SynapseSection } from "../src/synapse";
import { describeFloat, floatFor, readAverage, recordUsage } from "../src/synapse/tokens";
import { asQueryClient } from "./support/queryFake";
import type { SynapseField, SynapsePatch } from "../src/synapse/types";

const FIELDS: readonly SynapseField[] = [
  { name: "name", type: "text", label: "Name" },
  { name: "count", type: "number", label: "How many" },
  { name: "computerUse", type: "boolean", label: "Computer use" },
  { name: "platform", type: "enum", label: "Platform", options: ["mac", "linux"] },
];

describe("patch coercion", () => {
  it("drops a field the scope never offered", () => {
    // The failure this exists for: a value written into something the caller
    // did not put on screen. The engine drops it too, and the template
    // forbids it -- three checks, because only the caller knows what its own
    // form does.
    const patches = coercePatches(
      [
        { field: "name", value: "studio mac" },
        { field: "workerToken", value: "mql_wkr_pretend" },
      ],
      FIELDS,
    );
    expect(patches).toEqual([{ field: "name", value: "studio mac" }]);
  });

  it("coerces to the field's declared type", () => {
    expect(coercePatches([{ field: "count", value: " 12 " }], FIELDS)).toEqual([
      { field: "count", value: 12 },
    ]);
    expect(coercePatches([{ field: "computerUse", value: "yes" }], FIELDS)).toEqual([
      { field: "computerUse", value: true },
    ]);
  });

  it("drops rather than guesses at what will not coerce", () => {
    // "about five" is not 5, and a form silently holding a number nobody
    // chose is worse than a field left empty.
    expect(coercePatches([{ field: "count", value: "about five" }], FIELDS)).toEqual([]);
    expect(coercePatches([{ field: "computerUse", value: "if you like" }], FIELDS)).toEqual([]);
    expect(coercePatches([{ field: "platform", value: "windows" }], FIELDS)).toEqual([]);
  });

  it("matches an enum loosely and writes the option's own spelling", () => {
    // A person says "MAC"; the form has to store "mac".
    expect(coercePatches([{ field: "platform", value: "MAC" }], FIELDS)).toEqual([
      { field: "platform", value: "mac" },
    ]);
  });

  it("takes the first of two patches for one field", () => {
    // A reply naming a field twice disagrees with itself; taking the LAST
    // would make the value depend on ordering nobody specified.
    expect(
      coercePatches(
        [
          { field: "name", value: "first" },
          { field: "name", value: "second" },
        ],
        FIELDS,
      ),
    ).toEqual([{ field: "name", value: "first" }]);
  });

  it("survives a reply that is not the shape it claims", () => {
    for (const junk of [null, undefined, "patches", 7, [null], ["nope"], [{}]]) {
      expect(coercePatches(junk, FIELDS)).toEqual([]);
    }
  });
});

describe("the token float", () => {
  beforeEach(() => globalThis.localStorage?.clear());

  it("says first run, and estimates, when a scope has no history", () => {
    const float = floatFor("scope-a", 400);
    expect(float.firstRun).toBe(true);
    expect(float.estimated).toBe(true);
    expect(float.tokens).toBe(100);
    expect(describeFloat(float)).toMatch(/First run/);
  });

  it("uses the measured average once a reply has reported one", () => {
    recordUsage("scope-a", 300);
    const float = floatFor("scope-a", 999999);
    expect(float.firstRun).toBe(false);
    expect(float.estimated).toBe(false);
    expect(float.tokens).toBe(300);
    // The character estimate is IGNORED once there is a real number -- which
    // is the point of keeping one.
    expect(describeFloat(float)).toBe("300 tokens");
  });

  it("moves toward recent usage rather than a lifetime mean", () => {
    recordUsage("scope-a", 100);
    recordUsage("scope-a", 300);
    const avg = readAverage("scope-a");
    expect(avg?.avg).toBe(200);
    expect(avg?.n).toBe(2);
  });

  it("ignores a nonsense reading rather than poisoning the average", () => {
    recordUsage("scope-a", 100);
    recordUsage("scope-a", 0);
    recordUsage("scope-a", Number.NaN);
    recordUsage("scope-a", -5);
    expect(readAverage("scope-a")?.avg).toBe(100);
  });

  it("reads a corrupted entry as no history", () => {
    globalThis.localStorage.setItem("memql-portal-synapse-tokens-scope-a", "{not json");
    expect(readAverage("scope-a")).toBeNull();
    expect(floatFor("scope-a", 40).firstRun).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// The wired affordance, over a fake stream.
// ---------------------------------------------------------------------------

const SUGGEST_REPLY = {
  patches: [
    { field: "name", value: "Studio Mac" },
    { field: "platform", value: "mac" },
    // Not offered by the scope: it must not reach the form.
    { field: "secret", value: "nope" },
  ],
  note: "",
};

let submitted = 0;

function ExampleForm({ id = "example", label = "The example form" }): ReactNode {
  const [name, setName] = useState("");
  const [platform, setPlatform] = useState("linux");

  const fields: readonly SynapseField[] = [
    { name: "name", type: "text", label: "Name", value: name },
    { name: "platform", type: "enum", label: "Platform", value: platform, options: ["mac", "linux"] },
  ];

  function apply(patches: readonly SynapsePatch[]): void {
    for (const patch of patches) {
      if (patch.field === "name" && typeof patch.value === "string") setName(patch.value);
      if (patch.field === "platform" && typeof patch.value === "string") setPlatform(patch.value);
    }
  }

  return (
    <SynapseSection id={id} label={label} fields={fields} apply={apply}>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          submitted += 1;
        }}
      >
        <label>
          {label} name
          <input value={name} onChange={(event) => setName(event.target.value)} />
        </label>
        <output data-testid={`${id}-platform`}>{platform}</output>
        <button type="submit">Save</button>
      </form>
    </SynapseSection>
  );
}

// The fake takes its message so the test can assert what was SENT -- the
// domain and the payload are the contract with the engine, and a fake with no
// parameter cannot see either.
function renderSynapse(
  children: ReactNode,
  sendAndWait = vi.fn(async (_message: unknown) => ({
    aiSuggestResult: { domain: "uiAssist", result: SUGGEST_REPLY },
  })),
) {
  const dial = vi.fn(
    async () =>
      ({
        nodeId: "bff-test",
        serverVersion: "0.0.0-test",
        query: asQueryClient({
          listConcepts: vi.fn(async () => []),
          getMyAccess: vi.fn(async () => ({ userId: "u", clusterRole: "owner", primaryEmail: "a@b.test" })),
        }),
        dispatcher: { sendAndWait },
        close: vi.fn(),
        done: vi.fn(() => new Promise<void>(() => {})),
      }) as unknown as Connection,
  ) as unknown as typeof Connection.dial;

  render(
    <MemoryRouter>
      <AuthProvider
        config={{ identityUrl: "", identityApiBaseUrl: "", oauthClientId: "", authEnabled: false, domain: "" }}
        fetchImpl={async () => {
          throw new Error("no identity calls");
        }}
        storage={null}
        navigate={() => {}}
        redirectUri="https://api.example.test/auth/callback"
      >
        <ClusterProvider dial={dial}>
          <SynapseProvider>
            {children}
            <SynapseButton />
          </SynapseProvider>
        </ClusterProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
  return { sendAndWait };
}

function synapseButton(): HTMLElement {
  return screen.getByRole("button", { name: /^Synapse/ });
}

async function openPopover(): Promise<void> {
  await waitFor(() => expect(synapseButton()).toBeTruthy());
  fireEvent.pointerDown(synapseButton());
  fireEvent.pointerUp(synapseButton());
  await waitFor(() => expect(screen.getByRole("dialog", { name: "Synapse" })).toBeTruthy());
}

describe("the Synapse affordance", () => {
  beforeEach(() => {
    submitted = 0;
    globalThis.localStorage?.clear();
  });
  afterEach(() => vi.useRealTimers());

  it("names the section it will act on, in words", async () => {
    renderSynapse(<ExampleForm />);
    await openPopover();
    expect(screen.getByText("The example form")).toBeTruthy();
  });

  it("acts on the section you last touched", async () => {
    renderSynapse(
      <>
        <ExampleForm id="first" label="The first form" />
        <ExampleForm id="second" label="The second form" />
      </>,
    );
    await openPopover();
    // Registration order decides the default, so a page with one form needs
    // no gesture at all.
    expect(screen.getByText("The first form")).toBeTruthy();

    fireEvent.pointerEnter(document.querySelector('[data-synapse-scope="second"]')!);
    await waitFor(() => expect(screen.getByText("The second form")).toBeTruthy());
    // ...and the section itself says so, where the eyes already are.
    expect(
      document.querySelector('[data-synapse-scope="second"]')?.getAttribute("data-synapse-active"),
    ).toBe("true");
    expect(
      document.querySelector('[data-synapse-scope="first"]')?.getAttribute("data-synapse-active"),
    ).toBe("false");
  });

  it("fills the form and NEVER submits it", async () => {
    renderSynapse(<ExampleForm />);
    await openPopover();

    fireEvent.change(screen.getByLabelText(/The example form name/), {
      target: { value: "" },
    });
    fireEvent.change(screen.getByLabelText("What to fill in"), {
      target: { value: "call it the studio mac, it is a mac" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Fill" }));

    await waitFor(() =>
      expect((screen.getByLabelText(/The example form name/) as HTMLInputElement).value).toBe(
        "Studio Mac",
      ),
    );
    expect(screen.getByTestId("example-platform").textContent).toBe("mac");

    // THE HARD RULE. The reply filled two fields and the form was not sent --
    // a person reads it and presses Save themselves.
    expect(submitted).toBe(0);
  });

  it("does not let a patch the scope never offered reach the form", async () => {
    renderSynapse(<ExampleForm />);
    await openPopover();
    fireEvent.change(screen.getByLabelText("What to fill in"), { target: { value: "fill it" } });
    fireEvent.click(screen.getByRole("button", { name: "Fill" }));
    await waitFor(() => expect(screen.getByText(/2 fields filled/)).toBeTruthy());
    // The third patch named `secret`, which this scope does not declare.
    expect(screen.queryByText("nope")).toBeNull();
  });

  it("sends the scope and the person's own words, under the uiAssist domain", async () => {
    const { sendAndWait } = renderSynapse(<ExampleForm />);
    await openPopover();
    fireEvent.change(screen.getByLabelText("What to fill in"), { target: { value: "make it a mac" } });
    fireEvent.click(screen.getByRole("button", { name: "Fill" }));

    await waitFor(() => expect(sendAndWait).toHaveBeenCalled());
    const sent = sendAndWait.mock.calls[0]?.[0] as {
      aiSuggest?: Record<string, unknown>;
    };
    expect(sent.aiSuggest?.["domain"]).toBe("uiAssist");
    const payload = sent.aiSuggest?.["payload"] as { prompt: string; scope: { fields: unknown[] } };
    expect(payload.prompt).toBe("make it a mac");
    expect(payload.scope.fields).toHaveLength(2);
  });

  it("reports the cost of the fire, in the popover as well as on the button", async () => {
    renderSynapse(<ExampleForm />);
    await openPopover();
    fireEvent.change(screen.getByLabelText("What to fill in"), { target: { value: "fill it" } });
    fireEvent.click(screen.getByRole("button", { name: "Fill" }));

    // The FLOAT is aria-hidden; the popover's status line carries the same
    // fact in a sentence, which is what a screen reader gets.
    await waitFor(() => expect(screen.getByText("first run")).toBeTruthy());
    expect(screen.getByText("first run").getAttribute("aria-hidden")).toBe("true");
  });

  // The two defects the visual QA pass found (memql#4660). Both were
  // invisible to every assertion above, because both are about what happens
  // BETWEEN two interactions -- and both produced a wrong answer on screen
  // rather than an error anywhere.
  it("holds the active scope while you type into it", async () => {
    renderSynapse(<ExampleForm />);
    await openPopover();
    const scope = () => document.querySelector('[data-synapse-scope="example"]');
    expect(scope()?.getAttribute("data-synapse-active")).toBe("true");

    // `fields` carries each field's CURRENT value, so it is a new array per
    // keystroke. Registering that object directly re-ran the effect per
    // character, and its cleanup unregisters -- so the ring blinked off and
    // the active scope was momentarily nobody's.
    fireEvent.change(screen.getByLabelText(/The example form name/), {
      target: { value: "typed by hand" },
    });
    await waitFor(() =>
      expect((screen.getByLabelText(/The example form name/) as HTMLInputElement).value).toBe(
        "typed by hand",
      ),
    );
    expect(scope()?.getAttribute("data-synapse-active")).toBe("true");
    // ...and the popover still names it, rather than falling back to "nothing
    // here can be filled in".
    expect(screen.getByText("The example form")).toBeTruthy();
  });

  it("sends the values that are on screen NOW, not the ones at registration", async () => {
    // The other half of the same fix: the scope object is stable, so `fields`
    // has to be a live read rather than a snapshot -- otherwise "make it
    // shorter" would be told the field is empty.
    const { sendAndWait } = renderSynapse(<ExampleForm />);
    await openPopover();
    fireEvent.change(screen.getByLabelText(/The example form name/), {
      target: { value: "the long original name" },
    });
    fireEvent.change(screen.getByLabelText("What to fill in"), { target: { value: "shorten it" } });
    fireEvent.click(screen.getByRole("button", { name: "Fill" }));

    await waitFor(() => expect(sendAndWait).toHaveBeenCalled());
    const sent = sendAndWait.mock.calls[0]?.[0] as { aiSuggest?: Record<string, unknown> };
    const payload = sent.aiSuggest?.["payload"] as {
      scope: { fields: { name: string; value: string }[] };
    };
    expect(payload.scope.fields.find((f) => f.name === "name")?.value).toBe(
      "the long original name",
    );
  });

  it("does not carry one section's outcome to another", async () => {
    // Fill a form, move to a different scope, and the popover still read
    // "2 fields filled." -- a report about a section no longer in view. The
    // PROMPT is deliberately kept: the scope also changes on a hover between
    // two sections of one page, and clearing what somebody typed for that
    // would be worse than the staleness.
    renderSynapse(
      <>
        <ExampleForm id="first" label="The first form" />
        <ExampleForm id="second" label="The second form" />
      </>,
    );
    await openPopover();
    fireEvent.change(screen.getByLabelText("What to fill in"), { target: { value: "fill it" } });
    fireEvent.click(screen.getByRole("button", { name: "Fill" }));
    await waitFor(() => expect(screen.getByText(/2 fields filled/)).toBeTruthy());

    fireEvent.pointerEnter(document.querySelector('[data-synapse-scope="second"]')!);
    await waitFor(() => expect(screen.getByText("The second form")).toBeTruthy());
    expect(screen.queryByText(/fields filled/)).toBeNull();
  });

  it("keeps what you typed when the scope changes under you", async () => {
    // The other side of the same decision. A successful fill clears the
    // prompt (the words have been spent), but merely MOVING between two
    // sections of one page must not -- the pointer does that on its own, and
    // losing a sentence to a hover would be maddening.
    renderSynapse(
      <>
        <ExampleForm id="first" label="The first form" />
        <ExampleForm id="second" label="The second form" />
      </>,
    );
    await openPopover();
    fireEvent.change(screen.getByLabelText("What to fill in"), {
      target: { value: "a sentence I am still writing" },
    });
    fireEvent.pointerEnter(document.querySelector('[data-synapse-scope="second"]')!);
    await waitFor(() => expect(screen.getByText("The second form")).toBeTruthy());
    expect((screen.getByLabelText("What to fill in") as HTMLInputElement).value).toBe(
      "a sentence I am still writing",
    );
  });

  it("says so, and fills nothing, on a page with no fillable section", async () => {
    renderSynapse(<p>Just a page.</p>);
    await openPopover();
    expect(screen.getByText(/Nothing on this page can be filled in/)).toBeTruthy();
    // The button never pretends: no target, no Fill.
    expect((screen.getByRole("button", { name: "Fill" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("keeps the form intact when the call fails", async () => {
    renderSynapse(
      <ExampleForm />,
      vi.fn(async (_message: unknown) => {
        throw new Error("no provider configured");
      }),
    );
    await openPopover();
    fireEvent.change(screen.getByLabelText("What to fill in"), { target: { value: "fill it" } });
    fireEvent.click(screen.getByRole("button", { name: "Fill" }));

    await waitFor(() => expect(screen.getByText(/That did not run/)).toBeTruthy());
    // The sentence, not the raw string -- a status line follows the same rule
    // every other error render does.
    expect(screen.queryByText(/no provider configured/)).toBeNull();
    expect((screen.getByLabelText(/The example form name/) as HTMLInputElement).value).toBe("");
    expect(submitted).toBe(0);
  });
});
