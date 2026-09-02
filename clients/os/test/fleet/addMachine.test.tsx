import { act, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const h = vi.hoisted(() => ({
  connection: null as unknown,
  mint: vi.fn(),
}));

vi.mock("../../src/live/connection", () => ({
  useOsConnection: () => h.connection,
  bridgePathFor: (base: string) => base + "_memql/ws",
  osBridgePath: "/_memql/ws",
}));

// The mint is the one call this panel makes that is NOT a graph read: it
// rides the connection's dispatcher through the SDK's identity surface.
vi.mock("@znasllc-io/memql-sdk-core/identity", () => ({
  createWorkerToken: (...args: unknown[]) => h.mint(...args),
}));

const { AddMachine } = await import("../../src/apps/fleet/addMachine/AddMachine");
const { installCommand, workerClusterUrl, INSTALL_PLATFORMS } = await import(
  "../../src/apps/fleet/addMachine/install"
);
const { fakeConnection, withSession } = await import("./harness");

// ASSEMBLED FROM PARTS, deliberately. The repo's secret scanner matches
// `mql_<kind>_<43 base64url chars>` as one literal, and a test fixture that
// happens to reach that length would red the gitleaks lane on a file that
// contains no secret. Joining at runtime means no line here can ever match,
// whatever the fixture is later edited to say.
const TOKEN = ["mql", "wkr", "notARealTokenOnlyATestFixture"].join("_");

async function click(el: Element) {
  await act(async () => {
    (el as HTMLElement).click();
  });
}

async function type(el: HTMLInputElement, value: string) {
  await act(async () => {
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
    setter.call(el, value);
    el.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

function mount(machineCount: number, onClose = vi.fn()) {
  h.connection = fakeConnection();
  const view = render(withSession(<AddMachine machineCount={machineCount} onClose={onClose} />));
  return { view, onClose };
}

async function mintFor(name: string) {
  await type(screen.getByLabelText("What is this machine called") as HTMLInputElement, name);
  await click(screen.getByRole("button", { name: "Mint a token" }));
}

beforeEach(() => {
  h.connection = null;
  h.mint.mockReset();
  h.mint.mockResolvedValue({
    success: true,
    plainToken: TOKEN,
    identityId: "v1:identity:identity:1",
    ownerUserId: "v1:identity:user:me",
    errorCode: "",
    errorMessage: "",
  });
  globalThis.localStorage.clear();
  globalThis.sessionStorage.clear();
});

describe("adding a machine", () => {
  it("mints once and shows the token with the one-time warning", async () => {
    mount(0);
    await mintFor("studio-mac-mini");

    expect(h.mint).toHaveBeenCalledTimes(1);
    expect(h.mint.mock.calls[0]?.[1]).toEqual({ name: "studio-mac-mini" });
    expect(await screen.findByText(TOKEN)).toBeTruthy();
    expect(screen.getByText(/It is not shown again/)).toBeTruthy();
    expect(screen.getByText(/nowhere to look it up/)).toBeTruthy();
  });

  it("NEVER writes the token to browser storage or a URL", async () => {
    mount(0);
    await mintFor("studio-mac-mini");
    await screen.findByText(TOKEN);

    // This credential does not expire in fifteen minutes and lets a machine
    // act as its owner's worker. The OS persists a great deal to
    // localStorage, which is exactly why this has to be asserted rather than
    // assumed.
    const dump = [
      JSON.stringify(globalThis.localStorage),
      JSON.stringify(globalThis.sessionStorage),
      globalThis.location?.href ?? "",
      document.cookie,
    ].join("|");
    expect(dump).not.toContain(TOKEN);
    expect(dump).not.toContain("mql_wkr_");
  });

  it("composes the documented one-liner against this cluster's api host", async () => {
    mount(0);
    await mintFor("studio-mac-mini");
    const command = (await screen.findByText(/curl -fsSL/)).textContent ?? "";

    expect(command).toContain("--token " + TOKEN);
    // api.<domain>, with the scheme: sdk/go/worker.ParseClusterURL treats a
    // bare host:port as PLAINTEXT whatever the port.
    expect(command).toContain("--cluster https://api.memql.example.com");
    expect(command).toContain("install-mac.sh");
    expect(command).not.toContain("--computeruse");
  });

  it("adds --computeruse only when the build was asked for", async () => {
    mount(0);
    await click(screen.getByLabelText(/Install the computer-use build/));
    await mintFor("studio-mac-mini");
    expect((await screen.findByText(/curl -fsSL/)).textContent).toContain("--computeruse");
  });

  it("reports success by the population GROWING, not by matching the name", async () => {
    // The token's name is what the operator typed; the registration's is the
    // cockpit's hostname. They are routinely different, so a name match would
    // report failure on a success.
    const { view } = mount(2);
    await mintFor("studio-mac-mini");
    expect(await screen.findByText(/Waiting for the machine to connect/)).toBeTruthy();

    view.rerender(withSession(<AddMachine machineCount={3} onClose={vi.fn()} />));
    expect(await screen.findByText(/A new machine has registered/)).toBeTruthy();
  });

  it("does not report success when the population merely changes without growing", async () => {
    const { view } = mount(2);
    await mintFor("studio-mac-mini");
    // A machine revoked elsewhere shrinks the list; that is not this machine
    // arriving.
    view.rerender(withSession(<AddMachine machineCount={1} onClose={vi.fn()} />));
    expect(screen.getByText(/Waiting for the machine to connect/)).toBeTruthy();
  });

  it("gates closing behind an explicit acknowledgment once a token exists", async () => {
    const { onClose } = mount(0);
    await mintFor("studio-mac-mini");
    await screen.findByText(TOKEN);

    const done = screen.getByRole("button", { name: "Done" }) as HTMLButtonElement;
    expect(done.disabled).toBe(true);
    await click(done);
    expect(onClose).not.toHaveBeenCalled();

    await click(screen.getByLabelText(/I have copied the token/));
    await click(screen.getByRole("button", { name: "Done" }));
    expect(onClose).toHaveBeenCalled();
  });

  it("renders a refused mint in surface and shows no token", async () => {
    h.mint.mockResolvedValue({
      success: false,
      plainToken: "",
      identityId: "",
      ownerUserId: "",
      errorCode: "forbidden",
      errorMessage: "this account may not mint worker tokens",
    });
    mount(0);
    await mintFor("studio-mac-mini");

    await waitFor(() =>
      expect(screen.getByText("this account may not mint worker tokens")).toBeTruthy(),
    );
    expect(screen.getByText("The token was not minted.")).toBeTruthy();
    expect(screen.queryByText(/It is not shown again/)).toBeNull();
  });

  it("says something even when the cluster refuses without a reason", async () => {
    h.mint.mockResolvedValue({
      success: false,
      plainToken: "",
      identityId: "",
      ownerUserId: "",
      errorCode: "",
      errorMessage: "",
    });
    mount(0);
    await mintFor("studio-mac-mini");
    expect(
      await screen.findByText(/refused the mint and said nothing about why/),
    ).toBeTruthy();
  });
});

describe("the install command", () => {
  it("renders a placeholder rather than half a URL when no domain is published", () => {
    expect(workerClusterUrl("")).toBe("");
    const command = installCommand({
      platform: "linux",
      clusterUrl: "",
      token: "tok",
      computerUse: false,
    });
    // Obviously not a URL, so a copied command fails loudly at the shell
    // rather than dialling a host nobody meant.
    expect(command).toContain("<your cluster URL>");
  });

  it("strips a scheme and trailing slashes off the configured domain", () => {
    expect(workerClusterUrl("https://memql.example.com/")).toBe("https://api.memql.example.com");
  });

  it("is ONE physical line, whatever the inputs", () => {
    // The multi-line-with-trailing-backslashes form was split by terminal
    // paste handling, on this very panel: `bash -s --` ran with no arguments
    // and `--token mql_wkr_...` executed as its own failing command, with the
    // worker token in shell history either way (memql#4875). What is pinned
    // is the ABSENCE of the two characters a terminal can mis-handle -- any
    // newline or backslash reintroduces the split. Every combination is swept
    // because the computer-use branch was exactly where the old shape changed
    // its line structure.
    for (const platform of INSTALL_PLATFORMS) {
      for (const computerUse of [false, true]) {
        for (const clusterUrl of ["https://api.example.com", ""]) {
          const command = installCommand({ platform, clusterUrl, token: TOKEN, computerUse });
          expect(command).not.toContain("\n");
          expect(command).not.toContain("\\");
        }
      }
    }
  });

  it("pins the exact composed shape", () => {
    // Word for word, so a re-ordering of flags or a doubled space fails HERE
    // rather than on an operator's machine. The runbook (memql#4874) and the
    // portal composer (memql#4873) print this same single line; if this
    // assertion has to change, they change with it.
    const command = installCommand({
      platform: "mac",
      clusterUrl: "https://api.example.com",
      token: TOKEN,
      computerUse: true,
    });
    expect(command).toBe(
      "curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install/install-mac.sh" +
        ` | bash -s -- --token ${TOKEN} --cluster https://api.example.com --computeruse`,
    );
  });
});
